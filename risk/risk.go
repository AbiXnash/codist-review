// Package risk learns, from one repository's own history, which commits tend to bring bugs, and scores each commit
// by it. It is a small logistic regression trained on the machine in seconds; nothing leaves it.
//
// Labels come from the history itself (the SZZ method): for each commit whose message says it fixes something, the
// lines it removed or changed are traced with git blame to the commits that wrote them, and those count as having
// brought a bug. Commits where Codist found issues count too. The model is tested on the newest quarter of the
// history after training on the older three quarters, and only used when it ranks better than chance by a clear
// margin; otherwise it says why, and Codist keeps its newest-first order.
package risk

import (
	"log/slog"
	"math"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	git "github.com/abxengg/codist-git"
)

// Names are the features, in order. Counts are taken as log(1+n).
var Names = []string{
	"lines added", "lines deleted", "files changed", "share of Java files", "changes tests",
	"earlier changes to these files", "earlier fixes in these files", "is itself a fix",
	"author's earlier commits", "spread across files",
}

var fixWords = regexp.MustCompile(`(?i)\b(fix(e[sd])?|bugs?|issue|errors?|crash(es)?|npe|nullpointer\w*|regression|hotfix|broken|wrong)\b`)

// IsFix says whether a commit message says it fixes something.
func IsFix(subject string) bool { return fixWords.MatchString(subject) }

// Model is a trained risk model and what it found.
type Model struct {
	Ready     bool               `json:"ready"`
	Reason    string             `json:"reason,omitempty"` // why it is not used, in words a person can act on
	AUC       float64            `json:"auc"`              // on the newest quarter, held out from training: 0.5 is chance
	Commits   int                `json:"commits"`          // used to learn from
	Positives int                `json:"positives"`        // of them, known to have brought a bug
	Signals   []string           `json:"signals"`          // the features that weigh most, with their direction
	Weights   []float64          `json:"weights"`          // per feature, then the bias
	Mean      []float64          `json:"mean"`
	Std       []float64          `json:"std"`
	Scores    map[string]float64 `json:"-"` // every commit in the history, 0 to 1
	cutHigh   float64
	cutMedium float64
}

// Level is a commit's risk as a word: high for the riskiest fifth of this repository's commits, medium for the
// next three tenths, low for the rest; "" when the model is not ready or the commit was not scored.
func (m *Model) Level(sha string) string {
	if m == nil || !m.Ready {
		return ""
	}
	s, ok := m.Scores[sha]
	switch {
	case !ok:
		return ""
	case s >= m.cutHigh:
		return "high"
	case s >= m.cutMedium:
		return "medium"
	}
	return "low"
}

// Options bound the work.
type Options struct {
	Commits int             // how much history to learn from; default 2000
	Blames  int             // git blame calls for labels; default 400
	Known   map[string]bool // commits known to have brought an issue, such as from Codist's own findings
}

// Train learns from the repository's history. It never fails: a model that cannot be used says why.
func Train(repo *git.Repo, opt Options) *Model {
	if opt.Commits == 0 {
		opt.Commits = 2000
	}
	if opt.Blames == 0 {
		opt.Blames = 400
	}
	hist, err := repo.History(opt.Commits)
	if err != nil {
		return &Model{Reason: "Could not read the history: " + err.Error(), Scores: map[string]float64{}}
	}
	labels := Labels(repo, hist, opt.Blames)
	for sha := range opt.Known {
		labels[sha] = true
	}
	code, err := repo.CodeChurn(opt.Commits) // nil if it fails: then all changed lines count
	if err != nil {
		slog.Warn("code churn not read; counting all changed lines", "lib", "risk", "err", err)
	}
	feats := Features(hist, code)
	var x [][]float64
	var y []float64
	var shas []string
	for i, c := range hist {
		if c.Parents > 1 || len(c.Files) == 0 || massChange(feats[i]) {
			continue // merges, empty commits and mass changes (reformatting, renames) say nothing about one change
		}
		x = append(x, feats[i])
		y = append(y, b2f(labels[c.SHA]))
		shas = append(shas, c.SHA)
	}
	m := &Model{Commits: len(x), Scores: map[string]float64{}}
	for _, v := range y {
		m.Positives += int(v)
	}
	cut := len(x) * 3 / 4
	trainPos, testPos := count(y[:cut]), count(y[cut:])
	switch {
	case len(x) < 40:
		m.Reason = "Too little history to learn from (" + strconv.Itoa(len(x)) + " commits; 40 needed)."
		return m
	case trainPos < 8 || testPos < 3:
		m.Reason = "Too few commits known to have brought a bug (" + strconv.Itoa(m.Positives) + "); the model learns from fix commits and from issues Codist finds, so it gets better with time."
		return m
	}
	// Learn on the older three quarters, test on the newest quarter.
	mean, std := standardise(x[:cut])
	w := fit(scale(x[:cut], mean, std), y[:cut])
	m.AUC = auc(predictAll(w, scale(x[cut:], mean, std)), y[cut:])
	if m.AUC < 0.6 {
		m.Reason = "The history does not tell risky commits apart well enough here (AUC " + strconv.FormatFloat(m.AUC, 'f', 2, 64) + " on the newest commits; 0.60 needed), so commits are read newest first."
		return m
	}
	// Good enough: learn from all of it, and score every commit.
	m.Mean, m.Std = standardise(x)
	m.Weights = fit(scale(x, m.Mean, m.Std), y)
	scores := predictAll(m.Weights, scale(x, m.Mean, m.Std))
	for i, sha := range shas {
		m.Scores[sha] = scores[i]
	}
	sorted := append([]float64(nil), scores...)
	sort.Float64s(sorted)
	m.cutHigh = sorted[len(sorted)*8/10]
	m.cutMedium = sorted[len(sorted)/2]
	m.Signals = signals(m.Weights)
	m.Ready = true
	return m
}

