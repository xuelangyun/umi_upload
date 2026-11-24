package umi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

const (
	sseUpdateInterval    = 500 * time.Millisecond
	sseHeartbeatInterval = 15 * time.Second
	sseClientBuffer      = 8
	sseWriteTimeout      = 10 * time.Second
	sseLogLimit          = 250
	sseLogHistoryLimit   = 1500
)

type sseSubscriber struct {
	events      chan []byte
	state       []byte
	jobID       string
	logs        []string
}

type sseHub struct {
	mu      sync.Mutex
	once    sync.Once
	clients map[*sseSubscriber]struct{}
}

var dashboardSSE = newSSEHub()

func newSSEHub() *sseHub {
	return &sseHub{clients: make(map[*sseSubscriber]struct{})}
}

// liveStateSnapshot takes one bounded snapshot shared by every connected
// browser. The potentially large dataset table remains on /api/state.
func liveStateSnapshot() (map[string]any, string, []string) {
	scan := ScanView()

	PL.Lock()
	var current *Job
	for _, job := range Jobs {
		current = job
		break
	}
	var jobID, action string
	var done bool
	var logs []string
	if current != nil {
		jobID = current.ID
		action = current.Action
		done = current.Done
		logs = append([]string(nil), current.Log...)
	}
	PL.Unlock()

	var jobValue any
	if current != nil {
		jobValue = map[string]any{
			"id":     jobID,
			"action": action,
			"done":   done,
			"prog":   ProgView(current),
		}
	}
	return map[string]any{
		"scan":      scan,
		"job":       jobValue,
		"bandwidth": CurrentBandwidthTuneStatus(),
		"dataset_version": datasetRowsVersion(),
	}, jobID, logs
}

// logOverlap finds the longest prefix of current that is also a suffix of
// previous. This keeps incremental delivery working when Job.Log reaches its
// fixed history limit and drops its oldest entries on each append.
func logOverlap(previous, current []string) int {
	if len(previous) == 0 || len(current) == 0 {
		return 0
	}

	// Build the KMP failure table for current, then scan previous once.
	// The work is linear in the bounded server-side log history.
	failure := make([]int, len(current))
	for i, matched := 1, 0; i < len(current); i++ {
		for matched > 0 && current[i] != current[matched] {
			matched = failure[matched-1]
		}
		if current[i] == current[matched] {
			matched++
		}
		failure[i] = matched
	}

	matched := 0
	for _, line := range previous {
		for matched > 0 && (matched == len(current) || line != current[matched]) {
			matched = failure[matched-1]
		}
		if matched < len(current) && line == current[matched] {
			matched++
		}
	}
	return matched
}

// sseLogDelta returns only lines not already represented by the subscriber.
// A job change or a log replacement without an overlapping sequence resets
// the client tail; normal appends and bounded-history rollovers stay incremental.
func sseLogDelta(previousJobID string, previous []string, currentJobID string, current []string) ([]string, bool) {
	if previousJobID != currentJobID {
		return tailLog(current), true
	}
	overlap := logOverlap(previous, current)
	if overlap > 0 || len(previous) == 0 {
		return append([]string(nil), current[overlap:]...), false
	}
	return tailLog(current), true
}

func tailLog(lines []string) []string {
	if len(lines) > sseLogLimit {
		lines = lines[len(lines)-sseLogLimit:]
	}
	return append([]string(nil), lines...)
}

