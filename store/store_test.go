package store

import (
	"encoding/json"
	"testing"
	"time"
)

func TestReviewsFindingsAndIssues(t *testing.T) {
	s, err := Open(t.TempDir(), "repo")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	if err := s.SetReview(Review{SHA: "a", Status: "done", Started: now, Finished: now}); err != nil {
		t.Fatal(err)
	}
	fs := []Finding{
		{Path: "A.java", Line: 3, EndLine: 3, Severity: "low", Source: "rule", Title: "Low one", Key: "k1", Snippet: "x();"},
		{Path: "A.java", Line: 9, EndLine: 9, Severity: "high", Source: "model", Title: "High one", Key: "k2"},
	}
	if err := s.ReplaceFindings("a", fs); err != nil {
		t.Fatal(err)
	}
	got := s.Findings("a")
	if len(got) != 2 || got[0].Title != "High one" || got[0].State != "open" {
		t.Fatalf("findings: %+v", got)
	}
	if s.OpenByFile()["A.java"] != 2 || len(s.OpenIssues("A.java")) != 2 {
		t.Error("issues not recorded")
	}
	if list := s.OpenIssueList(); len(list) != 2 || list[0].Title != "High one" || list[0].Line != 9 {
		t.Errorf("open issue list: %+v", list)
	}
	if err := s.Resolve([]string{"k1"}, "b"); err != nil {
		t.Fatal(err)
	}
	if s.OpenByFile()["A.java"] != 1 || s.Findings("a")[1].State != "resolved" {
		t.Error("resolve did not stick")
	}
	if all := s.AllIssues(); len(all) != 2 || all[1].State != "resolved" || all[1].FixedSHA != "b" || len(all[0].Commits) != 1 {
		t.Errorf("all issues: %+v", all)
	}
	if err := s.SetIssueState("k2", "resolved"); err != nil || s.OpenByFile()["A.java"] != 0 {
		t.Errorf("manual resolve: %v", err)
	}
	if err := s.SetIssueState("nope", "open"); err == nil {
		t.Error("unknown issue accepted")
	}
	if err := s.ReplaceFindings("a", fs[:1]); err != nil || len(s.Findings("a")) != 1 {
		t.Error("replacing findings failed")
	}
	if r, ok := s.GetReview("a"); !ok || r.Status != "done" || s.Counts()["a"]["low"] != 1 {
		t.Errorf("review: %+v", r)
	}
}

func TestIncidents(t *testing.T) {
	s, _ := Open(t.TempDir(), "repo")
	defer s.Close()
	id, err := s.AddIncident(Incident{Title: "Orders fail", Logs: "java.lang.NullPointerException"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetAnalysis(id, json.RawMessage(`{"cause":"x"}`)); err != nil {
		t.Fatal(err)
	}
	in, err := s.Incident(id)
	if err != nil || in.Title != "Orders fail" || string(in.Analysis) != `{"cause":"x"}` || len(s.Incidents()) != 1 {
		t.Errorf("incident: %+v %v", in, err)
	}
}
