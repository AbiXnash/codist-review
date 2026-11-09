package review

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"

	model "github.com/abxengg/codist-model"
	"github.com/abxengg/codist-review/store"
)

// PromptVersion fingerprints the instructions and the reply format the model gets, so each result can be traced
// to the prompt that produced it and a changed prompt never reuses old cached replies.
var PromptVersion = func() string {
	h := sha1.Sum([]byte(systemPrompt + fmt.Sprint(schema)))
	return hex.EncodeToString(h[:4])
}()

// Stats is what the model cost for one commit, as the model server measured it.
type Stats struct {
	PromptVersion  string  `json:"promptVersion"`
	Model          string  `json:"model"`
	Calls          int     `json:"calls"`  // replies the model wrote
	Cached         int     `json:"cached"` // replies reused for a prompt seen before
	PromptTokens   int     `json:"promptTokens"`
	ReplyTokens    int     `json:"replyTokens"`
	ModelSeconds   float64 `json:"modelSeconds"`
	PrefillSeconds float64 `json:"prefillSeconds"` // reading prompts: the wait before the first token
	DecodeSeconds  float64 `json:"decodeSeconds"`  // writing replies
	Hidden         int     `json:"hidden"`         // findings not shown because a person marked them wrong
	Refuted        int     `json:"refuted"`        // serious findings a second look could not confirm, kept as notes
}

func (s *Stats) add(u model.Usage) {
	s.Calls++
	s.PromptTokens += u.PromptTokens
	s.ReplyTokens += u.ReplyTokens
	s.ModelSeconds += u.Took.Seconds()
	s.PrefillSeconds += u.Prefill.Seconds()
	s.DecodeSeconds += u.Decode.Seconds()
}

// cacheKey names a reply by the model, the prompt version and the exact prompt: the same change reviewed again,
// after a rebase, a re-run or on another branch, gets the same reply without asking the model.
func cacheKey(modelName, prompt string) string {
	h := sha1.Sum([]byte(modelName + "\x00" + PromptVersion + "\x00" + prompt))
	return hex.EncodeToString(h[:])
}

// hideWrong counts the findings whose issue a person marked wrong. They are still stored, so the judgement can be
// undone, but their issue is set aside: pages hide them and counts leave them out.
func (r *Reviewer) hideWrong(fs []store.Finding, st *Stats) []store.Finding {
	for k, f := range r.Store.AllFeedback() {
		if f.Verdict != "wrong" {
			continue
		}
		for _, x := range fs {
			if x.Key == k {
				st.Hidden++
			}
		}
	}
	return fs
}

// Usage sums what the model has cost across reviewed commits, so a person can see what reviews take on this
// machine: time per commit, how fast it writes, and how often a reply came from the cache.
func Usage(st *store.Store) map[string]any {
	var commits, calls, cached, promptTokens, replyTokens int
	var seconds, decode float64
	for _, raw := range st.AllStats() {
		var s Stats
		if json.Unmarshal(raw, &s) != nil {
			continue
		}
		commits++
		calls += s.Calls
		cached += s.Cached
		promptTokens += s.PromptTokens
		replyTokens += s.ReplyTokens
		seconds += s.ModelSeconds
		decode += s.DecodeSeconds
	}
	out := map[string]any{"commits": commits, "calls": calls, "cached": cached, "promptVersion": PromptVersion}
	if commits > 0 {
		out["secondsPerCommit"] = seconds / float64(commits)
	}
	if calls > 0 {
		out["promptTokensPerCall"] = promptTokens / calls
	}
	if decode > 0 {
		out["tokensPerSecond"] = float64(replyTokens) / decode
	}
	return out
}
