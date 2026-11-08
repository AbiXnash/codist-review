package review

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	git "github.com/AbiXnash/codist-git"
	"github.com/AbiXnash/codist-review/store"
)

// todoLang is a made-up second language, to show one plugs in with no change to the review: .todo files, where a
// line saying FIXME is an issue.
type todoLang struct{}

type todoFile struct{ lines []string }

func (todoLang) Name() string             { return "todo" }
func (todoLang) Owns(path string) bool    { return strings.HasSuffix(path, ".todo") }
func (todoLang) IsTest(path string) bool  { return strings.HasPrefix(path, "test/") }
func (todoLang) Facts(files []Parsed) any { return len(files) }
func (todoFile) Units() []Unit            { return nil }
func (f todoFile) Lines() []string        { return f.lines }
func (todoLang) Parse(path string, src []byte) Parsed {
	return todoFile{strings.Split(string(src), "\n")}
}
func (todoLang) Check(p Parsed, lines map[int]bool, facts any) []Issue {
	var out []Issue
	for i, l := range p.Lines() {
		if strings.Contains(l, "FIXME") && (lines == nil || lines[i+1]) {
			out = append(out, Issue{Rule: "fixme", Severity: "low", Title: "A FIXME was left in", Line: i + 1, End: i + 1})
		}
	}
	return out
}

func TestSecondLanguage(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=T", "-c", "user.email=t@t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	run("init", "-q")
	os.WriteFile(filepath.Join(dir, "plan.todo"), []byte("ship it\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "A.java"), []byte("class A {\n  void a() {\n  }\n}\n"), 0o644)
	run("add", ".")
	run("commit", "-q", "-m", "Start")
	os.WriteFile(filepath.Join(dir, "plan.todo"), []byte("ship it\nFIXME: tests\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "A.java"), []byte("class A {\n  void a() {\n    try { run(); } catch (Exception e) { }\n  }\n}\n"), 0o644)
	run("commit", "-q", "-am", "More")
	sha := head(t, dir)

	repo, _ := git.Open(dir)
	st, err := store.Open(t.TempDir(), "r")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := &Reviewer{Repo: repo, Store: st, MaxFiles: 8, Languages: []Language{Java, todoLang{}}}
	if err := r.Review(context.Background(), sha, Options{}); err != nil {
		t.Fatal(err)
	}
	rules := map[string]int{}
	for _, f := range st.Findings(sha) {
		rules[f.Rule]++
	}
	if rules["fixme"] != 1 || rules["empty-catch"] != 1 {
		t.Errorf("both languages reviewed in one commit: %v", rules)
	}
	// Without the second language listed, its files are not read.
	only := &Reviewer{Repo: repo, Store: st, MaxFiles: 8}
	if only.langOf("plan.todo") != nil || only.langOf("A.java") != Java {
		t.Error("the default is Java only")
	}
}
