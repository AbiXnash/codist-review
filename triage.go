package review

import (
	"hash/fnv"
	"math"
	"regexp"
	"slices"
	"strings"

	"github.com/AbiXnash/codist-review/store"
)

// Triage learns, from the findings this team marked useful or wrong, which new model findings look like the wrong
// ones, and shows those as notes instead of problems. It never hides a finding: a demoted one stays on the page, low,
// with the reason. It is a small logistic regression over the finding's severity, whether Codist already doubted it,
// and the words of its title and detail, trained in milliseconds whenever the reviewer is made.

const (
	triageBuckets   = 48  // hashed word features
	triageMinLabels = 20  // judged model findings needed before it is used
	triageMinEach   = 5   // of each kind
	triageCut       = 0.3 // below this chance of being useful, a finding is shown as a note
)

// TriageExample is one judged finding.
type TriageExample struct {
	Finding store.Finding
	Useful  bool
}

// Triage scores model findings by how much they look like the ones this team found useful.
type Triage struct {
	w []float64
}

var (
	triageWord = regexp.MustCompile(`[a-z][a-z]+`)
	triageStop = map[string]bool{"the": true, "and": true, "for": true, "this": true, "that": true, "with": true, "can": true,
		"not": true, "will": true, "when": true, "may": true, "are": true, "its": true, "from": true, "into": true, "which": true}
)

func triageFeatures(f store.Finding) []float64 {
	x := make([]float64, len(severities)+1+triageBuckets)
	if i := slices.Index(severities, f.Severity); i >= 0 {
		x[i] = 1
	}
	if f.Doubt != "" {
		x[len(severities)] = 1
	}
	for _, w := range triageWord.FindAllString(strings.ToLower(f.Title+" "+f.Detail), -1) {
		if len(w) < 3 || triageStop[w] {
			continue
		}
		h := fnv.New32a()
		h.Write([]byte(w))
		x[len(severities)+1+int(h.Sum32()%triageBuckets)] = 1
	}
	return x
}

// TrainTriage learns from judged model findings, or returns nil when there are too few of either kind to trust.
func TrainTriage(examples []TriageExample) *Triage {
	var x [][]float64
	var y []float64
	useful := 0
	for _, e := range examples {
		if e.Finding.Source != "model" {
			continue
		}
		x = append(x, triageFeatures(e.Finding))
		y = append(y, b2f(e.Useful))
		if e.Useful {
			useful++
		}
	}
	if len(x) < triageMinLabels || useful < triageMinEach || len(x)-useful < triageMinEach {
		return nil
	}
	return &Triage{w: logistic(x, y)}
}

// Useful is the chance a finding is one this team would mark useful.
func (t *Triage) Useful(f store.Finding) float64 {
	x := triageFeatures(f)
	z := t.w[len(t.w)-1]
	for j, v := range x {
		z += t.w[j] * v
	}
	return 1 / (1 + math.Exp(-z))
}

// demote shows a model finding that looks like the ones marked wrong as a low note, with the reason.
func (t *Triage) demote(f store.Finding) store.Finding {
	if t == nil || f.Source != "model" || t.Useful(f) >= triageCut {
		return f
	}
	f.Severity = "low"
	if f.Doubt == "" {
		f.Doubt = "Findings like this one were mostly marked wrong in this repository."
	}
	return f
}

// triageFromFeedback trains on the model findings this repository's team has judged.
func (r *Reviewer) triageFromFeedback() *Triage {
	feedback := r.Store.AllFeedback()
	if len(feedback) < triageMinLabels {
		return nil
	}
	var ex []TriageExample
	for _, is := range r.Store.AllIssues() {
		if fb, ok := feedback[is.Key]; ok && (fb.Verdict == "useful" || fb.Verdict == "wrong") {
			ex = append(ex, TriageExample{Finding: is.Latest, Useful: fb.Verdict == "useful"})
		}
	}
	return TrainTriage(ex)
}

// logistic is logistic regression by gradient descent with both classes weighted equally and a little L2, the same
// way the risk model learns. The last weight is the bias.
func logistic(x [][]float64, y []float64) []float64 {
	d := len(x[0])
	w := make([]float64, d+1)
	pos := 0.0
	for _, v := range y {
		pos += v
	}
	n := float64(len(y))
	wPos, wNeg := n/(2*math.Max(pos, 1)), n/(2*math.Max(n-pos, 1))
	const rate, l2, steps = 0.3, 0.01, 800
	for step := 0; step < steps; step++ {
		grad := make([]float64, d+1)
		for i, r := range x {
			z := w[d]
			for j, v := range r {
				z += w[j] * v
			}
			cw := wNeg
			if y[i] == 1 {
				cw = wPos
			}
			e := cw * (1/(1+math.Exp(-z)) - y[i])
			for j, v := range r {
				grad[j] += e * v
			}
			grad[d] += e
		}
		for j := range w {
			g := grad[j] / n
			if j < d {
				g += l2 * w[j]
			}
			w[j] -= rate * g
		}
	}
	return w
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
