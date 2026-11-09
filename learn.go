package review

import (
	"fmt"
	"sort"
	"strings"

	"github.com/abxengg/codist-review/store"
)

// Learning from feedback: the model sees a few of the findings this team marked wrong or useful, as examples of
// what not to report and what is worth reporting here (in-context learning). Nothing is trained; the examples change
// as the team keeps judging.

const maxExamples = 3 // of each kind

// feedbackExamples is a prompt section with the latest judged findings, or "" when there are none.
func (r *Reviewer) feedbackExamples() string {
	feedback := r.Store.AllFeedback()
	if len(feedback) == 0 {
		return ""
	}
	type example struct {
		issue store.TrackedIssue
		fb    store.Feedback
	}
	var wrong, useful []example
	for _, is := range r.Store.AllIssues() {
		fb, ok := feedback[is.Key]
		if !ok {
			continue
		}
		switch fb.Verdict {
		case "wrong":
			wrong = append(wrong, example{is, fb})
		case "useful":
			useful = append(useful, example{is, fb})
		}
	}
	latest := func(xs []example) []example {
		sort.SliceStable(xs, func(i, j int) bool { return xs[i].fb.Updated.After(xs[j].fb.Updated) })
		return xs[:min(maxExamples, len(xs))]
	}
	line := func(e example) string {
		s := fmt.Sprintf("- %q at `%s`", e.issue.Latest.Title, strings.TrimSpace(e.issue.Latest.Snippet))
		if e.fb.Note != "" {
			s += fmt.Sprintf(" (the team said: %s)", e.fb.Note)
		}
		return s + "\n"
	}
	var b strings.Builder
	b.WriteString("\n## How this team judged earlier findings\n")
	if w := latest(wrong); len(w) > 0 {
		b.WriteString("Marked wrong here. Do not report problems like these:\n")
		for _, e := range w {
			b.WriteString(line(e))
		}
	}
	if u := latest(useful); len(u) > 0 {
		b.WriteString("Marked useful here. This team wants problems like these reported:\n")
		for _, e := range u {
			b.WriteString(line(e))
		}
	}
	return b.String()
}
