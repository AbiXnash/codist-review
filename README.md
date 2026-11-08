# codist-review

How [Codist](https://github.com/AbiXnash/codist) reviews a commit, and where it keeps the results.

- **Review** runs the exact code checks on every changed line, then asks the local model about the changed files.
  Each model claim must quote the code it means; a quote that is not there is dropped, and a claim the code does not
  back (a null warning with nothing that makes the value null, say) is shown as a note rather than stated as fact.
- **Issues** are followed across commits by a key made of the rule or title, the file and the flagged line, so an
  issue is reported once and closed by the commit that fixes it.
- **Baseline** checks the whole code the first time Codist runs on a repository, crediting each issue to the commit
  that last changed its line.
- **Learning from feedback**: with `Learn` on, the model sees a few findings this team marked wrong or useful, as
  examples of what not to report and what is worth reporting here.
- **risk** learns from the repository's own history which commits tend to bring bugs (logistic regression, labels
  from fix commits traced back with git blame), tests itself on the newest commits, and is used only when it ranks
  clearly better than chance.
- **store** keeps reviews, findings, issues and incidents in one SQLite file, through a pure-Go driver.

```go
import (
    review "github.com/AbiXnash/codist-review"
    "github.com/AbiXnash/codist-review/store"
)

st, _ := store.Open(dataDir, "my-repo")
r := &review.Reviewer{Repo: repo, Store: st, Model: client, MaxFiles: 8}
err := r.Review(ctx, sha, review.Options{})
```

```bash
go test -tags 'grammar_subset grammar_subset_java' ./...
```

## Adding a language

A review needs five things from a language, written as the `Language` interface in `lang.go`: which files it owns,
which are tests, how to parse a file into lines and units (classes and their methods), what to learn from the whole
project once per HEAD (`Facts`), and its checks. Java comes from codist-java (`review.Java`).

To add one, say TypeScript:

1. Make a module such as `codist-ts` that parses and checks TypeScript. tree-sitter has a TypeScript grammar, so
   the parser Codist already uses can read it.
2. In it, or in this repository, write a small type with the five methods, the way `javaLang` in `lang.go` wraps
   codist-java. `lang_test.go` has a made-up language in 20 lines to start from.
3. List it: `Reviewer{Languages: []review.Language{review.Java, ts.Language}}`.

Everything else works for the new language as it is: findings and issues, following an issue until a commit fixes
it, the model's review with its claim checks, and Useful/Wrong learning. Still Java only: the callers and contracts
shown to the model, the code graph, the Repository page and running changed methods before and after a commit. Each
of those needs its own part of the language later.

## Licence

Source available under the [PolyForm Noncommercial License 1.0.0](LICENSE): free for any noncommercial purpose, such
as personal study, research, hobby projects and testing, and for use by charities, schools, public research and
public health bodies and government institutions. **Using it for a business, or in anything sold, needs a commercial
licence** from the author: open an issue at [github.com/AbiXnash/codist](https://github.com/AbiXnash/codist/issues)
to ask. Third-party code included in Codist keeps its own licence.

Required Notice: Copyright 2026 Abinash (https://github.com/AbiXnash)
