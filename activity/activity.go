// Package activity shows what Codist is doing, live: each work lane's queue (what runs now, what waits, what just
// finished), every model call with its prompt and reply, and the log as a stream.
package activity

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	model "github.com/abxengg/codist-model"
)

// ---------------------------------------------------------------- lanes

// Lane holds the jobs waiting in one lane of work, worked one at a time, and remembers the last ones it finished.
type Lane[T any] struct {
	Name string
	Wake chan struct{} // receives when a job is added

	key     func(T) string
	mu      sync.Mutex
	jobs    []T
	queued  map[string]bool
	current string
	started time.Time
	recent  []Finished
}

// Finished is a job a lane completed.
type Finished struct {
	SHA    string    `json:"sha"`
	At     time.Time `json:"at"`
	TookMs int64     `json:"tookMs"`
}

const keepFinished = 40

// NewLane makes a lane whose jobs are told apart by key (a commit id).
func NewLane[T any](name string, key func(T) string) *Lane[T] {
	return &Lane[T]{Name: name, Wake: make(chan struct{}, 1), key: key, queued: map[string]bool{}}
}

// Push adds a job; first puts it at the front. A job already waiting moves to the front when pushed again with first.
func (q *Lane[T]) Push(j T, first bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	k := q.key(j)
	if q.queued[k] {
		if !first || q.current == k {
			return
		}
		for i, x := range q.jobs {
			if q.key(x) == k {
				q.jobs = append(q.jobs[:i], q.jobs[i+1:]...)
				break
			}
		}
	}
	q.queued[k] = true
	if first {
		q.jobs = append([]T{j}, q.jobs...)
	} else {
		q.jobs = append(q.jobs, j)
	}
	select {
	case q.Wake <- struct{}{}:
	default:
	}
}

// Pop takes the next job and marks it running.
func (q *Lane[T]) Pop() (T, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var zero T
	if len(q.jobs) == 0 {
		return zero, false
	}
	j := q.jobs[0]
	q.jobs = q.jobs[1:]
	q.current, q.started = q.key(j), time.Now()
	return j, true
}

// Done marks the running job finished.
func (q *Lane[T]) Done(k string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.queued, k)
	if q.current == k {
		q.recent = append(q.recent, Finished{SHA: k, At: time.Now(), TookMs: time.Since(q.started).Milliseconds()})
		if len(q.recent) > keepFinished {
			q.recent = q.recent[len(q.recent)-keepFinished:]
		}
		q.current = ""
	}
}

// Waiting says whether a job is queued or being worked on.
func (q *Lane[T]) Waiting(k string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.queued[k]
}

// Running says whether the lane is working on k right now.
func (q *Lane[T]) Running(k string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.current == k
}

// SortBy orders the waiting jobs by score, highest first, keeping the order of equal scores.
func (q *Lane[T]) SortBy(score func(k string) float64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	sort.SliceStable(q.jobs, func(i, j int) bool { return score(q.key(q.jobs[i])) > score(q.key(q.jobs[j])) })
}

// Length is how many jobs wait.
func (q *Lane[T]) Length() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.jobs)
}

// LaneView is a lane as the page shows it.
type LaneView struct {
	Name     string     `json:"name"`
	Running  string     `json:"running,omitempty"`
	Since    time.Time  `json:"since,omitzero"`
	Waiting  []string   `json:"waiting"`
	Count    int        `json:"count"`
	Finished []Finished `json:"finished"`
}

const showWaiting = 60

// View is the lane now: the job running, the first jobs waiting, and the recent finished ones, newest first.
func (q *Lane[T]) View() LaneView {
	q.mu.Lock()
	defer q.mu.Unlock()
	v := LaneView{Name: q.Name, Running: q.current, Count: len(q.jobs), Waiting: []string{}, Finished: []Finished{}}
	if q.current != "" {
		v.Since = q.started
	}
	for _, j := range q.jobs[:min(len(q.jobs), showWaiting)] {
		v.Waiting = append(v.Waiting, q.key(j))
	}
	for i := len(q.recent) - 1; i >= 0; i-- {
		v.Finished = append(v.Finished, q.recent[i])
	}
	return v
}

// Viewer is anything with a View: a Lane of any job type.
type Viewer interface{ View() LaneView }

// ---------------------------------------------------------------- log

// Entry is one log line.
type Entry struct {
	Seq   int64             `json:"seq"`
	At    time.Time         `json:"at"`
	Level string            `json:"level"`
	Msg   string            `json:"msg"`
	Attrs map[string]string `json:"attrs,omitempty"`
}

// Event is what the stream sends: a log line or a model call.
type Event struct {
	Kind string `json:"kind"` // log or call
	Log  *Entry `json:"log,omitempty"`
	Call *Call  `json:"call,omitempty"`
}

const keepLog = 600

type core struct {
	mu   sync.Mutex
	seq  int64
	log  []Entry
	subs map[chan Event]struct{}
}

func (c *core) publish(ev Event) {
	for ch := range c.subs {
		select {
		case ch <- ev:
		default: // a slow page misses lines rather than slowing Codist
		}
	}
}

// Log is a slog.Handler that keeps the latest lines for the page and streams new ones, and passes every record on.
type Log struct {
	next  slog.Handler
	c     *core
	attrs []slog.Attr
	group string
}

// Default is the log the page reads.
var Default = &Log{c: &core{subs: map[chan Event]struct{}{}}}

// Wrap makes the handler Codist logs through: records go to next and to the page. Info and above always reach the
// page, debug only when next shows it too.
func (l *Log) Wrap(next slog.Handler) *Log { return &Log{next: next, c: l.c} }

