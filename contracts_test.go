package review

import (
	"strings"
	"testing"
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
