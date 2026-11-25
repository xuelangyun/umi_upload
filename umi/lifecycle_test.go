package umi

import (
	"testing"
	"time"
)


// resetForTests isolates the API shutdown smoke test from other tests in the
// same process. Production code never reopens task admission after Stop.
func (t *workTracker) resetForTests() {
	t.mu.Lock()
	t.stopped = false
	t.mu.Unlock()
}

func TestWorkTrackerStopsAdmissionAndWaitsForActiveWork(t *testing.T) {
	var tracker workTracker
	if !tracker.Begin() {
		t.Fatal("first work item was not admitted")
	}

	tracker.Stop()
	if tracker.Begin() {
		t.Fatal("work was admitted after shutdown stopped admission")
	}

	waited := make(chan struct{})
	go func() {
		tracker.Wait()
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatal("Wait returned before the admitted work item ended")
	case <-time.After(30 * time.Millisecond):
	}

	tracker.End()
	select {
	case <-waited:
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after the admitted work item ended")
	}
}
