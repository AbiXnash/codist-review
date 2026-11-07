package review

import (
	"regexp"
	"slices"
	"strings"
)

// A model is checked like a new colleague: it must quote the code it means, the quote must be
// near the line it names, and a claim the code cannot back is shown as unconfirmed rather than
// stated as fact. In our measurements, dropping weak claims cost almost no real catches.

var numbered = regexp.MustCompile(`^\s*(>>)?\s*\d*\s*\|?\s*`)

// normalise strips line numbers and all spacing, so a quote that joins a multi-line statement
// still matches.
func normalise(quote string) string {
	var parts []string
	for _, l := range strings.Split(quote, "\n") {
		parts = append(parts, numbered.ReplaceAllString(l, ""))
	}
	return squash(strings.Join(parts, ""))
}

// quoted says whether the quote appears in the file within reach lines of line.
func quoted(lines []string, quote string, line, reach int) (inFile, near bool) {
	want := normalise(quote)
	if want == "" {
		return false, false
	}
	var whole strings.Builder
	starts := make([]int, len(lines))
	for i, l := range lines {
		starts[i] = whole.Len()
		whole.WriteString(squash(l))
	}
	text := whole.String()
	for from := 0; ; {
		at := strings.Index(text[from:], want)
		if at < 0 {
			return inFile, false
		}
		at += from
		inFile = true
		first := 0 // 1-based line where the match starts
		for i, s := range starts {
			if s <= at {
				first = i + 1
			}
		}
		if first >= line-reach && first <= line+reach {
			return true, true
		}
		from = at + 1
	}
}

var (
	vague       = regexp.MustCompile(`(?i)^(consider|you (may|might) want|it (may|might) be|review the|ensure that|make sure)\b`)
	changeTitle = regexp.MustCompile(`(?i)^(improved|simplified|added|adding|renamed|refactored|updated|changed|moved|replaced|introduced|extracted|explicitly passing|use of)\b`)
	nullClaim   = regexp.MustCompile(`(?i)\bnull|\bnpe\b|nullpointer`)
	receiver    = regexp.MustCompile(`\b([A-Za-z_]\w*)\s*(?:\.\s*[A-Za-z_]\w*|\[)`)
	removedNull = regexp.MustCompile(`(?m)^-[^-].*\bnull\b`)
)

var severities = []string{"critical", "high", "medium", "low", "info"}

// judge decides what happens to one model finding: drop says why it is not shown at all; otherwise
// severity and doubt are what to show.
func judge(lines []string, patch string, line int, severity, title, evidence string) (drop, newSeverity, doubt string) {
	if line < 1 || line > len(lines) {
		return "points at a line that is not in the file", "", ""
	}
	if vague.MatchString(strings.TrimSpace(title)) {
		return "vague advice", "", ""
	}
	inFile, near := quoted(lines, evidence, line, 8)
	switch {
	case strings.TrimSpace(evidence) == "":
		doubt = "The model did not quote the code it means."
	case !inFile:
		return "quotes code that is not in the file", "", ""
	case !near:
		return "quotes code far from the line it names", "", ""
	}
	if doubt == "" {
		switch {
		case changeTitle.MatchString(strings.TrimSpace(title)):
			doubt = "This describes the change and names no problem in it."
		case nullClaim.MatchString(title) && !nullBacked(evidence, lines, line, patch):
			doubt = "Codist could not find where this value becomes null in this code."
		}
	}
	if !slices.Contains(severities, severity) {
		severity = "medium"
	}
	if doubt != "" && slices.Index(severities, severity) < slices.Index(severities, "low") {
		severity = "low"
	}
	return "", severity, doubt
}

// nullBacked says the code gives a value a way to be null: the change removed a null check, or
// nearby code tests the value for null, sets it to null, or takes it from a call that returns null
// on a miss.
func nullBacked(evidence string, lines []string, line int, patch string) bool {
	if removedNull.MatchString(patch) {
		return true
	}
	from, to := max(0, line-31), min(len(lines), line+30)
	near := strings.Join(lines[from:to], "\n")
	for _, m := range receiver.FindAllStringSubmatch(evidence, -1) {
		v := regexp.QuoteMeta(m[1])
		source := `(?m)\b` + v + `\s*[!=]=\s*null\b|\bnull\s*[!=]=\s*` + v + `\b|\b` + v + `\s*=\s*null\b|@Nullable\b[^;\n]*\b` + v +
			`\b|\b` + v + `\s*=[^;\n]*\.(get|find\w*|remove|poll|peek\w*|getProperty|getenv|getParameter|getHeader|getAttribute|lookup)\(`
		if regexp.MustCompile(source).MatchString(near) {
			return true
		}
	}
	return false
}
