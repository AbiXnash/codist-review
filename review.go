// Package review reviews a commit: code checks on every changed line, then the model on the
// changed files, with each model claim checked against the code before it is shown.
package review

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AbiXnash/codist-git"
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
	// Languages are the languages reviewed; empty means Java only. See Language.
	Languages []Language
	// NoSecondLook turns off the model's second look at each serious finding it made (see secondLook). It is on
	// unless turned off: on the planted-bug test it turned false alarms on clean commits into notes.
	NoSecondLook bool
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
	warnIf(r.Store.SetReview(store.Review{SHA: sha, Status: "running", Model: modelName, Started: started}), "review status not saved", "sha", c.Short)
	fail := func(err error) error {
		logger().Warn("review failed", "sha", c.Short, "took", time.Since(started).Round(time.Millisecond), "err", err)
		warnIf(r.Store.SetReview(store.Review{SHA: sha, Status: "failed", Model: modelName, Note: err.Error(), Started: started, Finished: time.Now()}),
			"review status not saved", "sha", c.Short)
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
		file   Parsed
	}
	var targets []target
	var findings []store.Finding
	for _, c := range changes {
		lang := r.langOf(c.Path)
		if c.Status == "deleted" || lang == nil || (r.Ignored != nil && r.Ignored(c.Path)) {
			continue
		}
		src, ok := r.Repo.FileAt(sha, c.Path)
		if !ok {
			logger().Warn("changed file not readable", "sha", sha[:7], "path", c.Path)
			continue
		}
		f := lang.Parse(c.Path, src)
		changed := map[int]bool{}
		for _, l := range c.Added {
			changed[l] = true
		}
		for _, is := range lang.Check(f, changed, r.facts(lang)) {
			findings = append(findings, finding(sha, c.Path, f, is.Line, is.End, is.Severity, "rule", is.Rule, is.Title, is.Detail, is.Suggestion, ""))
		}
		targets = append(targets, target{c, f})
	}
	stats := Stats{PromptVersion: PromptVersion, Model: modelName}
	logger().Debug("checks done", "sha", c.Short, "changed", len(changes), "reviewed", len(targets), "rule_findings", len(findings))
	findings = r.hideWrong(findings, &stats)
	if err := r.Store.ReplaceFindings(sha, findings); err != nil {
		return fail(err)
	}

	var notes []string
	if opt.Why != "" {
		notes = append(notes, opt.Why)
	}
	if r.Model != nil && !opt.RulesOnly {
		commit := c
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
		system := systemPrompt + rulesSection(r.Rules) + learned // the same for every file, so it goes first
		var triage *Triage
		if r.Learn {
			triage = r.triageFromFeedback()
		}
		reviewed := 0
		for _, t := range targets {
			if ctx.Err() != nil {
				return fail(ctx.Err())
			}
			if l := r.langOf(t.change.Path); l != nil && l.IsTest(t.change.Path) || len(t.change.Added) == 0 {
				continue
			}
			if reviewed >= r.MaxFiles {
				notes = append(notes, fmt.Sprintf("The model reviewed the first %d changed files; checks covered all of them.", r.MaxFiles))
				break
			}
			reviewed++
			fileStart := time.Now()
			got, dropped, err := r.modelReview(ctx, commit, t.change, t.file, findings, system, related[t.change.Path], &stats)
			if err != nil {
				logger().Warn("model could not review file", "sha", c.Short, "path", t.change.Path, "err", err)
				notes = append(notes, "The model could not review "+t.change.Path+": "+err.Error())
				continue
			}
			logger().Debug("model reviewed file", "sha", c.Short, "path", t.change.Path, "kept", len(got), "dropped", dropped,
				"took", time.Since(fileStart).Round(time.Millisecond))
			if dropped > 0 {
				logger().Info("dropped model findings", "sha", c.Short, "path", t.change.Path, "count", dropped)
			}
			if !r.NoSecondLook {
				got = r.secondLook(ctx, commit, t.change, t.file.Lines(), got, &stats)
			}
			for i := range got {
				got[i] = triage.demote(got[i])
			}
			findings = append(findings, r.hideWrong(got, &stats)...)
			warnIf(r.Store.ReplaceFindings(sha, findings), "progress not saved", "sha", c.Short) // show progress file by file
		}
	}
	r.resolve(sha, changes)
	r.settle(sha, findings)
	if stats.Hidden > 0 {
		notes = append(notes, fmt.Sprintf("%d finding%s not shown because %s marked wrong before.", stats.Hidden, plural(stats.Hidden), map[bool]string{true: "it was", false: "they were"}[stats.Hidden == 1]))
	}
	if modelName != "" {
		if raw, err := json.Marshal(stats); err == nil {
			warnIf(r.Store.SetStats(sha, raw), "model usage not saved", "sha", c.Short)
		}
	}
	logger().Debug("review done", "sha", c.Short, "findings", len(findings), "hidden", stats.Hidden, "took", time.Since(started).Round(time.Millisecond))
	return r.Store.SetReview(store.Review{SHA: sha, Status: "done", Model: modelName, Note: strings.Join(notes, " "), Started: started, Finished: time.Now()})
}

