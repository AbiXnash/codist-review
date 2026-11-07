package review

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"

	model "github.com/AbiXnash/codist-model"
	"github.com/AbiXnash/codist-review/store"
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
