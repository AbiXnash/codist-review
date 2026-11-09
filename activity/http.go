package activity

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Routes registers what the Activity page reads:
//
//	GET /api/activity          the model calls, newest first
//	GET /api/activity/{id}     one call with its prompt and reply
//	GET /api/activity/lanes    each lane: running now, waiting, recently finished
//	GET /api/activity/log      the kept log lines (?after=seq for newer ones)
//	GET /api/activity/stream   new log lines and model calls as server-sent events
func Routes(handle func(pattern string, h func(http.ResponseWriter, *http.Request)), lanes ...Viewer) {
	handle("GET /api/activity", func(w http.ResponseWriter, r *http.Request) { reply(w, Calls()) })
	handle("GET /api/activity/{id}", func(w http.ResponseWriter, r *http.Request) {
		c, ok := CallByID(r.PathValue("id"))
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			reply(w, map[string]string{"error": "That call is no longer kept."})
			return
		}
		reply(w, c)
	})
	handle("GET /api/activity/lanes", func(w http.ResponseWriter, r *http.Request) {
		out := make([]LaneView, 0, len(lanes))
		for _, l := range lanes {
			if l != nil {
				out = append(out, l.View())
			}
		}
		reply(w, out)
	})
	handle("GET /api/activity/log", func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		reply(w, Default.Lines(after))
	})
	handle("GET /api/activity/stream", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		ch, stop := Default.subscribe()
		defer stop()
		tick := time.NewTicker(20 * time.Second)
		defer tick.Stop()
		fmt.Fprint(w, ": hello\n\n")
		flusher.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-tick.C:
				fmt.Fprint(w, ": keep-alive\n\n")
			case ev := <-ch:
				raw, _ := json.Marshal(ev)
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Kind, raw)
			}
			flusher.Flush()
		}
	})
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
