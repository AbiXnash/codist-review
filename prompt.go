package review

import (
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/AbiXnash/codist-git"
	"github.com/AbiXnash/codist-java"
	"github.com/AbiXnash/codist-review/store"
)

//go:embed review.md
var systemPrompt string

// schema forces the model to return a quote and a line for every finding.
var schema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"findings": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"line":       map[string]any{"type": "integer"},
					"endLine":    map[string]any{"type": "integer"},
					"severity":   map[string]any{"type": "string", "enum": severities},
					"title":      map[string]any{"type": "string"},
					"evidence":   map[string]any{"type": "string"},
					"detail":     map[string]any{"type": "string"},
					"suggestion": map[string]any{"type": "string"},
				},
				"required": []string{"line", "severity", "title", "evidence", "detail"},
			},
		},
	},
	"required": []string{"findings"},
}

type modelFinding struct {
	Line       int    `json:"line"`
	EndLine    int    `json:"endLine"`
	Severity   string `json:"severity"`
	Title      string `json:"title"`
	Evidence   string `json:"evidence"`
	Detail     string `json:"detail"`
	Suggestion string `json:"suggestion"`
}

// maxWholeFile is the size up to which the model sees the whole file. Longer files are cut to the
// changed methods, because every 1,000 prompt tokens costs about 20 seconds on a laptop and a long
// prompt distracts a small model.
const maxWholeFile = 300

// modelReview asks the model about one file. system is the instructions plus what stays the same for every file (the
// team's rules and judged examples), so the model server can reuse its work on that opening part between calls.
func (r *Reviewer) modelReview(ctx context.Context, c git.Commit, ch git.FileChange, f *java.File, known []store.Finding, system, related string, st *Stats) ([]store.Finding, int, error) {
	var out struct {
		Findings []modelFinding `json:"findings"`
	}
	prompt := userPrompt(c, ch, f, known) + related
	key := cacheKey(r.Model.Name, system+"\x00"+prompt)
	if raw, ok := r.Store.CachedReply(key); ok && json.Unmarshal(raw, &out) == nil {
		st.Cached++
		logger().Debug("model reply from cache", "sha", c.Short, "path", ch.Path)
	} else {
		u, err := r.Model.ChatJSON(ctx, system, prompt, schema, &out)
		if err != nil {
			return nil, 0, err
		}
		st.add(u)
		if raw, err := json.Marshal(out); err == nil {
			warnIf(r.Store.SetCachedReply(key, raw), "model reply not cached", "sha", c.Short, "path", ch.Path)
		}
	}
	var got []store.Finding
	dropped := 0
	for _, m := range out.Findings {
		drop, severity, doubt := judge(f.Lines, ch.Patch, m.Line, strings.ToLower(m.Severity), m.Title, m.Evidence)
		if drop != "" || repeatsRule(known, ch.Path, m.Line) {
			logger().Debug("model finding dropped", "sha", c.Short, "path", ch.Path, "line", m.Line, "title", m.Title, "why", cmp.Or(drop, "a code check already reported it"))
			dropped++
			continue
		}
		got = append(got, finding(c.SHA, ch.Path, f, m.Line, m.EndLine, severity, "model", "", strings.TrimSpace(m.Title), m.Detail, m.Suggestion, doubt))
	}
	return got, dropped, nil
}

// repeatsRule says a code check already reported this line; the check's wording is exact.
func repeatsRule(known []store.Finding, path string, line int) bool {
	for _, k := range known {
		if k.Source == "rule" && k.Path == path && line >= k.Line-1 && line <= k.EndLine+1 {
			return true
		}
	}
	return false
}

func userPrompt(c git.Commit, ch git.FileChange, f *java.File, known []store.Finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Commit\n%s\n%s\n\n", c.Subject, c.Body)
	changed := map[int]bool{}
	for _, l := range ch.Added {
		changed[l] = true
	}
	var already []string
	for _, k := range known {
		if k.Path == ch.Path {
			already = append(already, fmt.Sprintf("- line %d: %s", k.Line, k.Title))
		}
	}
	if len(already) > 0 {
		fmt.Fprintf(&b, "## Already found by code checks (do not repeat)\n%s\n\n", strings.Join(already, "\n"))
	}
	patch := ch.Patch
	if len(patch) > 6000 {
		patch = patch[:6000] + "\n… (diff cut)"
	}
	fmt.Fprintf(&b, "## Diff of %s\n```diff\n%s```\n\n", ch.Path, patch)
	fmt.Fprintf(&b, "## %s after the commit (\">>\" marks changed lines)\n```java\n", ch.Path)
	for _, r := range shownRanges(f, changed) {
		if r[0] > 1 {
			b.WriteString("     …\n")
		}
		for l := r[0]; l <= r[1]; l++ {
			mark := "  "
			if changed[l] {
				mark = ">>"
			}
			fmt.Fprintf(&b, "%s%4d| %s\n", mark, l, f.Lines[l-1])
		}
	}
	b.WriteString("```\n")
	return b.String()
}

// shownRanges picks the lines the model sees: the whole file when it is short; otherwise the
// class header and fields, and each method that contains a changed line, with a little context.
func shownRanges(f *java.File, changed map[int]bool) [][2]int {
	n := len(f.Lines)
	if n <= maxWholeFile {
		return [][2]int{{1, n}}
	}
	var rs [][2]int
	for _, t := range f.Types {
		header := t.Line
		for _, fl := range t.Fields {
			header = max(header, fl.Line)
		}
		rs = append(rs, [2]int{max(1, t.Line-3), min(n, header)})
		for _, m := range t.Methods {
			for l := m.Line; l <= m.End; l++ {
				if changed[l] {
					rs = append(rs, [2]int{max(1, docStart(f.Lines, m.Line)-1), min(n, m.End+1)}) // with its javadoc
					break
				}
			}
		}
	}
	for l := range changed { // changed lines outside any method, such as fields
		rs = append(rs, [2]int{max(1, l-3), min(n, l+3)})
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i][0] < rs[j][0] })
	var merged [][2]int
	for _, r := range rs {
		if len(merged) > 0 && r[0] <= merged[len(merged)-1][1]+1 {
			merged[len(merged)-1][1] = max(merged[len(merged)-1][1], r[1])
			continue
		}
		merged = append(merged, r)
	}
	return merged
}
