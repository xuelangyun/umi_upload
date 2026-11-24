package umi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestSSELogDelta(t *testing.T) {
	tests := []struct {
		name            string
		previousJobID   string
		previousSeq     uint64
		currentJobID    string
		currentSeq      uint64
		current         []string
		want            []string
		wantReset       bool
	}{
		{name: "first snapshot", currentJobID: "job-1", currentSeq: 2, current: []string{"one", "two"}, want: []string{"one", "two"}, wantReset: true},
		{name: "append", previousJobID: "job-1", previousSeq: 2, currentJobID: "job-1", currentSeq: 3, current: []string{"one", "two", "three"}, want: []string{"three"}},
		{name: "unchanged", previousJobID: "job-1", previousSeq: 1, currentJobID: "job-1", currentSeq: 1, current: []string{"one"}},
		{name: "new job", previousJobID: "job-1", previousSeq: 8, currentJobID: "job-2", currentSeq: 1, current: []string{"fresh"}, want: []string{"fresh"}, wantReset: true},
		{name: "subscriber fell behind retained history", previousJobID: "job-1", previousSeq: 0, currentJobID: "job-1", currentSeq: 3, current: []string{"two", "three"}, want: []string{"two", "three"}, wantReset: true},
		{name: "sequence moved backwards", previousJobID: "job-1", previousSeq: 5, currentJobID: "job-1", currentSeq: 4, current: []string{"four"}, want: []string{"four"}, wantReset: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reset := sseLogDelta(tt.previousJobID, tt.previousSeq, tt.currentJobID, tt.currentSeq, tt.current)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("delta = %#v, want %#v", got, tt.want)
			}
			if reset != tt.wantReset {
				t.Fatalf("reset = %v, want %v", reset, tt.wantReset)
			}
		})
	}
}

func TestSSELogDeltaBeyondDisplayTailRemainsIncremental(t *testing.T) {
	current := make([]string, sseLogLimit+20)
	for i := range current {
		current[i] = fmt.Sprintf("line-%d", i)
	}
	currentSeq := uint64(len(current))
	current = append(current, "new-line")
	currentSeq++

	got, reset := sseLogDelta("job-1", currentSeq-1, "job-1", currentSeq, current)
	if reset {
		t.Fatal("appending beyond the UI tail limit must remain incremental")
	}
	if !reflect.DeepEqual(got, []string{"new-line"}) {
		t.Fatalf("delta = %#v, want only the appended line", got)
	}
}

func TestSSELogDeltaAtServerHistoryRolloverRemainsIncremental(t *testing.T) {
	previous := make([]string, sseLogHistoryLimit)
	for i := range previous {
		previous[i] = fmt.Sprintf("line-%04d", i)
	}
	current := append(append([]string(nil), previous[1:]...), "new-line")

	got, reset := sseLogDelta("job-1", sseLogHistoryLimit, "job-1", sseLogHistoryLimit+1, current)
	if reset {
		t.Fatal("server history rollover must not reset the client log tail")
	}
	if !reflect.DeepEqual(got, []string{"new-line"}) {
		t.Fatalf("delta = %#v, want only the newly appended line", got)
	}
}

func TestJobLogSequenceTracksBoundedHistory(t *testing.T) {
	job := &Job{}
	for i := 0; i < sseLogHistoryLimit+2; i++ {
		JobLog(job, "", fmt.Sprintf("line-%d", i))
	}
	if job.logSequence != uint64(sseLogHistoryLimit+2) {
		t.Fatalf("log sequence = %d, want %d", job.logSequence, sseLogHistoryLimit+2)
	}
	if len(job.Log) != sseLogHistoryLimit {
		t.Fatalf("retained log length = %d, want %d", len(job.Log), sseLogHistoryLimit)
	}
	if job.Log[0] != "line-2" || job.Log[len(job.Log)-1] != fmt.Sprintf("line-%d", sseLogHistoryLimit+1) {
		t.Fatal("bounded history does not contain the expected most recent lines")
	}
}

