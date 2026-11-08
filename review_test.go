package review

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AbiXnash/codist-git"
	"github.com/AbiXnash/codist-review/store"
)

func TestJudge(t *testing.T) {
	code := strings.Split(`class A {
  String a(Map<String, B> m, String k) {
    B b = m.get(k);
    return b.name();
  }
  String c(C c) {
    return c.name();
  }
  String d(D d) {
    if (d == null) { log(); }
    return d.name();
  }
  int e(List<Item> items) {
    return items.stream()
        .mapToInt(Item::price)
        .sum();
  }
  long f(long id) {
    Order o = repo.findById(id).orElseThrow();
    return o.total();
  }
  void g(String role) {
    if (!HR.equals(role)) { deny(); }
  }
}`, "\n")
	cases := []struct {
		name, severity, title, evidence, patch string
		line                                   int
		drop                                   bool
		want                                   string
		doubted                                bool
	}{
		{"value from a map lookup", "high", "Null dereference", "return b.name();", "", 4, false, "high", false},
		{"value tested for null nearby", "high", "Possible NullPointerException", "return d.name();", "", 11, false, "high", false},
		{"nothing makes it null", "high", "Null dereference on some path", "return c.name();", "", 7, false, "low", true},
		{"the change removed a null check", "high", "Null header not handled", "return c.name();", "-  if (c == null) return;\n", 7, false, "high", false},
		{"only describes the change", "medium", "Improved variable naming", "return c.name();", "", 7, false, "low", true},
		{"an ordinary claim is untouched", "high", "Wrong total for empty orders", "return c.name();", "", 7, false, "high", false},
		{"a quote joined from several lines", "medium", "Sum ignores discounts", "return items.stream().mapToInt(Item::price).sum();", "", 14, false, "medium", false},
		{"no quote at all", "high", "Wrong total", "", "", 7, false, "low", true},
		{"quote not in the file", "high", "Wrong total", "return x.total();", "", 7, true, "", false},
		{"quote far away", "high", "Wrong total", "class A {", "", 16, true, "", false},
		{"vague advice", "medium", "Consider adding validation", "return c.name();", "", 7, true, "", false},
		{"orElseThrow never gives null", "high", "NullPointerException risk", "return o.total();", "", 20, false, "low", true},
		{"only says the message disagrees", "high", "Commit message contradicts code change", "return c.name();", "", 7, false, "low", true},
		{"line outside the file", "high", "Wrong total", "return c.name();", "", 99, true, "", false},
		{"null-safe equals, though a null check was removed", "high", "Null Pointer Exception", "if (!HR.equals(role)) { deny(); }", "-  if (role != null && role != HR) {\n", 23, false, "low", true},
	}
	for _, c := range cases {
		drop, sev, doubt := judge(code, c.patch, c.line, c.severity, c.title, c.evidence)
		if (drop != "") != c.drop || (!c.drop && (sev != c.want || (doubt != "") != c.doubted)) {
			t.Errorf("%s: drop %q, severity %q, doubt %q", c.name, drop, sev, doubt)
		}
	}
}

