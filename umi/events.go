package umi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	sseUpdateInterval   = 500 * time.Millisecond
	sseHeartbeatInterval = 15 * time.Second
	sseLogLimit          = 250
)

// liveStateSnapshot returns only the changing state needed by the live dashboard.
// The full dataset table remains on /api/state and is fetched only on initialization,
// explicit user actions, or after a reconnect/completion.
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
	}, jobID, logs
}

// sseLogDelta returns new log lines when the previous log slice is still a
// prefix. If the job changed or the bounded server log rolled over, it asks
// the client to replace its local tail with a fresh snapshot.
func sseLogDelta(previousJobID string, previous []string, currentJobID string, current []string) ([]string, bool) {
	if previousJobID != currentJobID {
		return tailLog(current), true
	}
	if len(current) >= len(previous) {
		prefix := true
		for i := range previous {
			if previous[i] != current[i] {
				prefix = false
				break
			}
		}
		if prefix {
			return append([]string(nil), current[len(previous):]...), false
		}
	}
	return tailLog(current), true
}

func tailLog(lines []string) []string {
	if len(lines) > sseLogLimit {
		lines = lines[len(lines)-sseLogLimit:]
	}
	return append([]string(nil), lines...)
}

func writeSSE(w io.Writer, event string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw)
	return err
}

// serveEvents streams changed live state. A client never blocks upload workers:
// each connection owns its snapshot and writes only on its own HTTP handler.
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
	w.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(w, "retry: 2000\n\n"); err != nil {
		return
	}
	flusher.Flush()

	updateTicker := time.NewTicker(sseUpdateInterval)
	defer updateTicker.Stop()
	heartbeatTicker := time.NewTicker(sseHeartbeatInterval)
	defer heartbeatTicker.Stop()

	var previousState []byte
	var previousJobID string
	var previousLogs []string
	sendState := func() bool {
		state, jobID, logs := liveStateSnapshot()
		stateJSON, err := json.Marshal(state)
		if err != nil {
			return true
		}
		delta, reset := sseLogDelta(previousJobID, previousLogs, jobID, logs)
		changed := !bytes.Equal(previousState, stateJSON) || reset || len(delta) > 0
		if !changed {
			return true
		}

		payload := map[string]any{
			"scan":      state["scan"],
			"job":       state["job"],
			"bandwidth": state["bandwidth"],
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
		if err := writeSSE(w, "state", payload); err != nil {
			return false
		}
		flusher.Flush()
		previousState = stateJSON
		previousJobID = jobID
		previousLogs = append(previousLogs[:0], logs...)
		return true
	}

	if !sendState() {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-updateTicker.C:
			if !sendState() {
				return
			}
		case <-heartbeatTicker.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