// Labels finds the commits that brought a bug, from the fix commits after them, within hist.
func Labels(repo *git.Repo, hist []git.CommitStat, blames int) map[string]bool {
	in := map[string]bool{}
	for _, c := range hist {
		in[c.SHA] = true
	}
	out := map[string]bool{}
	for i := len(hist) - 1; i >= 0 && blames > 0; i-- { // newest fixes first, while the budget lasts
		c := hist[i]
		if c.Parents != 1 || !IsFix(c.Subject) {
			continue
		}
		changes, err := repo.Changes(c.SHA)
		if err != nil {
			slog.Debug("fix commit not read", "lib", "risk", "sha", c.SHA, "err", err)
			continue
		}
		for _, ch := range changes {
			if ch.Status != "modified" || !strings.HasSuffix(ch.Path, ".java") || isTest(ch.Path) {
				continue
			}
			before, ok := repo.FileAt(c.SHA+"~1", ch.Path)
			if !ok {
				continue
			}
			code := git.CodeLines(before)
			var lines []int
			for _, l := range removed(ch.Patch) {
				if l >= 1 && l <= len(code) && code[l-1] { // code only: a fix that also tidies a comment does not blame it
					lines = append(lines, l)
				}
			}
			for _, r := range ranges(lines) {
				if blames == 0 {
					break
				}
				blames--
				for _, sha := range repo.BlameAt(c.SHA+"~1", ch.Path, r[0], r[1]) {
					if sha != c.SHA && in[sha] {
						out[sha] = true
					}
				}
			}
		}
	}
	return out
}

var hunk = regexp.MustCompile(`^@@ -(\d+)`)

