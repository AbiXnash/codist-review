package review

import (
	"fmt"
	"strings"

	git "github.com/AbiXnash/codist-git"
)

// A change is often wrong only against what the code around it promises: a method whose javadoc says the total
// never goes above what was paid, a parameter named customerId that is passed the limit. With Contracts on, the
// model also sees the signature and javadoc of each method the changed code calls in other files, and the team's
// own rules from .codist/rules.md, so it can judge the change against them rather than guess.

const (
	maxContracts = 8    // per changed file
	maxDocLines  = 12   // of one javadoc
	maxRules     = 4000 // characters of the team's rules
)

// contractContext gives, for each changed file, a prompt section with the methods in other files that the changed
// methods call: their signature with parameter names, and their javadoc.
func (r *Reviewer) contractContext(cd *code, changes []git.FileChange) map[string]string {
	if cd == nil {
		return nil
	}
	out := map[string]string{}
	for _, ch := range changes {
		touches := func(from, to int) bool { // the commit changed a line in this method
			for _, l := range ch.Added {
				if l >= from && l <= to {
					return true
				}
			}
			return false
		}
		var b strings.Builder
		seen := map[string]bool{}
		n := 0
		for _, c := range cd.calls {
			if n >= maxContracts {
				break
			}
			if c.From.File != ch.Path || !touches(c.From.Line, c.From.EndLine) || c.To.File == ch.Path || c.To.Test {
				continue
			}
			key := c.To.File + "#" + c.To.Class + "#" + c.To.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			lines := strings.Split(string(cd.sources[c.To.File]), "\n")
			doc := javadoc(lines, c.To.Line)
			sig := c.To.Signature
			if sig == "" && c.To.Line >= 1 && c.To.Line <= len(lines) {
				sig = strings.TrimSpace(lines[c.To.Line-1])
			}
			if n == 0 {
				b.WriteString("\n## What the called methods promise\n")
				b.WriteString("The changed code calls these methods in other files. Check that it passes the right values to the right " +
					"parameters and keeps what each javadoc promises. Report problems only in the changed file, at its line.\n")
			}
			n++
			fmt.Fprintf(&b, "\n### %s.%s (%s)\n```java\n", c.To.Class, c.To.Name, c.To.File)
			for _, d := range doc {
				b.WriteString(d + "\n")
			}
			b.WriteString(sig + "\n```\n")
		}
		if n > 0 {
			out[ch.Path] = b.String()
		}
	}
	return out
}

// javadoc returns the javadoc written above the declaration at line (1-based), skipping annotations, or nothing.
func javadoc(lines []string, line int) []string {
	i := line - 2
	for i >= 0 {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "@") {
			i--
			continue
		}
		break
	}
	if i < 0 || !strings.HasSuffix(strings.TrimSpace(lines[i]), "*/") {
		return nil
	}
	end := i
	for i >= 0 && !strings.Contains(lines[i], "/**") {
		if strings.Contains(lines[i], "/*") && !strings.Contains(lines[i], "/**") {
			return nil // a plain block comment, not javadoc
		}
		i--
	}
	if i < 0 {
		return nil
	}
	var out []string
	for _, l := range lines[i : end+1] {
		out = append(out, strings.TrimRight(l, " \t\r"))
	}
	if len(out) > maxDocLines {
		out = append(out[:maxDocLines-1], "     …")
	}
	return dedent(out)
}

// docStart is the first line of the javadoc and annotations above the declaration at line, or line itself.
func docStart(lines []string, line int) int {
	start := line
	i := line - 2
	for i >= 0 && (strings.TrimSpace(lines[i]) == "" || strings.HasPrefix(strings.TrimSpace(lines[i]), "@")) {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "@") {
			start = i + 1
		}
		i--
	}
	if i >= 0 && strings.HasSuffix(strings.TrimSpace(lines[i]), "*/") {
		for j := i; j >= 0 && i-j < 40; j-- {
			if strings.Contains(lines[j], "/**") {
				return j + 1
			}
		}
	}
	return start
}

func dedent(ls []string) []string {
	pad := -1
	for _, l := range ls {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " \t"))
		if pad < 0 || n < pad {
			pad = n
		}
	}
	for i, l := range ls {
		if len(l) >= pad && pad > 0 {
			ls[i] = l[pad:]
		}
	}
	return ls
}

// rulesSection is the team's written rules for the model, from .codist/rules.md in the working tree.
func rulesSection(rules string) string {
	rules = strings.TrimSpace(rules)
	if rules == "" {
		return ""
	}
	if len(rules) > maxRules {
		rules = rules[:maxRules] + "\n…"
	}
	return "\n## This team's rules\nThe team wrote these rules for this code. A change that breaks one is a bug even when " +
		"the code runs; name the rule you rely on.\n" + rules + "\n"
}
