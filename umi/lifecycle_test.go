package umi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
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


func TestProcessOneDoesNotStartAfterShutdownCancellation(t *testing.T) {
	oldCtx, oldCancel := AppCtx, AppCancel
	ctx, cancel := context.WithCancel(context.Background())
	AppCtx, AppCancel = ctx, cancel
	t.Cleanup(func() {
		cancel()
		AppCtx, AppCancel = oldCtx, oldCancel
	})
	cancel()

	job := &Job{Items: []JobItem{{State: "queued"}}}
	ProcessOne(job, 0, &DatasetRecord{}, "upload", nil)

	if job.Items[0].State != "queued" {
		t.Fatalf("canceled task state = %q, want queued", job.Items[0].State)
	}
	if len(job.Actives) != 0 {
		t.Fatalf("canceled task registered active contexts: %d", len(job.Actives))
	}
}


func TestRootUpdateIsRejectedDuringScan(t *testing.T) {
	oldDBPath := DBPath
	oldScan := Scan
	CloseDB()
	DBPath = filepath.Join(t.TempDir(), "roots.db")
	if err := InitDB(); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	activeWork.resetForTests()
	PL.Lock()
	Scan.Running = true
	PL.Unlock()
	t.Cleanup(func() {
		activeWork.Wait()
		activeWork.resetForTests()
		CloseDB()
		DBPath = oldDBPath
		PL.Lock()
		Scan = oldScan
		PL.Unlock()
	})

	handler := NewHandler(fstest.MapFS{})
	req := httptest.NewRequest(http.MethodPost, "/api/roots", strings.NewReader(`{"add":["/tmp/new-root"]}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusConflict {
		t.Fatalf("root update status = %d, want %d: %s", res.Code, http.StatusConflict, res.Body.String())
	}

	db, err := OpenDB()
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM roots WHERE path=?", "/tmp/new-root").Scan(&count); err != nil {
		t.Fatalf("query roots: %v", err)
	}
	if count != 0 {
		t.Fatalf("root update wrote %d rows while scan was running", count)
	}
}
