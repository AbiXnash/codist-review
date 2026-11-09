package review

import (
	"strings"
	"testing"

	"github.com/abxengg/codist-review/store"
)

func TestJavadoc(t *testing.T) {
	src := strings.Split(`class A {
    /**
     * Records a refund. The total never goes above what was paid.
     */
    @Transactional
    public long refund(long orderId, long amount) {
        return 0;
    }

    /* not javadoc */
    void b() {
    }

    void c() {
    }
}`, "\n")
	got := javadoc(src, 6)
	if len(got) != 3 || !strings.Contains(got[1], "never goes above") || !strings.HasPrefix(got[0], "/**") {
		t.Fatalf("javadoc of refund: %q", got)
	}
	if javadoc(src, 11) != nil || javadoc(src, 14) != nil {
		t.Fatal("a plain comment or no comment is not javadoc")
	}
	if docStart(src, 6) != 2 || docStart(src, 14) != 14 {
		t.Fatalf("docStart: %d %d", docStart(src, 6), docStart(src, 14))
	}
	if rulesSection("  ") != "" || !strings.Contains(rulesSection("1. One discount per order."), "One discount") {
		t.Fatal("rules section")
	}
}

func TestTriage(t *testing.T) {
	f := func(sev, title, doubt string) store.Finding {
		return store.Finding{Source: "model", Severity: sev, Title: title, Doubt: doubt}
	}
	var ex []TriageExample
	for i := 0; i < 12; i++ {
		ex = append(ex, TriageExample{f("high", "Refund total can exceed amount paid", ""), true},
			TriageExample{f("low", "Null or empty input not validated", ""), false})
	}
	if TrainTriage(ex[:10]) != nil {
		t.Fatal("too few judged findings to trust")
	}
	tr := TrainTriage(ex)
	if tr == nil {
		t.Fatal("enough judged findings")
	}
	weak := tr.demote(f("medium", "Empty code input not validated", ""))
	if weak.Severity != "low" || weak.Doubt == "" {
		t.Fatalf("a finding like the wrong ones should become a note: %+v", weak)
	}
	if good := tr.demote(f("high", "Refund can exceed what was paid", "")); good.Severity != "high" {
		t.Fatalf("a finding like the useful ones stays: %+v", good)
	}
	if rule := tr.demote(store.Finding{Source: "rule", Severity: "high", Title: "Null or empty input not validated"}); rule.Severity != "high" {
		t.Fatal("code checks are never demoted")
	}
	var none *Triage
	if none.demote(f("low", "x", "")).Severity != "low" {
		t.Fatal("no triage changes nothing")
	}
}
