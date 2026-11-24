package umi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestSSELogDelta(t *testing.T) {
	tests := []struct {
		name       string
		prevID     string
		prev       []string
		currentID  string
		current    []string
		want       []string
		wantReset  bool
	}{
		{
			name: "first snapshot",
			currentID: "job-1",
			current: []string{"one", "two"},
			want: []string{"one", "two"},
			wantReset: true,
		},
		{
			name: "append",
			prevID: "job-1", prev: []string{"one", "two"},
			currentID: "job-1", current: []string{"one", "two", "three"},
			want: []string{"three"},
		},
		{
			name: "unchanged",
			prevID: "job-1", prev: []string{"one"},
			currentID: "job-1", current: []string{"one"},
			want: nil,
		},
		{
			name: "new job",
			prevID: "job-1", prev: []string{"one"},
			currentID: "job-2", current: []string{"fresh"},
			want: []string{"fresh"},
			wantReset: true,
		},
		{
			name: "log rollover",
			prevID: "job-1", prev: []string{"old-1", "old-2", "old-3"},
			currentID: "job-1", current: []string{"old-2", "old-3", "new"},
			want: []string{"old-2", "old-3", "new"},
			wantReset: true,
		},
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
