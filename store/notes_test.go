package store

import (
	"testing"
	"time"
)

func TestNotesAreShownButOpenNoIssue(t *testing.T) {
	s, _ := Open(t.TempDir(), "repo")
	now := time.Now()
	s.SetReview(Review{SHA: "a", Status: "done", Started: now, Finished: now})
	err := s.ReplaceFindings("a", []Finding{
		{Path: "A.java", Line: 3, Severity: "high", Source: "rule", Rule: "string-identity", Title: "real", Key: "k1"},
		{Path: "A.java", Line: 9, Severity: "low", Source: "llm", Title: "wrong claim", Key: "k2",
			Doubt: "Codist checked the types on this line: nothing here compares strings with == or !="},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(s.Findings("a")); got != 2 {
		t.Fatalf("both are shown, got %d", got)
	}
	if open := s.OpenIssueList(); len(open) != 1 || open[0].Key != "k1" {
		t.Fatalf("only the real finding is an open issue: %+v", open)
	}
	if c := s.Counts()["a"]; c["high"] != 1 || c["low"] != 0 {
		t.Fatalf("notes do not colour the commit: %+v", c)
	}
	if err := s.SetIssueState("k2", "ignored"); err != nil {
		t.Fatalf("a person can still mark a note wrong: %v", err)
	}
}
