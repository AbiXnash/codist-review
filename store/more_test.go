package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReviewsAndFirstSHA(t *testing.T) {
	s, _ := Open(t.TempDir(), "repo")
	defer s.Close()
	at := time.UnixMilli(1_700_000_000_000)
	s.SetReview(Review{SHA: "a", Status: "done", Model: "m", Note: "n", Started: at, Finished: at})
	s.SetReview(Review{SHA: "b", Status: "running", Started: at})
	s.SetReview(Review{SHA: "a", Status: "failed", Started: at}) // replaces
	rv := s.Reviews()
	if len(rv) != 2 || rv["a"].Status != "failed" || !rv["b"].Started.Equal(at) || !rv["b"].Finished.IsZero() {
		t.Errorf("%+v", rv)
	}
	if _, ok := s.GetReview("zzz"); ok {
		t.Error("unknown review found")
	}
	// A finding from the first-run scan starts its issue at the commit that wrote the line.
	s.SetReview(Review{SHA: "baseline", Status: "done"})
	s.ReplaceFindings("baseline", []Finding{{Path: "A.java", Line: 1, Severity: "medium", Source: "rule", Rule: "r", Title: "T", Key: "k", FirstSHA: "c1"}})
	if all := s.AllIssues(); len(all) != 1 || all[0].FirstSHA != "c1" || all[0].Commits[0] != "baseline" {
		t.Errorf("%+v", all)
	}
}

func TestIncidentEdits(t *testing.T) {
	s, _ := Open(t.TempDir(), "repo")
	defer s.Close()
	id, _ := s.AddIncident(Incident{Title: "Old", Logs: "x"})
	if in, _ := s.Incident(id); string(in.Analysis) != "{}" {
		t.Errorf("an incident starts with an empty analysis, got %s", in.Analysis)
	}
	if err := s.SetIncidentTitle(id, "New"); err != nil {
		t.Fatal(err)
	}
	if in, _ := s.Incident(id); in.Title != "New" {
		t.Errorf("title %q", in.Title)
	}
	if err := s.DeleteIncident(id); err != nil || len(s.Incidents()) != 0 {
		t.Errorf("delete: %v", err)
	}
	if err := s.DeleteIncident(id); err == nil {
		t.Error("deleting a missing incident should fail")
	}
	if _, err := s.Incident(id); err == nil {
		t.Error("a deleted incident is gone")
	}
}

// Every read answers empty and every write fails once the database is gone, rather than panicking.
func TestClosed(t *testing.T) {
	s, _ := Open(t.TempDir(), "repo")
	s.Close()
	if s.SetReview(Review{SHA: "a"}) == nil || s.ReplaceFindings("a", nil) == nil || s.Resolve([]string{"k"}, "a") == nil ||
		s.SetIssueState("k", "open") == nil || s.DeleteIncident(1) == nil || s.SetIncidentTitle(1, "t") == nil ||
		s.SetAnalysis(1, nil) == nil {
		t.Error("a write succeeded on a closed database")
	}
	if _, err := s.AddIncident(Incident{}); err == nil {
		t.Error("add succeeded")
	}
	if _, err := s.Incident(1); err == nil {
		t.Error("read succeeded")
	}
	if len(s.Reviews()) != 0 || s.Findings("a") != nil || len(s.Counts()) != 0 || len(s.OpenIssues("p")) != 0 ||
		len(s.OpenByFile()) != 0 || s.Incidents() != nil || s.OpenIssueList() != nil || s.AllIssues() != nil {
		t.Error("a read returned data from a closed database")
	}
	if _, ok := s.GetReview("a"); ok {
		t.Error("review found")
	}
}

func TestOpenErrors(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, nil, 0o600)
	if _, err := Open(filepath.Join(file, "sub"), "r"); err == nil {
		t.Error("a folder under a file cannot be made")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "bad.db"), []byte("this is not a database, just text that is long enough to have a header"), 0o600)
	if _, err := Open(dir, "bad"); err == nil {
		t.Error("a file that is not SQLite should fail")
	}
}

