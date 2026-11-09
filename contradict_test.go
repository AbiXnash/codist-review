package review

import (
	"strings"
	"testing"

	git "github.com/AbiXnash/codist-git"
	"github.com/AbiXnash/codist-review/store"
)

const inputJava = `package dev.glowmaze;

import java.awt.event.KeyEvent;

public final class Input {
    public void keyPressed(KeyEvent e) {
        String key = KeyEvent.getKeyText(e.getKeyCode());
        if (key == "P") {
            pause();
        }
        if (e.getKeyCode() == KeyEvent.VK_M) {
            mute();
        }
    }
}
`

const stateJava = `package dev.glowmaze;

public final class GameState {
    boolean muted;

    /** M mutes and unmutes. */
    public synchronized void toggleMute() {
        muted = !muted;
    }
}
`

func TestStringEqualityClaimsAreCheckedAgainstTypes(t *testing.T) {
	r := &Reviewer{}
	f := Java.Parse("Input.java", []byte(inputJava))
	ch := git.FileChange{Path: "Input.java"}
	got := r.contradict("abcdef1", ch, f, nil, []store.Finding{
		{Line: 11, Severity: "medium", Title: "Inconsistent string comparison", Detail: "The comparison for the 'M' key still uses '==', which can lead to incorrect results when comparing strings."},
		{Line: 8, Severity: "high", Title: "Strings compared with ==", Detail: "key == \"P\" compares references, not text."},
	})
	if got[0].Doubt == "" || got[0].Severity != "low" || !strings.Contains(got[0].Doubt, "checked the types") {
		t.Fatalf("two int key codes compared with == is not a string comparison: %+v", got[0])
	}
	if got[1].Doubt != "" || got[1].Severity != "high" {
		t.Fatalf("a real string comparison with == stays an alarm: %+v", got[1])
	}
}

func TestMissingClaimsLookAtTheRestOfTheCommit(t *testing.T) {
	r := &Reviewer{}
	f := Java.Parse("GameState.java", []byte(stateJava))
	ch := git.FileChange{Path: "GameState.java"}
	claim := store.Finding{Line: 8, Severity: "high", Title: "Key handling not implemented",
		Detail: "The commit adds a method to toggle mute, but handling of the 'M' key is missing."}
	other := git.FileChange{Path: "Input.java", Patch: "@@ -10,3 +10,6 @@\n         }\n+        if (e.getKeyCode() == KeyEvent.VK_M) {\n+            state.toggleMute();\n+        }\n"}
	got := r.contradict("abcdef1", ch, f, []git.FileChange{other}, []store.Finding{claim})
	if got[0].Doubt == "" || !strings.Contains(got[0].Doubt, "Input.java") {
		t.Fatalf("Input.java in the same commit calls toggleMute: %+v", got[0])
	}
	got = r.contradict("abcdef1", ch, f, nil, []store.Finding{claim})
	if got[0].Doubt != "" {
		t.Fatalf("with no other file doing it, the claim stands: %+v", got[0])
	}
	unrelated := git.FileChange{Path: "Renderer.java", Patch: "@@ -1 +1 @@\n+        draw();\n"}
	if got = r.contradict("abcdef1", ch, f, []git.FileChange{unrelated}, []store.Finding{claim}); got[0].Doubt != "" {
		t.Fatalf("another file that does not use it does not count: %+v", got[0])
	}
}

func TestMemberAtAndOthersSection(t *testing.T) {
	f := Java.Parse("GameState.java", []byte(stateJava))
	if n := memberAt(f, 8); n != "toggleMute" {
		t.Fatalf("member at line 8: %q", n)
	}
	changes := []git.FileChange{{Path: "A.java", Patch: "+a"}, {Path: "B.java", Patch: "+b"}, {Path: "C.java", Status: "deleted"}}
	others := otherChanges(changes, "A.java")
	if len(others) != 1 || others[0].Path != "B.java" {
		t.Fatalf("others: %+v", others)
	}
	if s := othersSection(others); !strings.Contains(s, "### B.java") || !strings.Contains(s, "report problems only in the file under review") {
		t.Fatalf("section:\n%s", s)
	}
	if othersSection(nil) != "" {
		t.Fatal("a one-file commit adds nothing")
	}
}