func encodeSSEState(state map[string]any, delta []string, reset bool) ([]byte, error) {
	payload := map[string]any{
		"scan":      state["scan"],
		"job":       state["job"],
		"bandwidth": state["bandwidth"],
		"dataset_version": state["dataset_version"],
	}
	if job, ok := state["job"].(map[string]any); ok {
		jobWithLog := make(map[string]any, len(job)+2)
		for key, value := range job {
			jobWithLog[key] = value
		}
		jobWithLog["log"] = delta
		jobWithLog["log_reset"] = reset
		payload["job"] = jobWithLog
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	frame := make([]byte, 0, len(raw)+24)
	frame = append(frame, "event: state\ndata: "...)
	frame = append(frame, raw...)
	frame = append(frame, '\n', '\n')
	return frame, nil
}

// subscribe registers a client with its own delta baseline and an initial
// complete live snapshot. A new client cannot inherit another client's log
// cursor and therefore cannot see duplicate or missing log lines.
func (h *sseHub) subscribe() (<-chan []byte, func(), error) {
	h.mu.Lock()
	state, jobID, logs := liveStateSnapshot()
	stateJSON, err := json.Marshal(state)
	if err != nil {
		h.mu.Unlock()
		return nil, nil, err
	}
	frame, err := encodeSSEState(state, tailLog(logs), true)
	if err != nil {
		h.mu.Unlock()
		return nil, nil, err
	}
	sub := &sseSubscriber{
		events: make(chan []byte, sseClientBuffer),
		state:  stateJSON,
		jobID:  jobID,
		logs:   append([]string(nil), logs...),
	}
	sub.events <- frame
	h.clients[sub] = struct{}{}
	h.mu.Unlock()

	ctx := AppCtx
	if ctx == nil {
		ctx = context.Background()
	}
	h.once.Do(func() { go h.run(ctx) })

	unsubscribe := func() {
		h.mu.Lock()
		if _, exists := h.clients[sub]; exists {
			delete(h.clients, sub)
			close(sub.events)
		}
		h.mu.Unlock()
	}
	return sub.events, unsubscribe, nil
}

// publishLocked fans one status snapshot out to all clients. Slow consumers
// are disconnected instead of back-pressuring the upload workers or other
// browser sessions; EventSource reconnects and the next subscription starts
// from a fresh snapshot.
func (h *sseHub) publishLocked(state map[string]any, jobID string, logs []string) {
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return
	}
	for sub := range h.clients {
		delta, reset := sseLogDelta(sub.jobID, sub.logs, jobID, logs)
		if bytes.Equal(sub.state, stateJSON) && !reset && len(delta) == 0 {
			continue
		}
		frame, err := encodeSSEState(state, delta, reset)
		if err != nil {
			continue
		}
		select {
		case sub.events <- frame:
			sub.state = append(sub.state[:0], stateJSON...)
			sub.jobID = jobID
			sub.logs = append(sub.logs[:0], logs...)
		default:
			delete(h.clients, sub)
			close(sub.events)
		}
	}
}

func (h *sseHub) publishCurrent() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.clients) == 0 {
		return
	}
	state, jobID, logs := liveStateSnapshot()
	h.publishLocked(state, jobID, logs)
}

func (h *sseHub) run(ctx context.Context) {
	ticker := time.NewTicker(sseUpdateInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			h.mu.Lock()
			for sub := range h.clients {
				delete(h.clients, sub)
				close(sub.events)
			}
			h.mu.Unlock()
			return
		case <-ticker.C:
			h.publishCurrent()
		}
	}
}

func writeSSEFrame(w http.ResponseWriter, flusher http.Flusher, data []byte) error {
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
	_, err := w.Write(data)
	if err == nil {
		flusher.Flush()
	}
	_ = controller.SetWriteDeadline(time.Time{})
	return err
}

// serveEvents streams a shared status snapshot. Each response only writes its
// own buffered events; a slow browser is evicted after its queue fills.
func serveEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "Method Not Allowed"})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming is not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	if writeSSEFrame(w, flusher, []byte("retry: 2000\n\n")) != nil {
		return
	}

	events, unsubscribe, err := dashboardSSE.subscribe()
	if err != nil {
		return
	}
	defer unsubscribe()

	heartbeat := time.NewTicker(sseHeartbeatInterval)
	defer heartbeat.Stop()

	// Deliver the initial full live snapshot even when the request context is
	// already closing; this also makes initialization deterministic in tests.
	initial, open := <-events
	if !open || writeSSEFrame(w, flusher, initial) != nil {
		return
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case frame, open := <-events:
			if !open || writeSSEFrame(w, flusher, frame) != nil {
				return
			}
		case <-heartbeat.C:
			if writeSSEFrame(w, flusher, []byte(": keep-alive\n\n")) != nil {
				return
			}
		}
	}
}

// liveHTTPStateSnapshot adds the bounded log tail needed by HTTP fallback.
// It excludes metadata and the potentially large dataset list.
func liveHTTPStateSnapshot() map[string]any {
	state, _, logs := liveStateSnapshot()
	if job, ok := state["job"].(map[string]any); ok {
		job["log"] = tailLog(logs)
	}
	return state
}
