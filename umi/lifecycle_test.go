package umi

import (
	"testing"
	"time"
)

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
