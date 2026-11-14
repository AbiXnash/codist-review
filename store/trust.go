package store

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// A repository's own settings file can ask Codist to run the repository's code or to send it to another machine.
// Those settings wait until the person says they trust the repository; the list of trusted repositories is a
// file in the data folder, one path per line, so it is the person's and never part of a repository.

func trustFile(dir string) string { return filepath.Join(dir, "trusted") }

// Trusted says whether the person trusted the repository at root.
func Trusted(dir, root string) bool {
	raw, err := os.ReadFile(trustFile(dir))
	return err == nil && slices.Contains(strings.Split(strings.ReplaceAll(string(raw), "\r", ""), "\n"), filepath.Clean(root))
}

// Trust records that the person trusts the repository at root.
func Trust(dir, root string) error {
	if Trusted(dir, root) {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(trustFile(dir), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, filepath.Clean(root))
	return err
}