// removed lists the old-side line numbers a patch removes or replaces.
func removed(patch string) []int {
	var out []int
	o := 0
	for _, l := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(l, "@@"):
			if m := hunk.FindStringSubmatch(l); m != nil {
				o, _ = strconv.Atoi(m[1])
			}
		case strings.HasPrefix(l, "---"), strings.HasPrefix(l, "+"), strings.HasPrefix(l, `\`):
		case strings.HasPrefix(l, "-"):
			out = append(out, o)
			o++
		default:
			o++
		}
	}
	return out
}

// ranges joins consecutive lines into from-to pairs.
func ranges(lines []int) [][2]int {
	var out [][2]int
	for _, l := range lines {
		if n := len(out); n > 0 && out[n-1][1] == l-1 {
			out[n-1][1] = l
			continue
		}
		out = append(out, [2]int{l, l})
	}
	return out
}

func isTest(p string) bool {
	b := path.Base(p)
	return strings.Contains("/"+p, "/src/test/") || strings.HasSuffix(b, "Test.java") || strings.HasSuffix(b, "Tests.java")
}

// massLines and massFiles mark a mass change, such as reformatting or a large rename, which is left out of learning.
const massLines, massFiles = 3000, 100

func massChange(f []float64) bool {
	return math.Expm1(f[0])+math.Expm1(f[1]) > massLines || math.Expm1(f[2]) > massFiles
}

// Features describes each commit with what was known when it was made, walking from the oldest. With code (lines
// of Java code per commit and file, from git.CodeChurn), sizes count only code: comments, javadoc, imports, blank
// lines and other files do not make a change look bigger.
func Features(hist []git.CommitStat, code map[string]map[string][2]int) [][]float64 {
	changes, fixes, authors := map[string]int{}, map[string]int{}, map[string]int{}
	out := make([][]float64, len(hist))
	for i, c := range hist {
		added, deleted, java, tests, maxChanges, sumFixes := 0, 0, 0, 0, 0, 0
		var sizes []float64
		files := 0
		for _, f := range c.Files {
			a, d := max(0, f.Added), max(0, f.Deleted)
			if code != nil {
				lines := code[c.SHA][f.Path] // zero when the file changed no code
				a, d = lines[0], lines[1]
			}
			added += a
			deleted += d
			if a+d > 0 {
				files++
			}
			if strings.HasSuffix(f.Path, ".java") {
				java++
			}
			if isTest(f.Path) {
				tests = 1
			}
			maxChanges = max(maxChanges, changes[f.Path])
			sumFixes += fixes[f.Path]
			sizes = append(sizes, float64(a+d))
		}
		share := 0.0
		if len(c.Files) > 0 {
			share = float64(java) / float64(len(c.Files))
		}
		fix := IsFix(c.Subject)
		out[i] = []float64{lg(added), lg(deleted), lg(files), share, float64(tests), lg(maxChanges), lg(sumFixes),
			b2f(fix), lg(authors[c.Author]), entropy(sizes)}
		for _, f := range c.Files {
			changes[f.Path]++
			if fix {
				fixes[f.Path]++
			}
		}
		authors[c.Author]++
	}
	return out
}

// entropy is how evenly a change spreads over its files, from 0 (one file) to 1.
func entropy(sizes []float64) float64 {
	total := 0.0
	for _, s := range sizes {
		total += s
	}
	if len(sizes) < 2 || total == 0 {
		return 0
	}
	h := 0.0
	for _, s := range sizes {
		if s > 0 {
			p := s / total
			h -= p * math.Log2(p)
		}
	}
	return h / math.Log2(float64(len(sizes)))
}

// Score is the risk of a commit described by f, from 0 to 1.
func (m *Model) Score(f []float64) float64 {
	if m == nil || !m.Ready {
		return 0
	}
	return predict(m.Weights, scale([][]float64{f}, m.Mean, m.Std)[0])
}

func standardise(x [][]float64) (mean, std []float64) {
	n, d := float64(len(x)), len(x[0])
	mean, std = make([]float64, d), make([]float64, d)
	for _, r := range x {
		for j, v := range r {
			mean[j] += v / n
		}
	}
	for _, r := range x {
		for j, v := range r {
			std[j] += (v - mean[j]) * (v - mean[j]) / n
		}
	}
	for j := range std {
		std[j] = math.Sqrt(std[j])
		if std[j] < 1e-9 {
			std[j] = 1
		}
	}
	return mean, std
}

func scale(x [][]float64, mean, std []float64) [][]float64 {
	out := make([][]float64, len(x))
	for i, r := range x {
		out[i] = make([]float64, len(r))
		for j, v := range r {
			out[i][j] = (v - mean[j]) / std[j]
		}
	}
	return out
}

// fit is logistic regression by gradient descent, with both classes weighted equally (bug-bringing commits are
// few) and a little L2 so a feature seen in few commits cannot dominate. The last weight is the bias.
func fit(x [][]float64, y []float64) []float64 {
	d := len(x[0])
	w := make([]float64, d+1)
	pos := count(y)
	wPos, wNeg := float64(len(y))/(2*math.Max(pos, 1)), float64(len(y))/(2*math.Max(float64(len(y))-pos, 1))
	const rate, l2, steps = 0.3, 0.01, 600
	for step := 0; step < steps; step++ {
		grad := make([]float64, d+1)
		for i, r := range x {
			p := predict(w, r)
			cw := wNeg
			if y[i] == 1 {
				cw = wPos
			}
			e := cw * (p - y[i])
			for j, v := range r {
				grad[j] += e * v
			}
			grad[d] += e
		}
		for j := range w {
			g := grad[j] / float64(len(x))
			if j < d {
				g += l2 * w[j]
			}
			w[j] -= rate * g
		}
	}
	return w
}

func predict(w, r []float64) float64 {
	z := w[len(w)-1]
	for j, v := range r {
		z += w[j] * v
	}
	return 1 / (1 + math.Exp(-z))
}

func predictAll(w []float64, x [][]float64) []float64 {
	out := make([]float64, len(x))
	for i, r := range x {
		out[i] = predict(w, r)
	}
	return out
}

// auc is the chance that a bug-bringing commit scores above one that is not (ties count half).
func auc(scores, y []float64) float64 {
	var pos, neg []float64
	for i, s := range scores {
		if y[i] == 1 {
			pos = append(pos, s)
		} else {
			neg = append(neg, s)
		}
	}
	if len(pos) == 0 || len(neg) == 0 {
		return 0.5
	}
	wins := 0.0
	for _, p := range pos {
		for _, n := range neg {
			switch {
			case p > n:
				wins++
			case p == n:
				wins += 0.5
			}
		}
	}
	return wins / float64(len(pos)*len(neg))
}

// signals names the three features that weigh most, and which way.
func signals(w []float64) []string {
	idx := make([]int, len(Names))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return math.Abs(w[idx[a]]) > math.Abs(w[idx[b]]) })
	var out []string
	for _, i := range idx[:3] {
		dir := "raises"
		if w[i] < 0 {
			dir = "lowers"
		}
		out = append(out, Names[i]+" "+dir+" risk")
	}
	return out
}

func lg(n int) float64   { return math.Log1p(float64(n)) }
func b2f(b bool) float64 { return map[bool]float64{true: 1}[b] }
func count(y []float64) float64 {
	n := 0.0
	for _, v := range y {
		n += v
	}
	return n
}
