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
		name      string
		prevID    string
		prev      []string
		currentID string
		current   []string
		want      []string
		wantReset bool
	}{
		{name: "first snapshot", currentID: "job-1", current: []string{"one", "two"}, want: []string{"one", "two"}, wantReset: true},
		{name: "append", prevID: "job-1", prev: []string{"one", "two"}, currentID: "job-1", current: []string{"one", "two", "three"}, want: []string{"three"}},
		{name: "unchanged", prevID: "job-1", prev: []string{"one"}, currentID: "job-1", current: []string{"one"}, want: nil},
		{name: "new job", prevID: "job-1", prev: []string{"one"}, currentID: "job-2", current: []string{"fresh"}, want: []string{"fresh"}, wantReset: true},
		{name: "log rollover", prevID: "job-1", prev: []string{"old-1", "old-2", "old-3"}, currentID: "job-1", current: []string{"old-2", "old-3", "new"}, want: []string{"old-2", "old-3", "new"}, wantReset: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reset := sseLogDelta(tt.prevID, tt.prev, tt.currentID, tt.current)
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
	previous := make([]string, sseLogLimit+20)
	for i := range previous {
		previous[i] = fmt.Sprintf("line-%d", i)
	}
	current := append(append([]string(nil), previous...), "new-line")

	got, reset := sseLogDelta("job-1", previous, "job-1", current)
	if reset {
		t.Fatal("appending beyond the UI tail limit must remain incremental")
	}
	if !reflect.DeepEqual(got, []string{"new-line"}) {
		t.Fatalf("delta = %#v, want only the appended line", got)
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
	first := &sseSubscriber{events: make(chan []byte, 2), state: stateJSON, jobID: "job-1", logs: []string{"one"}}
	second := &sseSubscriber{events: make(chan []byte, 2), state: stateJSON, jobID: "job-1", logs: []string{"one", "two"}}
	h.clients[first] = struct{}{}
	h.clients[second] = struct{}{}

	h.publishLocked(state, "job-1", []string{"one", "two", "three"})

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

	h.publishLocked(map[string]any{"value": 2}, "", nil)

	if _, exists := h.clients[sub]; exists {
		t.Fatal("slow subscriber was not removed")
	}
	if _, open := <-sub.events; open {
		t.Fatal("slow subscriber channel was not closed")
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
	if !strings.Contains(rec.Body.String(), "event: state\ndata: ") {
		t.Fatalf("initial SSE state event missing: %q", rec.Body.String())
	}
	if rec.Header().Get("X-Accel-Buffering") != "no" {
		t.Fatal("SSE proxy buffering was not disabled")
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
