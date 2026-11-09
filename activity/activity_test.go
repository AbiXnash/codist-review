package activity

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	model "github.com/AbiXnash/codist-model"
)

type job struct{ sha string }

func TestLaneQueueAndView(t *testing.T) {
	q := NewLane("AI model", func(j job) string { return j.sha })
	q.Push(job{"a"}, false)
	q.Push(job{"b"}, false)
	q.Push(job{"c"}, true)
	q.Push(job{"a"}, false) // already waiting: no change
	if q.Length() != 3 {
		t.Fatalf("length %d", q.Length())
	}
	j, _ := q.Pop()
	if j.sha != "c" || !q.Running("c") || !q.Waiting("c") {
		t.Fatalf("popped %v", j)
	}
	v := q.View()
	if v.Name != "AI model" || v.Running != "c" || v.Since.IsZero() || v.Count != 2 || strings.Join(v.Waiting, ",") != "a,b" {
		t.Fatalf("view %+v", v)
	}
	q.Done("c")
	v = q.View()
	if v.Running != "" || len(v.Finished) != 1 || v.Finished[0].SHA != "c" || q.Waiting("c") {
		t.Fatalf("after done %+v", v)
	}
	q.SortBy(func(k string) float64 { return map[string]float64{"a": 1, "b": 2}[k] })
	if j, _ := q.Pop(); j.sha != "b" {
		t.Fatalf("sorted: %v", j)
	}
	select {
	case <-q.Wake:
	default:
		t.Fatal("a push wakes the worker")
	}
}

func TestLogKeepsAndStreams(t *testing.T) {
	var out strings.Builder
	h := Default.Wrap(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelInfo}))
	lg := slog.New(h).With("lib", "review")
	start := Default.Lines(0)
	var last int64
	if len(start) > 0 {
		last = start[len(start)-1].Seq
	}
	lg.Info("reviewed", "sha", "abc1234", "took", time.Second)
	lg.Debug("hidden from the terminal") // not shown by next, so not kept either
	got := Default.Lines(last)
	if len(got) != 1 || got[0].Msg != "reviewed" || got[0].Attrs["sha"] != "abc1234" || got[0].Attrs["lib"] != "review" || got[0].Level != "info" {
		t.Fatalf("kept %+v", got)
	}
	if !strings.Contains(out.String(), "reviewed") {
		t.Fatal("records still reach the terminal")
	}
}

func TestRoutesAndCalls(t *testing.T) {
	Watch()
	defer func() { model.Observer = nil }()
	model.Observer(model.Call{ID: 7, Label: model.Label{Kind: "review", Title: "Review A.java", SHA: "abc"}, Model: "m", User: "## Commit\nx", Start: time.Now()})
	model.Observer(model.Call{ID: 7, Label: model.Label{Kind: "review", Title: "Review A.java", SHA: "abc"}, Model: "m", User: "## Commit\nx", Reply: "{}", Done: true,
		Start: time.Now(), Usage: model.Usage{PromptTokens: 10, ReplyTokens: 2, Took: time.Second}})
	q := NewLane("Code checks", func(j job) string { return j.sha })
	q.Push(job{"z"}, false)
	mux := http.NewServeMux()
	Routes(mux.HandleFunc, q)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	get := func(path string, v any) {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		json.NewDecoder(res.Body).Decode(v)
	}
	var cs []Call
	get("/api/activity", &cs)
	if len(cs) == 0 || cs[0].ID != "7" || cs[0].Status != "done" || cs[0].User != "" || cs[0].PromptTokens != 10 {
		t.Fatalf("calls %+v", cs)
	}
	var one Call
	get("/api/activity/7", &one)
	if one.User == "" || one.Response != "{}" {
		t.Fatalf("one call has its texts: %+v", one)
	}
	var lanes []LaneView
	get("/api/activity/lanes", &lanes)
	if len(lanes) != 1 || lanes[0].Count != 1 || lanes[0].Waiting[0] != "z" {
		t.Fatalf("lanes %+v", lanes)
	}
	// the stream sends new log lines
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/activity/stream", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	sc := bufio.NewScanner(res.Body)
	sc.Scan() // ": hello"
	slog.New(Default.Wrap(slog.NewTextHandler(&strings.Builder{}, nil))).Info("streamed line")
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data:") && strings.Contains(sc.Text(), "streamed line") {
			return
		}
	}
	t.Fatal("the stream did not send the new line")
}
