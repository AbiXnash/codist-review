package review

import (
	"strings"

	java "github.com/abxengg/codist-java"
)

// Language is everything a review needs from one programming language. Java comes from codist-java. Another language,
// say TypeScript from a codist-ts module, plugs in by providing these and being listed in Reviewer.Languages; the
// review, its prompts, claim checks and issue tracking stay as they are. Callers and contracts shown to the model are
// still Java only.
type Language interface {
	Name() string            // as in a code fence: "java"
	Owns(path string) bool   // a file of this language
	IsTest(path string) bool // test code: checked, but not read by the model
	Parse(path string, src []byte) Parsed
	// Facts is what the checks know about the whole project, built from every main file once per HEAD.
	Facts(files []Parsed) any
	// Check reports problems on the given lines (nil: the whole file), using facts from Facts or nil.
	Check(f Parsed, lines map[int]bool, facts any) []Issue
}

// Issue is one problem a language's checks found, with the lines it covers.
type Issue struct {
	Rule, Severity, Title, Detail, Suggestion string
	Line, End                                 int
}

// Parsed is one parsed source file, as far as a review needs it.
type Parsed interface {
	Lines() []string
	// Units are the file's types, such as classes, each with its header and its members, such as methods, so a long
	// file can be shown to the model as only the parts a commit changed.
	Units() []Unit
}

// Unit is a type in a file: from its first line to the end of its header (fields included), and its members.
type Unit struct {
	Start, HeaderEnd int
	Members          [][2]int // first and last line of each member
}

// Java is the Java language, from codist-java.
var Java Language = javaLang{}

type javaLang struct{}

type javaFile struct{ f *java.File }

func (javaLang) Name() string            { return "java" }
func (javaLang) Owns(path string) bool   { return strings.HasSuffix(path, ".java") }
func (javaLang) IsTest(path string) bool { return strings.Contains(path, "/src/test/") }
func (javaLang) Parse(path string, src []byte) Parsed {
	return javaFile{java.Parse(path, src)}
}

func (javaLang) Facts(files []Parsed) any {
	var fs []*java.File
	for _, p := range files {
		if j, ok := p.(javaFile); ok {
			fs = append(fs, j.f)
		}
	}
	return java.FactsOf(fs)
}

func (javaLang) Check(p Parsed, lines map[int]bool, facts any) []Issue {
	j, ok := p.(javaFile)
	if !ok {
		return nil
	}
	fs, _ := facts.(*java.Facts)
	var out []Issue
	for _, is := range java.CheckWith(j.f, lines, fs) {
		out = append(out, Issue{is.Rule, is.Severity, is.Title, is.Detail, is.Suggestion, is.Line, is.End})
	}
	return out
}

func (j javaFile) Lines() []string { return j.f.Lines }

func (j javaFile) Units() []Unit {
	var out []Unit
	for _, t := range j.f.Types {
		u := Unit{Start: t.Line, HeaderEnd: t.Line}
		for _, fl := range t.Fields {
			u.HeaderEnd = max(u.HeaderEnd, fl.Line)
		}
		for _, m := range t.Methods {
			u.Members = append(u.Members, [2]int{m.Line, m.End})
		}
		out = append(out, u)
	}
	return out
}

// languages are the ones this reviewer reads: Java unless others are listed.
func (r *Reviewer) languages() []Language {
	if len(r.Languages) == 0 {
		return []Language{Java}
	}
	return r.Languages
}

// langOf is the language that owns path, or nil when no language this reviewer reads does.
func (r *Reviewer) langOf(path string) Language {
	for _, l := range r.languages() {
		if l.Owns(path) {
			return l
		}
	}
	return nil
}
