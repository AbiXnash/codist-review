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
