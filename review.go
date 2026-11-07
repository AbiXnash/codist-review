// Package review reviews a commit: code checks on every changed line, then the model on the
// changed files, with each model claim checked against the code before it is shown.
package review

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/AbiXnash/codist-git"
	"github.com/AbiXnash/codist-java"
	"github.com/AbiXnash/codist-model"
	"github.com/AbiXnash/codist-review/store"
)

// Reviewer reviews commits of one repository.
type Reviewer struct {
	Repo     *git.Repo
	Store    *store.Store
	Model    *model.Client // nil means rules only
	MaxFiles int
	Callers  bool // show the model the code in other files that calls the changed methods
	Learn    bool // show the model findings this team marked wrong or useful, as examples
	// Contracts shows the model the signature and javadoc of the methods the changed code calls in other files.
	Contracts bool
	Rules     string // the team's own rules for the model, from .codist/rules.md

	Ignored func(path string) bool
}

// Options says how much of a review to do.
type Options struct {
	RulesOnly bool   // skip the model, for catching up on older commits
	Why       string // why the model was skipped, shown with the review
}

// Review reviews one commit and stores the result. Code check findings are saved first, so they
// show while the model is still working.
func (r *Reviewer) Review(ctx context.Context, sha string, opt Options) error {
	c, err := r.Repo.Get(sha) // always store under the full id, whatever form it was given in
	if err != nil {
		return err
	}
	sha = c.SHA
	started := time.Now()
	modelName := ""
	if r.Model != nil && !opt.RulesOnly {
		modelName = r.Model.Name
	}
	_ = r.Store.SetReview(store.Review{SHA: sha, Status: "running", Model: modelName, Started: started})
	fail := func(err error) error {
		_ = r.Store.SetReview(store.Review{SHA: sha, Status: "failed", Model: modelName, Note: err.Error(), Started: started, Finished: time.Now()})
		return err
	}
	changes, err := r.Repo.Changes(sha)
	if err != nil {
		return fail(err)
	}
	for i := range changes {
		changes[i].Added = withDeletions(changes[i].Added, changes[i].Patch)
	}
	type target struct {
		change git.FileChange
		file   *java.File
	}
	var targets []target
	var findings []store.Finding
	for _, c := range changes {
		if c.Status == "deleted" || !strings.HasSuffix(c.Path, ".java") || (r.Ignored != nil && r.Ignored(c.Path)) {
			continue
		}
		src, ok := r.Repo.FileAt(sha, c.Path)
		if !ok {
			continue
		}
		f := java.Parse(c.Path, src)
		changed := map[int]bool{}
		for _, l := range c.Added {
			changed[l] = true
		}
		for _, is := range java.Check(f, changed) {
			findings = append(findings, finding(sha, c.Path, f, is.Line, is.End, is.Severity, "rule", is.Rule, is.Title, is.Detail, is.Suggestion, ""))
		}
		targets = append(targets, target{c, f})
	}
	stats := Stats{PromptVersion: PromptVersion, Model: modelName}
	findings = r.hideWrong(findings, &stats)
	if err := r.Store.ReplaceFindings(sha, findings); err != nil {
		return fail(err)
	}

	var notes []string
	if opt.Why != "" {
		notes = append(notes, opt.Why)
	}
	if r.Model != nil && !opt.RulesOnly {
		commit, _ := r.Repo.Get(sha)
		var related map[string]string
		learned := ""
		if r.Learn {
			learned = r.feedbackExamples()
		}
		related = map[string]string{}
		if r.Callers || r.Contracts {
			var changed []git.FileChange
			for _, t := range targets {
				changed = append(changed, t.change)
			}
			cd := r.codeAt(sha)
			if r.Callers {
				for p, v := range r.callerContext(cd, changed) {
					related[p] += v
				}
			}
			if r.Contracts {
				for p, v := range r.contractContext(cd, changed) {
					related[p] += v
				}
			}
		}
		learned += rulesSection(r.Rules)
		reviewed := 0
		for _, t := range targets {
			if ctx.Err() != nil {
				return fail(ctx.Err())
			}
			if strings.Contains(t.change.Path, "/src/test/") || len(t.change.Added) == 0 {
				continue
			}
			if reviewed >= r.MaxFiles {
				notes = append(notes, fmt.Sprintf("The model reviewed the first %d changed files; checks covered all of them.", r.MaxFiles))
				break
			}
			reviewed++
			got, dropped, err := r.modelReview(ctx, commit, t.change, t.file, findings, related[t.change.Path]+learned, &stats)
			if err != nil {
				notes = append(notes, "The model could not review "+t.change.Path+": "+err.Error())
				continue
			}
			if dropped > 0 {
				slog.Info("dropped model findings", "sha", sha[:7], "path", t.change.Path, "count", dropped)
			}
			findings = append(findings, r.hideWrong(got, &stats)...)
			_ = r.Store.ReplaceFindings(sha, findings) // show progress file by file
		}
	}
	r.resolve(sha, changes)
	if stats.Hidden > 0 {
		notes = append(notes, fmt.Sprintf("%d finding%s not shown because %s marked wrong before.", stats.Hidden, plural(stats.Hidden), map[bool]string{true: "it was", false: "they were"}[stats.Hidden == 1]))
	}
	if modelName != "" {
		if raw, err := json.Marshal(stats); err == nil {
			_ = r.Store.SetStats(sha, raw)
		}
	}
	return r.Store.SetReview(store.Review{SHA: sha, Status: "done", Model: modelName, Note: strings.Join(notes, " "), Started: started, Finished: time.Now()})
}

