package review

import (
	"fmt"
	"regexp"
	"strings"

	git "github.com/abxengg/codist-git"
	"github.com/abxengg/codist-review/store"
)

// Some model claims can be settled exactly, without asking the model again: what a line compares, or whether the
// thing a claim calls missing is in another file of the same commit. A claim the facts contradict is kept as a note
// with the reason, never dropped, so a person can still see it. These run before the second look, which a small
// model can also get wrong (it confirmed "strings compared with ==" on two int key codes).

var (
	// "strings compared with ==", "compares the string by reference", "use equals instead of =="
	claimStringEq = regexp.MustCompile(`(?i)(strings?|text)\b[^.]{0,60}(==|!=|identity|reference|same object)|` +
		`compar\w*\s+(the\s+)?strings?|(==|!=)[^.]{0,40}strings?|equals\(?\)?\s+(method\s+)?instead`)
	// "not implemented", "is missing", "never called", "no code that handles"
	claimMissing = regexp.MustCompile(`(?i)\b(not\s+(yet\s+)?(implemented|handled|wired|called|used|hooked|invoked)|` +
		`(is|are)\s+missing|missing\s+(the\s+)?(call|handling|implementation|code|logic|key)|never\s+(called|used|invoked)|` +
		`no\s+(code|call|handling)\b)`)
	backticked = regexp.MustCompile("`([A-Za-z_][A-Za-z0-9_]*)(?:\\(\\))?`")
	declared   = regexp.MustCompile(`^\s*(?:@\w+\s+)*(?:(?:public|protected|private|static|final|synchronized|abstract|default|native)\s+)*` +
		`(?:<[^>]+>\s+)?[\w.<>\[\], ?]+\s+(\w+)\s*\(`)
)

// contradict checks the model's findings on one file against exact facts, and turns the contradicted ones into notes.
func (r *Reviewer) contradict(sha string, ch git.FileChange, f Parsed, others []git.FileChange, got []store.Finding) []store.Finding {
	for i, fd := range got {
		if fd.Doubt != "" {
			continue
		}
		why := r.stringEqWrong(ch.Path, f, fd)
		if why == "" {
			why = doneElsewhere(f, others, fd)
		}
		if why == "" {
			continue
		}
		logger().Debug("model claim contradicted", "sha", sha[:min(7, len(sha))], "path", ch.Path, "line", fd.Line, "title", fd.Title, "why", why)
		got[i].Doubt = why
		got[i].Severity = "low"
	}
	return got
}

// stringEqWrong refutes "strings compared with ==" when the language's own type-aware check finds no string
// comparison by == or != on the lines the claim names.
func (r *Reviewer) stringEqWrong(path string, f Parsed, fd store.Finding) string {
	if !claimStringEq.MatchString(fd.Title + " " + fd.Detail) {
		return ""
	}
	lang := r.langOf(path)
	if lang == nil || lang.Name() != "java" {
		return ""
	}
	lines := f.Lines()
	on := map[int]bool{}
	for l := fd.Line; l <= max(fd.Line, fd.EndLine); l++ {
		if l >= 1 && l <= len(lines) && strings.Contains(lines[l-1], "==") || l >= 1 && l <= len(lines) && strings.Contains(lines[l-1], "!=") {
			on[l] = true
		}
	}
	if len(on) == 0 {
		return "" // the claim is about another line; nothing to check here
	}
	for _, is := range lang.Check(f, on, nil) {
		if is.Rule == "string-identity" {
			return ""
		}
	}
	return "Codist checked the types on this line: nothing here compares strings with == or !=."
}

// doneElsewhere refutes "this is not implemented / never called" when another file of the same commit uses the
// method the claim is about.
func doneElsewhere(f Parsed, others []git.FileChange, fd store.Finding) string {
	if len(others) == 0 || !claimMissing.MatchString(fd.Title+" "+fd.Detail) {
		return ""
	}
	names := map[string]bool{}
	if n := memberAt(f, fd.Line); n != "" {
		names[n] = true
	}
	for _, m := range backticked.FindAllStringSubmatch(fd.Title+" "+fd.Detail, -1) {
		if len(m[1]) > 2 {
			names[m[1]] = true
		}
	}
	for _, o := range others {
		for _, l := range strings.Split(o.Patch, "\n") {
			if !strings.HasPrefix(l, "+") || strings.HasPrefix(l, "+++") {
				continue
			}
			for n := range names {
				if regexp.MustCompile(`\b` + regexp.QuoteMeta(n) + `\b`).MatchString(l) {
					return fmt.Sprintf("This commit does it in another file: %s uses %s (`%s`).", o.Path, n, strings.TrimSpace(l[1:]))
				}
			}
		}
	}
	return ""
}

// memberAt is the name of the method or constructor holding line, from its declaration.
func memberAt(f Parsed, line int) string {
	lines := f.Lines()
	for _, u := range f.Units() {
		for _, m := range u.Members {
			if line < m[0] || line > m[1] {
				continue
			}
			for l := m[0]; l <= m[1] && l <= len(lines); l++ {
				if hit := declared.FindStringSubmatch(lines[l-1]); hit != nil {
					return hit[1]
				}
			}
		}
	}
	return ""
}

// otherChanges is the rest of a commit's changed files, which the model sees in brief so it does not call missing
// what another file of the same commit adds.
func otherChanges(changes []git.FileChange, self string) []git.FileChange {
	var out []git.FileChange
	for _, c := range changes {
		if c.Path != self && c.Status != "deleted" {
			out = append(out, c)
		}
	}
	return out
}

const (
	maxOtherPatch = 1500 // characters of each other file's diff
	maxOthers     = 4000 // in all
)

// othersSection is the prompt section with the commit's other changes.
func othersSection(others []git.FileChange) string {
	if len(others) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## Other files this commit changes (for context; report problems only in the file under review)\n")
	for _, o := range others {
		if b.Len() > maxOthers {
			b.WriteString("… (more files)\n")
			break
		}
		fmt.Fprintf(&b, "### %s\n```diff\n%s\n```\n", o.Path, clip(o.Patch, maxOtherPatch))
	}
	return b.String()
}