// finding builds a stored finding with the key that identifies the same issue in later commits:
// the rule or title, the file, and the flagged line with spacing removed, so moving code up or
// down keeps the same issue.
func finding(sha, path string, f Parsed, line, end int, severity, source, rule, title, detail, suggestion, doubt string) store.Finding {
	snippet := ""
	if lines := f.Lines(); line >= 1 && line <= len(lines) {
		snippet = strings.TrimSpace(lines[line-1])
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

// resolve closes open issues in the files a commit changed.
func (r *Reviewer) resolve(sha string, changes []git.FileChange) {
	for _, c := range changes {
		open := r.Store.OpenIssues(c.Path)
		if len(open) == 0 {
			continue
		}
		var src []byte
		if c.Status != "deleted" {
			var ok bool
			if src, ok = r.Repo.FileAt(sha, c.Path); !ok {
				logger().Warn("file not readable, its issues stay open", "sha", sha[:min(7, len(sha))], "path", c.Path)
				continue
			}
		}
		// Only issues brought before this commit can be fixed by it. Catching up newest first, an older commit is
		// reviewed after newer ones, and the issues they brought are not in its code yet: that is not a fix.
		var fixed []string
		for _, k := range r.gone(sha, c.Path, src, c.Status != "deleted", open) {
			if i := slices.IndexFunc(open, func(o store.OpenIssue) bool { return o.Key == k }); open[i].FirstSHA == BaselineSHA || r.Repo.IsAncestor(open[i].FirstSHA, sha) {
				fixed = append(fixed, k)
			}
		}
		r.close(sha, c.Path, fixed)
	}
}

// settle closes the issues this commit opened that a later commit has already fixed. Codist reads its backlog newest
// first, so the commit that fixed an issue may have been reviewed before the one that brought it, when there was
// nothing yet to close. Only this commit's issues are settled: one opened later may simply not exist yet at an
// earlier commit.
func (r *Reviewer) settle(sha string, findings []store.Finding) {
	mine := map[string]map[string]bool{} // path -> keys of the issues this commit opened there
	for _, f := range findings {
		if f.SHA == sha && f.Key != "" {
			if mine[f.Path] == nil {
				mine[f.Path] = map[string]bool{}
			}
			mine[f.Path][f.Key] = true
		}
	}
	for path, keys := range mine {
		var open []store.OpenIssue
		for _, o := range r.Store.OpenIssues(path) {
			if keys[o.Key] {
				open = append(open, o)
			}
		}
		for _, later := range r.Repo.Later(sha, path) {
			if len(open) == 0 {
				break
			}
			src, present := r.Repo.FileAt(later, path) // gone when that commit deleted or moved the file
			fixed := r.gone(later, path, src, present, open)
			r.close(later, path, fixed)
			open = slices.DeleteFunc(open, func(o store.OpenIssue) bool { return slices.Contains(fixed, o.Key) })
		}
	}
}

// gone lists which open issues of a file are no longer there in src, the file at sha (present false when it is not
// there at all). A check issue is gone when the check, run on the whole file, no longer reports it. A model issue is
// gone when the line it flagged is gone, since the model cannot be asked again for every commit.
func (r *Reviewer) gone(sha, path string, src []byte, present bool, open []store.OpenIssue) []string {
	var text string
	still := map[string]bool{}
	if present {
		text = squash(string(src))
		lang := r.langOf(path)
		if lang == nil {
			return nil
		}
		f := lang.Parse(path, src)
		for _, is := range lang.Check(f, nil, r.facts(lang)) {
			still[finding(sha, path, f, is.Line, is.End, is.Severity, "rule", is.Rule, is.Title, "", "", "").Key] = true
		}
	}
	var fixed []string
	for _, o := range open {
		if o.Rule != "" && !still[o.Key] || o.Rule == "" && squash(o.Snippet) != "" && !strings.Contains(text, squash(o.Snippet)) {
			fixed = append(fixed, o.Key)
		}
	}
	return fixed
}

// close marks issues fixed by the commit sha.
func (r *Reviewer) close(sha, path string, fixed []string) {
	if len(fixed) == 0 {
		return
	}
	logger().Debug("issues fixed", "sha", sha[:min(7, len(sha))], "path", path, "count", len(fixed))
	warnIf(r.Store.Resolve(fixed, sha), "fixed issues not saved", "sha", sha[:min(7, len(sha))], "path", path)
}

// BaselineSHA names the first-run scan of the whole code, which is stored like a review.
const BaselineSHA = "baseline"

// Baseline checks every file in full and records what it finds as open issues, each credited to
// the commit that last changed its line. It returns how many it found.
func (r *Reviewer) Baseline(files map[string][]byte) (int, error) {
	started := time.Now()
	var findings []store.Finding
	type file struct {
		path   string
		lang   Language
		parsed Parsed
	}
	var all []file
	byLang := map[Language][]Parsed{}
	for path, src := range files {
		if lang := r.langOf(path); lang != nil && (r.Ignored == nil || !r.Ignored(path)) {
			p := lang.Parse(path, src)
			all = append(all, file{path, lang, p})
			if !lang.IsTest(path) {
				byLang[lang] = append(byLang[lang], p)
			}
		}
	}
	facts := map[Language]any{}
	for lang, ps := range byLang {
		facts[lang] = lang.Facts(ps)
	}
	for _, x := range all {
		path, f := x.path, x.parsed
		for _, is := range x.lang.Check(f, nil, facts[x.lang]) {
			fd := finding(BaselineSHA, path, f, is.Line, is.End, is.Severity, "rule", is.Rule, is.Title, is.Detail, is.Suggestion, "")
			if c, ok := r.Repo.LastChange(path, is.Line); ok {
				fd.FirstSHA = c.SHA
			}
			findings = append(findings, fd)
		}
	}
	logger().Debug("baseline checked", "files", len(files), "findings", len(findings), "took", time.Since(started).Round(time.Millisecond))
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

var factsCache sync.Map // repository root and language -> cachedFacts

type cachedFacts struct {
	head  string
	facts any
}

// facts is what a language's checks know about the whole project, such as which methods return an Optional. It is
// read from HEAD once per HEAD: the newest code is the best guess at the project's methods for any commit.
func (r *Reviewer) facts(lang Language) any {
	head := r.Repo.Head()
	key := r.Repo.Root + "\x00" + lang.Name()
	if c, ok := factsCache.Load(key); ok && c.(cachedFacts).head == head {
		return c.(cachedFacts).facts
	}
	files, err := r.Repo.Files(head)
	if err != nil {
		logger().Warn("project facts not read; checks see one file at a time", "lang", lang.Name(), "err", err)
		return nil
	}
	var paths []string
	for _, p := range files {
		if lang.Owns(p) && !lang.IsTest(p) && (r.Ignored == nil || !r.Ignored(p)) {
			paths = append(paths, p)
		}
	}
	var parsed []Parsed
	for p, src := range r.Repo.ReadAll(head, paths) {
		parsed = append(parsed, lang.Parse(p, src))
	}
	facts := lang.Facts(parsed)
	factsCache.Store(key, cachedFacts{head, facts})
	logger().Debug("project facts", "lang", lang.Name(), "head", head[:min(7, len(head))], "files", len(parsed))
	return facts
}