// finding builds a stored finding with the key that identifies the same issue in later commits:
// the rule or title, the file, and the flagged line with spacing removed, so moving code up or
// down keeps the same issue.
func finding(sha, path string, f *java.File, line, end int, severity, source, rule, title, detail, suggestion, doubt string) store.Finding {
	snippet := ""
	if line >= 1 && line <= len(f.Lines) {
		snippet = strings.TrimSpace(f.Lines[line-1])
	}
	what := rule
	if what == "" {
		what = strings.ToLower(title)
	}
	h := sha1.Sum([]byte(what + "\x00" + path + "\x00" + squash(snippet)))
	return store.Finding{SHA: sha, Path: path, Line: line, EndLine: max(end, line), Severity: severity, Source: source, Rule: rule,
		Title: title, Detail: detail, Suggestion: suggestion, Snippet: snippet, Doubt: doubt, Key: hex.EncodeToString(h[:8])}
}

var space = regexp.MustCompile(`\s+`)

func squash(s string) string { return space.ReplaceAllString(s, "") }

// resolve closes open issues in the files a commit changed. A code check issue is fixed when the
// check, run on the whole new file, no longer reports it. A model issue is fixed when the line it
// flagged is gone, since the model cannot be asked again for every commit.
func (r *Reviewer) resolve(sha string, changes []git.FileChange) {
	for _, c := range changes {
		open := r.Store.OpenIssues(c.Path)
		if len(open) == 0 {
			continue
		}
		var text string
		still := map[string]bool{}
		if c.Status != "deleted" {
			src, ok := r.Repo.FileAt(sha, c.Path)
			if !ok {
				continue
			}
			text = squash(string(src))
			f := java.Parse(c.Path, src)
			for _, is := range java.Check(f, nil) {
				still[finding(sha, c.Path, f, is.Line, is.End, is.Severity, "rule", is.Rule, is.Title, "", "", "").Key] = true
			}
		}
		var fixed []string
		for _, o := range open {
			gone := o.Rule != "" && !still[o.Key] || o.Rule == "" && squash(o.Snippet) != "" && !strings.Contains(text, squash(o.Snippet))
			if gone {
				fixed = append(fixed, o.Key)
			}
		}
		if len(fixed) > 0 {
			_ = r.Store.Resolve(fixed, sha)
		}
	}
}

// BaselineSHA names the first-run scan of the whole code, which is stored like a review.
const BaselineSHA = "baseline"

// Baseline checks every file in full and records what it finds as open issues, each credited to
// the commit that last changed its line. It returns how many it found.
func (r *Reviewer) Baseline(files map[string][]byte) (int, error) {
	started := time.Now()
	var findings []store.Finding
	for path, src := range files {
		if r.Ignored != nil && r.Ignored(path) {
			continue
		}
		f := java.Parse(path, src)
		for _, is := range java.Check(f, nil) {
			fd := finding(BaselineSHA, path, f, is.Line, is.End, is.Severity, "rule", is.Rule, is.Title, is.Detail, is.Suggestion, "")
			if c, ok := r.Repo.LastChange(path, is.Line); ok {
				fd.FirstSHA = c.SHA
			}
			findings = append(findings, fd)
		}
	}
	if err := r.Store.SetReview(store.Review{SHA: BaselineSHA, Status: "running", Started: started}); err != nil {
		return 0, err
	}
	if err := r.Store.ReplaceFindings(BaselineSHA, findings); err != nil {
		return 0, err
	}
	return len(findings), r.Store.SetReview(store.Review{SHA: BaselineSHA, Status: "done", Note: "Checked the whole code on first run.", Started: started, Finished: time.Now()})
}

func plural(n int) string {
	if n == 1 {
		return " was"
	}
	return "s were"
}

// withDeletions adds to a file's added lines the lines on either side of each place where lines were only deleted,
// so a commit that removes a check (an owner filter, a null test) is still reviewed there.
func withDeletions(added []int, patch string) []int {
	seen := map[int]bool{}
	for _, l := range added {
		seen[l] = true
	}
	out := append([]int(nil), added...)
	mark := func(l int) {
		if l >= 1 && !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	n, deleting := 0, false
	for _, l := range strings.Split(patch, "\n") {
		if deleting && !strings.HasPrefix(l, "-") { // a run of deleted lines ends: pure deletion unless lines replace it
			if !strings.HasPrefix(l, "+") {
				mark(n - 1)
				mark(n)
			}
			deleting = false
		}
		switch {
		case strings.HasPrefix(l, "@@"):
			if m := newStart.FindStringSubmatch(l); m != nil {
				n, _ = strconv.Atoi(m[1])
			}
		case n == 0: // file headers before the first hunk
		case strings.HasPrefix(l, "-"):
			deleting = true
		case strings.HasPrefix(l, "\\"):
		default: // added or unchanged
			n++
		}
	}
	slices.Sort(out)
	return out
}

var newStart = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)`)
