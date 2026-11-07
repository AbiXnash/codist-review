package review

import (
	"fmt"
	"sort"
	"strings"

	git "github.com/AbiXnash/codist-git"
	java "github.com/AbiXnash/codist-java"
)

// A change can break code in a file it never touches: a method that now returns null, throws, or means something
// else still has callers that assume the old behaviour. With Callers on, the model also sees those callers.

const (
	maxCallers     = 3  // per changed file
	maxCallerLines = 30 // per caller
)

// callerContext gives, for each changed file, a prompt section with the methods in other files that call the
// methods the commit changed there, from the call graph of the code at sha.
func (r *Reviewer) callerContext(sha string, changes []git.FileChange) map[string]string {
	files, err := r.Repo.Files(sha)
	if err != nil {
		return nil
	}
	var paths []string
	for _, f := range files {
		if strings.HasSuffix(f, ".java") && !strings.Contains(f, "/src/test/") && (r.Ignored == nil || !r.Ignored(f)) {
			paths = append(paths, f)
		}
	}
	sources := r.Repo.ReadAll(sha, paths)
	ix := java.Build(sources, nil)
	calls := java.BuildCallGraph(sources, ix.Types).Calls()
	out := map[string]string{}
	for _, ch := range changes {
		changed := map[string]bool{} // method names the commit changed in this file
		for _, l := range ch.Added {
			if _, m := ix.TypeAt(ch.Path, l); m != nil {
				changed[m.Name] = true
			}
		}
		var found []java.MethodCall
		seen := map[string]bool{}
		for _, c := range calls {
			key := c.From.File + "#" + c.From.Class + "#" + c.From.Name
			if c.To.File != ch.Path || !changed[c.To.Name] || c.From.File == ch.Path || c.From.Test || seen[key] {
				continue
			}
			seen[key] = true
			found = append(found, c)
		}
		if len(found) == 0 {
			continue
		}
		sort.SliceStable(found, func(i, j int) bool { return found[i].Confirmed && !found[j].Confirmed }) // a known receiver first
		var b strings.Builder
		b.WriteString("\n## Code in other files that calls the changed methods\n")
		b.WriteString("Check whether the change breaks what these callers expect. Report problems only in the changed file, at its line.\n")
		for _, c := range found[:min(maxCallers, len(found))] {
			lines := strings.Split(string(sources[c.From.File]), "\n")
			from, to := c.From.Line, min(c.From.EndLine, c.From.Line+maxCallerLines-1, len(lines))
			fmt.Fprintf(&b, "\n### %s: %s.%s calls %s\n```java\n", c.From.File, c.From.Class, c.From.Name, c.To.Name)
			for n := from; n <= to; n++ {
				fmt.Fprintf(&b, "%4d| %s\n", n, lines[n-1])
			}
			if c.From.EndLine > to {
				b.WriteString("     …\n")
			}
			b.WriteString("```\n")
		}
		out[ch.Path] = b.String()
	}
	return out
}