// A commit adds a swallowed exception; a later commit removes it, which resolves the issue.
func TestReviewAndResolve(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=T", "-c", "user.email=t@t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	path := filepath.Join(dir, "svc", "src", "main", "java", "A.java")
	os.MkdirAll(filepath.Dir(path), 0o755)
	write := func(s string) { os.WriteFile(path, []byte(s), 0o644) }
	run("init", "-q")
	write("class A {\n  void a() {\n    run();\n  }\n}\n")
	run("add", ".")
	run("commit", "-q", "-m", "Add A")
	write("class A {\n  void a() {\n    try {\n      run();\n    } catch (Exception e) {\n    }\n  }\n}\n")
	run("commit", "-q", "-am", "Guard run")
	bad := head(t, dir)

	repo, _ := git.Open(dir)
	st, err := store.Open(t.TempDir(), "r")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := &Reviewer{Repo: repo, Store: st, MaxFiles: 8}
	if err := r.Review(context.Background(), bad[:7], Options{}); err != nil { // a short id is stored under the full one
		t.Fatal(err)
	}
	fs := st.Findings(bad)
	if len(fs) != 1 || fs[0].Rule != "empty-catch" || fs[0].Line != 5 || fs[0].State != "open" {
		t.Fatalf("findings: %+v", fs)
	}
	if rv, _ := st.GetReview(bad); rv.Status != "done" {
		t.Errorf("status %q", rv.Status)
	}
	write("class A {\n  void a() {\n    try {\n      run();\n    } catch (Exception e) {\n      throw new IllegalStateException(e);\n    }\n  }\n}\n")
	run("commit", "-q", "-am", "Rethrow")
	fix := head(t, dir)
	if err := r.Review(context.Background(), fix, Options{}); err != nil {
		t.Fatal(err)
	}
	if got := st.Findings(bad); got[0].State != "resolved" || len(st.Findings(fix)) != 0 {
		t.Errorf("after the fix: %+v, fix findings %+v", got, st.Findings(fix))
	}
}

// Catching up on history, Codist reads the newest commits first, so the fix is reviewed before the bug it fixes. The
// bug's issue must still close, credited to the fix, while an issue no later commit fixed stays open.
func TestNewestFirstStillResolves(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=T", "-c", "user.email=t@t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	a := filepath.Join(dir, "svc", "src", "main", "java", "A.java")
	b := filepath.Join(dir, "svc", "src", "main", "java", "B.java")
	os.MkdirAll(filepath.Dir(a), 0o755)
	run("init", "-q")
	os.WriteFile(a, []byte("class A {\n  void a() {\n    run();\n  }\n}\n"), 0o644)
	run("add", ".")
	run("commit", "-q", "-m", "Add A")
	os.WriteFile(a, []byte("class A {\n  void a() {\n    try {\n      run();\n    } catch (Exception e) {\n    }\n  }\n}\n"), 0o644)
	os.WriteFile(b, []byte("class B {\n  void b() {\n    try {\n      run();\n    } catch (Exception e) {\n    }\n  }\n}\n"), 0o644)
	run("add", ".")
	run("commit", "-q", "-m", "Guard run")
	bad := head(t, dir)
	os.WriteFile(a, []byte("class A {\n  void a() {\n    run();\n  }\n}\n"), 0o644)
	run("commit", "-q", "-am", "Let errors through in A")
	fix := head(t, dir)

	repo, _ := git.Open(dir)
	st, err := store.Open(t.TempDir(), "r")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := &Reviewer{Repo: repo, Store: st, MaxFiles: 8}
	for _, sha := range []string{fix, bad} { // newest first, as the backlog runs
		if err := r.Review(context.Background(), sha, Options{}); err != nil {
			t.Fatal(err)
		}
	}
	state := map[string]string{}
	for _, f := range st.Findings(bad) {
		state[filepath.Base(f.Path)] = f.State
	}
	if state["A.java"] != "resolved" || state["B.java"] != "open" {
		t.Errorf("want A's issue resolved by the later fix and B's still open, got %v", state)
	}
}

func head(t *testing.T, dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestWithDeletions(t *testing.T) {
	patch := "@@ -20,4 +20,3 @@ class A {\n     Order one(long c, long id) {\n         return orders.findById(id)\n-            .filter(o -> o.customerId() == c)\n             .orElseThrow();\n"
	got := withDeletions(nil, patch)
	if len(got) != 2 || got[0] != 21 || got[1] != 22 {
		t.Fatalf("lines around the deletion: %v", got)
	}
	if got := withDeletions([]int{5}, "@@ -5,1 +5,1 @@\n-a\n+b\n"); len(got) != 1 || got[0] != 5 {
		t.Fatalf("a replaced line: %v", got)
	}
}