func (l *Log) Enabled(ctx context.Context, lv slog.Level) bool {
	return lv >= slog.LevelInfo || l.next != nil && l.next.Enabled(ctx, lv)
}

func (l *Log) Handle(ctx context.Context, r slog.Record) error {
	if l.next != nil && l.next.Enabled(ctx, r.Level) {
		_ = l.next.Handle(ctx, r)
	}
	e := Entry{At: r.Time, Level: strings.ToLower(r.Level.String()), Msg: r.Message, Attrs: map[string]string{}}
	add := func(a slog.Attr) {
		k := a.Key
		if l.group != "" {
			k = l.group + "." + k
		}
		e.Attrs[k] = a.Value.String()
	}
	for _, a := range l.attrs {
		add(a)
	}
	r.Attrs(func(a slog.Attr) bool { add(a); return true })
	l.c.mu.Lock()
	l.c.seq++
	e.Seq = l.c.seq
	l.c.log = append(l.c.log, e)
	if len(l.c.log) > keepLog {
		l.c.log = l.c.log[len(l.c.log)-keepLog:]
	}
	l.c.publish(Event{Kind: "log", Log: &e})
	l.c.mu.Unlock()
	return nil
}

func (l *Log) WithAttrs(as []slog.Attr) slog.Handler {
	n := *l
	if l.next != nil {
		n.next = l.next.WithAttrs(as)
	}
	n.attrs = append(append([]slog.Attr{}, l.attrs...), as...)
	return &n
}

func (l *Log) WithGroup(name string) slog.Handler {
	n := *l
	if l.next != nil {
		n.next = l.next.WithGroup(name)
	}
	n.group = strings.Trim(l.group+"."+name, ".")
	return &n
}

// Lines are the kept log lines after seq.
func (l *Log) Lines(after int64) []Entry {
	l.c.mu.Lock()
	defer l.c.mu.Unlock()
	out := []Entry{}
	for _, e := range l.c.log {
		if e.Seq > after {
			out = append(out, e)
		}
	}
	return out
}

func (l *Log) subscribe() (chan Event, func()) {
	ch := make(chan Event, 256)
	l.c.mu.Lock()
	l.c.subs[ch] = struct{}{}
	l.c.mu.Unlock()
	return ch, func() {
		l.c.mu.Lock()
		delete(l.c.subs, ch)
		l.c.mu.Unlock()
	}
}

// ---------------------------------------------------------------- model calls

// Call is a model call as the page shows it, in the shape of the interface's Activity.
type Call struct {
	ID           string `json:"id"`
	At           int64  `json:"at"`
	Kind         string `json:"kind"`
	Title        string `json:"title"`
	Status       string `json:"status"` // running, done, failed
	Model        string `json:"model"`
	SHA          string `json:"sha,omitempty"`
	Path         string `json:"path,omitempty"`
	Error        string `json:"error,omitempty"`
	TookMs       int64  `json:"tookMs"`
	PromptTokens int    `json:"promptTokens,omitempty"`
	ReplyTokens  int    `json:"replyTokens,omitempty"`
	System       string `json:"system,omitempty"`
	User         string `json:"user,omitempty"`
	Response     string `json:"response,omitempty"`
}

const keepCalls = 200

var calls = struct {
	mu   sync.Mutex
	list []Call
}{}

// Watch records every model call from now on, for the page.
func Watch() {
	model.Observer = func(c model.Call) {
		v := Call{ID: strconv.FormatInt(c.ID, 10), At: c.Start.UnixMilli(), Kind: or(c.Label.Kind, "model"), Title: or(c.Label.Title, firstHeading(c.User)),
			Status: "running", Model: c.Model, SHA: c.Label.SHA, Path: c.Label.Path, System: c.System, User: c.User}
		if c.Done {
			v.Status, v.Response, v.Error = "done", c.Reply, c.Err
			if c.Err != "" {
				v.Status = "failed"
			}
			v.TookMs, v.PromptTokens, v.ReplyTokens = c.Usage.Took.Milliseconds(), c.Usage.PromptTokens, c.Usage.ReplyTokens
		} else {
			v.TookMs = 0
		}
		calls.mu.Lock()
		i := -1
		for k := range calls.list {
			if calls.list[k].ID == v.ID {
				i = k
				break
			}
		}
		if i >= 0 {
			calls.list[i] = v
		} else {
			calls.list = append(calls.list, v)
			if len(calls.list) > keepCalls {
				calls.list = calls.list[len(calls.list)-keepCalls:]
			}
		}
		calls.mu.Unlock()
		brief := v
		brief.System, brief.User, brief.Response = "", "", ""
		Default.c.mu.Lock()
		Default.c.publish(Event{Kind: "call", Call: &brief})
		Default.c.mu.Unlock()
	}
}

// Calls are the recorded model calls, newest first, without their texts.
func Calls() []Call {
	calls.mu.Lock()
	defer calls.mu.Unlock()
	out := make([]Call, 0, len(calls.list))
	for i := len(calls.list) - 1; i >= 0; i-- {
		c := calls.list[i]
		c.System, c.User, c.Response = "", "", ""
		out = append(out, c)
	}
	return out
}

// CallByID is one recorded call with its prompt and reply.
func CallByID(id string) (Call, bool) {
	calls.mu.Lock()
	defer calls.mu.Unlock()
	for _, c := range calls.list {
		if c.ID == id {
			return c, true
		}
	}
	return Call{}, false
}

func firstHeading(prompt string) string {
	for _, l := range strings.Split(prompt, "\n") {
		if t := strings.TrimSpace(strings.TrimLeft(l, "#")); t != "" && !strings.HasPrefix(t, "```") {
			return t
		}
	}
	return "Model call"
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