func TestReplaceFindingsIsAllOrNothing(t *testing.T) {
	s, _ := Open(t.TempDir(), "repo")
	defer s.Close()
	s.SetReview(Review{SHA: "a", Status: "done"})
	if err := s.ReplaceFindings("a", []Finding{{Key: "k"}}); err != nil {
		t.Fatal(err)
	}
	s.db.Exec(`CREATE TRIGGER no_findings BEFORE INSERT ON findings BEGIN SELECT RAISE(FAIL, 'blocked'); END`)
	if err := s.ReplaceFindings("a", []Finding{{Key: "k"}}); err == nil {
		t.Error("a failing insert should fail the whole replace")
	}
	s.db.Exec(`DROP TRIGGER no_findings`)
	s.db.Exec(`CREATE TRIGGER no_issues BEFORE INSERT ON issues BEGIN SELECT RAISE(FAIL, 'blocked'); END`)
	if err := s.ReplaceFindings("a", []Finding{{Key: "k2"}}); err == nil {
		t.Error("a failing issue insert should fail the whole replace")
	}
	if len(s.Findings("a")) != 1 {
		t.Error("a failed replace changed the findings")
	}
}

func TestBehaviour(t *testing.T) {
	s, _ := Open(t.TempDir(), "repo")
	defer s.Close()
	if _, ok := s.Behaviour("a"); ok || len(s.BehaviourDone()) != 0 {
		t.Error("nothing stored yet")
	}
	s.SetBehaviour("a", json.RawMessage(`{"status":"skipped"}`))
	s.SetBehaviour("a", json.RawMessage(`{"status":"done"}`))
	if r, ok := s.Behaviour("a"); !ok || string(r) != `{"status":"done"}` || !s.BehaviourDone()["a"] {
		t.Errorf("%s %v", r, ok)
	}
	s.Close()
	if s.SetBehaviour("b", nil) == nil || len(s.BehaviourDone()) != 0 {
		t.Error("closed store")
	}
}

func TestCacheStatsFeedback(t *testing.T) {
	s, _ := Open(t.TempDir(), "repo")
	defer s.Close()
	if _, ok := s.CachedReply("k"); ok {
		t.Error("empty cache")
	}
	s.SetCachedReply("k", json.RawMessage(`{"a":1}`))
	if r, ok := s.CachedReply("k"); !ok || string(r) != `{"a":1}` {
		t.Error("cache")
	}
	s.SetStats("x", json.RawMessage(`{"calls":2}`))
	if r, ok := s.Stats("x"); !ok || string(r) != `{"calls":2}` || len(s.AllStats()) != 1 {
		t.Error("stats")
	}
	if _, ok := s.Stats("y"); ok {
		t.Error("no stats")
	}
	s.SetFeedback("i", "wrong", "noise")
	if f := s.AllFeedback()["i"]; f.Verdict != "wrong" || f.Note != "noise" || f.Updated.IsZero() {
		t.Errorf("%+v", f)
	}
	s.SetFeedback("i", "", "")
	if len(s.AllFeedback()) != 0 {
		t.Error("feedback removed")
	}
	s.Close()
	if len(s.AllStats()) != 0 || len(s.AllFeedback()) != 0 || s.SetStats("x", nil) == nil || s.SetCachedReply("k", nil) == nil {
		t.Error("closed store")
	}
}

func TestOpenRepoNamesByPathAndMovesOldData(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(t.TempDir(), "shop"), filepath.Join(t.TempDir(), "shop")
	if Name(a) == Name(b) || !strings.HasPrefix(Name(a), "shop-") {
		t.Fatalf("same folder name, different places: %s %s", Name(a), Name(b))
	}
	old, err := Open(dir, "shop") // what an older Codist made
	if err != nil {
		t.Fatal(err)
	}
	if err := old.SetReview(Review{SHA: "abc", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	old.Close()
	s, err := OpenRepo(dir, a)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, ok := s.GetReview("abc"); !ok {
		t.Fatal("the old database was not carried over")
	}
	if _, err := os.Stat(filepath.Join(dir, "shop.db")); !os.IsNotExist(err) {
		t.Fatal("the old file should have moved")
	}
}
