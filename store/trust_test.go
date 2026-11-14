package store

import (
	"path/filepath"
	"testing"
)

func TestTrust(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	a, b := filepath.Join(t.TempDir(), "my repo"), filepath.Join(t.TempDir(), "other")
	if Trusted(dir, a) {
		t.Error("nothing is trusted at first")
	}
	if err := Trust(dir, a); err != nil {
		t.Fatal(err)
	}
	if err := Trust(dir, a+string(filepath.Separator)); err != nil { // the same folder, written another way
		t.Fatal(err)
	}
	if !Trusted(dir, a) || Trusted(dir, b) || Trusted(dir, filepath.Dir(a)) {
		t.Error("only the repository named is trusted")
	}
	if err := Trust(dir, b); err != nil || !Trusted(dir, b) || !Trusted(dir, a) {
		t.Errorf("a second repository: %v", err)
	}
}
