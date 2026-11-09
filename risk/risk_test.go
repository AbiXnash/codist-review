package risk

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	git "github.com/abxengg/codist-git"
)

type repo struct {
	t   *testing.T
	dir string
}

func newRepo(t *testing.T) *repo {
	r := &repo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	return r
}

func (r *repo) git(args ...string) string {
	r.t.Helper()
	out, err := exec.Command("git", append([]string{"-C", r.dir, "-c", "user.name=Dev", "-c", "user.email=d@x", "-c", "commit.gpgsign=false"}, args...)...).CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %s", args, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *repo) commit(msg string, files map[string]string) string {
	r.t.Helper()
	for p, s := range files {
		full := filepath.Join(r.dir, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(s), 0o644)
	}
	r.git("add", "-A")
	r.git("commit", "-q", "-m", msg)
	return r.git("rev-parse", "HEAD")
}

func (r *repo) open() *git.Repo {
	g, err := git.Open(r.dir)
	if err != nil {
		r.t.Fatal(err)
	}
	return g
}

func lines(n int, bad string) string {
	var b strings.Builder
	b.WriteString("class A {\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "  int f%d() { return %d; }\n", i, i)
	}
	if bad != "" {
		b.WriteString("  " + bad + "\n")
	}
	b.WriteString("}\n")
	return b.String()
}

// A big change that a later fix corrects is labelled as having brought the bug.
func TestLabels(t *testing.T) {
	r := newRepo(t)
	r.commit("Add A", map[string]string{"src/main/java/A.java": lines(2, "")})
	bad := r.commit("Add totals", map[string]string{"src/main/java/A.java": lines(2, "int total() { return 1 - 2; }")})
	r.commit("Docs", map[string]string{"README.md": "x"})
	r.commit("Fix wrong total", map[string]string{"src/main/java/A.java": lines(2, "int total() { return 1 + 2; }")})
	g := r.open()
	hist, _ := g.History(10)
	got := Labels(g, hist, 10)
	if len(got) != 1 || !got[bad] {
		t.Errorf("labels %v, want only %s", got, bad[:7])
	}
	if len(Labels(g, hist, 0)) != 0 {
		t.Error("no blame budget, no labels")
	}
}

func TestNotReady(t *testing.T) {
	r := newRepo(t)
	for i := 0; i < 5; i++ {
		r.commit(fmt.Sprint("Change ", i), map[string]string{"src/main/java/A.java": lines(i+1, "")})
	}
	m := Train(r.open(), Options{})
	if m.Ready || !strings.Contains(m.Reason, "Too little history") || m.Level("x") != "" || m.Score([]float64{}) != 0 {
		t.Errorf("%+v", m)
	}
	r2 := newRepo(t)
	for i := 0; i < 45; i++ {
		r2.commit(fmt.Sprint("Change ", i), map[string]string{fmt.Sprintf("src/main/java/F%d.java", i%5): lines(i%7+1, "")})
	}
	if m := Train(r2.open(), Options{}); m.Ready || !strings.Contains(m.Reason, "Too few commits known to have brought a bug") {
		t.Errorf("%+v", m)
	}
	if m := Train(&git.Repo{Root: t.TempDir()}, Options{}); m.Ready || !strings.Contains(m.Reason, "Could not read") {
		t.Errorf("%+v", m)
	}
	var nilModel *Model
	if nilModel.Level("x") != "" || nilModel.Score(nil) != 0 {
		t.Error("nil model")
	}
}

// In a history where big changes are the ones later fixed, the model must learn that and rank them high.
func TestLearnsThePattern(t *testing.T) {
	r := newRepo(t)
	var risky []string
	for i := 0; i < 64; i++ {
		file := fmt.Sprintf("src/main/java/F%d.java", i%8)
		if i%4 == 0 {
			sha := r.commit(fmt.Sprint("Add feature ", i), map[string]string{file: lines(30+i%5, fmt.Sprintf("int bug%d() { return -1; }", i))})
			risky = append(risky, sha)
			r.commit(fmt.Sprint("Fix bug ", i), map[string]string{file: lines(30+i%5, fmt.Sprintf("int bug%d() { return 1; }", i))})
		} else {
			r.commit(fmt.Sprint("Tidy ", i), map[string]string{fmt.Sprintf("src/main/java/T%d.java", i): lines(1, "")})
		}
	}
	// A large javadoc-only commit: many lines, no code. It must not look risky.
	var doc strings.Builder
	doc.WriteString("/**\n")
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&doc, " * Explains part %d of the class in more words.\n", i)
	}
	doc.WriteString(" */\nclass Doc {}\n")
	docs := r.commit("Improve javadoc", map[string]string{"src/main/java/Doc.java": doc.String()})
	m := Train(r.open(), Options{})
	if !m.Ready || m.AUC < 0.6 || m.Positives < 12 || len(m.Signals) != 3 {
		t.Fatalf("%+v", m)
	}
	if m.Level(docs) == "high" {
		t.Errorf("a javadoc-only commit is ranked high (score %.2f)", m.Scores[docs])
	}
	high := 0
	for _, sha := range risky {
		if m.Level(sha) == "high" || m.Level(sha) == "medium" {
			high++
		}
	}
	if high < len(risky)*3/4 {
		t.Errorf("only %d of %d bug-bringing commits ranked medium or high", high, len(risky))
	}
	if m.Level("nope") != "" || m.Score(make([]float64, len(Names))) <= 0 {
		t.Error("unknown commit, or a score")
	}
}

func TestHelpers(t *testing.T) {
	if got := removed("@@ -3,4 +3,2 @@\n a\n-b();\n-// c\n-d();\n+e();\n\\ No newline\n"); fmt.Sprint(got) != "[4 5 6]" {
		t.Errorf("removed %v", got)
	}
	if got := ranges([]int{1, 2, 3, 7, 9, 10}); fmt.Sprint(got) != "[[1 3] [7 7] [9 10]]" {
		t.Errorf("ranges %v", got)
	}
	if entropy([]float64{5}) != 0 || entropy([]float64{1, 1}) != 1 || entropy([]float64{0, 0}) != 0 {
		t.Error("entropy")
	}
	if auc([]float64{0.9, 0.1}, []float64{1, 0}) != 1 || auc([]float64{0.5, 0.5}, []float64{1, 0}) != 0.5 || auc([]float64{1}, []float64{1}) != 0.5 {
		t.Error("auc")
	}
	if !IsFix("Fix NPE in parser") || !IsFix("crash on empty input") || IsFix("Add search") || !isTest("a/src/test/java/X.java") || !isTest("XTest.java") {
		t.Error("words")
	}
}