func TestTailLogIsBounded(t *testing.T) {
	lines := make([]string, sseLogLimit+10)
	for i := range lines {
		lines[i] = string(rune('a' + (i % 26)))
	}
	got := tailLog(lines)
	if len(got) != sseLogLimit {
		t.Fatalf("tail length = %d, want %d", len(got), sseLogLimit)
	}
	if got[0] != lines[10] || got[len(got)-1] != lines[len(lines)-1] {
		t.Fatal("tail did not preserve the most recent log lines")
	}
}

func TestSSEHubKeepsIndependentClientLogCursors(t *testing.T) {
	h := newSSEHub()
	state := map[string]any{
		"scan": map[string]any{"running": false},
		"job": map[string]any{"id": "job-1", "done": false, "prog": map[string]any{"sent": 10}},
		"bandwidth": map[string]any{"current_mbps": 1.0},
	}
	stateJSON, _ := json.Marshal(state)
	first := &sseSubscriber{events: make(chan []byte, 2), state: stateJSON, jobID: "job-1", logSequence: 1}
	second := &sseSubscriber{events: make(chan []byte, 2), state: stateJSON, jobID: "job-1", logSequence: 2}
	h.clients[first] = struct{}{}
	h.clients[second] = struct{}{}

	h.publishLocked(state, "job-1", 3, []string{"one", "two", "three"})

	readPayload := func(ch <-chan []byte) map[string]any {
		t.Helper()
		frame := <-ch
		line := strings.Split(string(frame), "\n")[1]
		var payload map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
			t.Fatal(err)
		}
		return payload
	}
	firstPayload := readPayload(first.events)
	secondPayload := readPayload(second.events)
	firstJob := firstPayload["job"].(map[string]any)
	secondJob := secondPayload["job"].(map[string]any)
	if got := firstJob["log"].([]any); len(got) != 2 || got[0] != "two" || got[1] != "three" {
		t.Fatalf("first client delta = %#v", got)
	}
	if got := secondJob["log"].([]any); len(got) != 1 || got[0] != "three" {
		t.Fatalf("second client delta = %#v", got)
	}
}

func TestSSEHubDisconnectsSlowSubscriber(t *testing.T) {
	h := newSSEHub()
	stateJSON, _ := json.Marshal(map[string]any{"value": 1})
	sub := &sseSubscriber{
		events: make(chan []byte, 1),
		state: stateJSON,
	}
	sub.events <- []byte("queued")
	h.clients[sub] = struct{}{}

	h.publishLocked(map[string]any{"value": 2}, "", 0, nil)

	if _, exists := h.clients[sub]; exists {
		t.Fatal("slow subscriber was not removed")
	}
	if queued, open := <-sub.events; !open || string(queued) != "queued" {
		t.Fatalf("queued event = %q, open = %v", queued, open)
	}
	if _, open := <-sub.events; open {
		t.Fatal("slow subscriber channel was not closed after draining its queue")
	}
}

func TestServeEventsSendsInitialSnapshot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	serveEvents(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("content type = %q, want text/event-stream", got)
	}
	if !strings.Contains(rec.Body.String(), "retry: 2000\n\n") {
		t.Fatalf("SSE retry directive is not newline-delimited: %q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "event: state\ndata: ") {
		t.Fatalf("initial SSE state event missing: %q", rec.Body.String())
	}
	if rec.Header().Get("X-Accel-Buffering") != "no" {
		t.Fatal("SSE proxy buffering was not disabled")
	}
	if !rec.Flushed {
		t.Fatal("initial SSE snapshot was not flushed")
	}
}

func TestServeEventsRejectsNonGet(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/events", nil)
	rec := httptest.NewRecorder()
	serveEvents(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestSSEStateIncludesDatasetVersion(t *testing.T) {
	frame, err := encodeSSEState(map[string]any{
		"scan": map[string]any{"running": false},
		"job": nil,
		"bandwidth": map[string]any{"current_mbps": 0.0},
		"dataset_version": uint64(42),
	}, nil, false)
	if err != nil {
		t.Fatalf("encode SSE state: %v", err)
	}
	lines := strings.Split(string(frame), "\n")
	if len(lines) < 2 {
		t.Fatalf("invalid SSE frame: %q", frame)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &payload); err != nil {
		t.Fatalf("decode SSE payload: %v", err)
	}
	if got := payload["dataset_version"]; got != float64(42) {
		t.Fatalf("dataset_version = %#v, want 42", got)
	}
}
