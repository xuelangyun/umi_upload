package umi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDynamicLimiterResizesWithoutCancelingActiveWork(t *testing.T) {
	l := NewDynamicLimiter(2)
	if err := l.Acquire(context.Background()); err != nil { t.Fatal(err) }
	if err := l.Acquire(context.Background()); err != nil { t.Fatal(err) }
	l.SetLimit(1)
	active, limit := l.Stats()
	if active != 2 || limit != 1 { t.Fatalf("active/limit = %d/%d, want 2/1", active, limit) }

	acquired := make(chan struct{})
	go func() {
		if err := l.Acquire(context.Background()); err == nil { close(acquired) }
	}()
	select {
	case <-acquired:
		t.Fatal("new work acquired while active count exceeded reduced limit")
	case <-time.After(40 * time.Millisecond):
	}
	l.Release()
	l.Release()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("waiting work was not admitted after an active upload completed")
	}
	l.Release()
	l.Release()
	active, limit = l.Stats()
	if active != 0 || limit != 1 { t.Fatalf("final active/limit = %d/%d, want 0/1", active, limit) }
}

func TestDynamicLimiterIncreaseWakesWaiter(t *testing.T) {
	l := NewDynamicLimiter(1)
	if err := l.Acquire(context.Background()); err != nil { t.Fatal(err) }
	acquired := make(chan struct{})
	go func() {
		if err := l.Acquire(context.Background()); err == nil { close(acquired) }
	}()
	l.SetLimit(2)
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("increasing the limit did not wake a waiting upload")
	}
	l.Release()
	l.Release()
}

func TestDynamicLimiterAcquireHonorsContext(t *testing.T) {
	l := NewDynamicLimiter(1)
	if err := l.Acquire(context.Background()); err != nil { t.Fatal(err) }
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := l.Acquire(ctx); err == nil { t.Fatal("Acquire should return context deadline") }
	l.Release()
}

func TestApplyRuntimeConfigUpdatesActiveLimiters(t *testing.T) {
	oldParallel := runtimeParallel.Load()
	oldActiveCfg := ActiveCfg()
	defer setActiveCfg(oldActiveCfg)
	runtimeConfigMu.Lock()
	oldApplied := runtimeAppliedCfg
	runtimeAppliedCfg = nil
	runtimeConfigMu.Unlock()

	files := NewDynamicLimiter(2)
	datasets := NewDynamicLimiter(1)
	registerUploadLimiter(files)
	registerDatasetLimiter(datasets)
	defer func() {
		unregisterUploadLimiter(files)
		unregisterDatasetLimiter(datasets)
		runtimeParallel.Store(oldParallel)
		runtimeConfigMu.Lock()
		runtimeAppliedCfg = oldApplied
		runtimeConfigMu.Unlock()
	}()

	cfg := map[string]string{
		"jobs": "5", "ds_jobs": "3", "parallel": "7",
		"max_jobs": "20", "max_parallel": "8", "target_mbps": "350",
	}
	ApplyActiveRuntimeConfig(cfg, false)
	if got := ActiveCfg()["target_mbps"]; got != "350" {
		t.Fatalf("active target_mbps = %q, want 350", got)
	}
	_, fileLimit := files.Stats()
	_, datasetLimit := datasets.Stats()
	if fileLimit != 15 { t.Fatalf("file limit = %d, want 15", fileLimit) }
	if datasetLimit != 3 { t.Fatalf("dataset limit = %d, want 3", datasetLimit) }
	if got := CurrentUploadParallel(4); got != 7 { t.Fatalf("parallel = %d, want 7", got) }

	// Simulate tuner-discovered concurrency. Updating non-concurrency
	// settings must retain these values rather than resetting to the baseline.
	files.SetLimit(18)
	runtimeParallel.Store(8)
	updated := copyStringMap(cfg)
	updated["target_mbps"] = "420"
	updated["auto_tune"] = "false"
	ApplyActiveRuntimeConfig(updated, false)
	_, fileLimit = files.Stats()
	if fileLimit != 18 { t.Fatalf("unrelated config update reset file limit to %d, want 18", fileLimit) }
	if got := CurrentUploadParallel(4); got != 8 { t.Fatalf("unrelated config update reset parallel to %d, want 8", got) }

	// An explicit max_parallel change rebases the multipart limit immediately.
	updated["max_parallel"] = "5"
	ApplyActiveRuntimeConfig(updated, false)
	if got := CurrentUploadParallel(4); got != 5 { t.Fatalf("parallel after lowering max_parallel = %d, want 5", got) }
}
func TestResetRuntimeTuningRestoresConfiguredBaseline(t *testing.T) {
	oldParallel := runtimeParallel.Load()
	oldActiveCfg := ActiveCfg()
	defer setActiveCfg(oldActiveCfg)
	runtimeConfigMu.Lock()
	oldApplied := runtimeAppliedCfg
	runtimeAppliedCfg = nil
	runtimeConfigMu.Unlock()
	defer func() {
		runtimeParallel.Store(oldParallel)
		runtimeConfigMu.Lock()
		runtimeAppliedCfg = oldApplied
		runtimeConfigMu.Unlock()
	}()

	cfg := map[string]string{
		"jobs": "5", "ds_jobs": "2", "parallel": "4",
		"max_jobs": "20", "max_parallel": "12",
	}
	ApplyActiveRuntimeConfig(cfg, false)
	runtimeParallel.Store(9)
	ResetRuntimeTuning(cfg)
	if got := ActiveCfg()["parallel"]; got != "4" {
		t.Fatalf("active parallel = %q, want 4", got)
	}
	if got := CurrentUploadParallel(1); got != 4 {
		t.Fatalf("new upload inherited tuned parallel %d, want configured baseline 4", got)
	}
}

func TestHashFileStopsWhenStopSignalIsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.bin")
	if err := os.WriteFile(path, make([]byte, 8*1024*1024), 0o600); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	close(stop)
	if _, err := hashFileWithStop(path, nil, stop); !errors.Is(err, context.Canceled) {
		t.Fatalf("hashFileWithStop error = %v, want context.Canceled", err)
	}
}

func TestOSSResumeProgressIsNotCountedAsCurrentTraffic(t *testing.T) {
	if !isOSSResumeProgress(20, 20, 20) {
		t.Fatal("checkpoint resume callback must be identified")
	}
	if isOSSResumeProgress(20, 10, 30) {
		t.Fatal("a newly completed part must count as current traffic")
	}
	if isOSSResumeProgress(0, 10, 10) {
		t.Fatal("a fresh upload's first part must count as current traffic")
	}
}

func TestAddNetworkTrafficDoesNotChangeLogicalProgress(t *testing.T) {
	job := &Job{M: NewMeter()}
	ctx := &JobContext{Parent: job, Prog: ProgState{Total: 100}}
	AddNetworkTraffic(ctx, 50)
	if ctx.Prog.Sent != 0 {
		t.Fatalf("network-only bytes changed logical progress to %d", ctx.Prog.Sent)
	}
	if got := job.M.Total(); got != 50 {
		t.Fatalf("network meter total = %d, want 50", got)
	}
}

func TestCappedProgressDeltaNeverExceedsObjectSize(t *testing.T) {
	tests := []struct {
		current, total, delta, want int64
	}{
		{current: 0, total: 100, delta: 40, want: 40},
		{current: 80, total: 100, delta: 50, want: 20},
		{current: 100, total: 100, delta: 10, want: 0},
		{current: 20, total: 100, delta: -5, want: 0},
	}
	for _, tt := range tests {
		if got := cappedProgressDelta(tt.current, tt.total, tt.delta); got != tt.want {
			t.Errorf("cappedProgressDelta(%d, %d, %d) = %d, want %d", tt.current, tt.total, tt.delta, got, tt.want)
		}
	}
}

func TestS3ProgressHookReportsRequestBodyBytes(t *testing.T) {
	var reported int64
	hook := &s3ProgressHook{onDelta: func(n int64) { reported += n }}
	buf := make([]byte, 4096)
	n, err := hook.Read(buf)
	if err != nil {
		t.Fatalf("progress hook Read returned error: %v", err)
	}
	if n != len(buf) {
		t.Fatalf("progress hook Read = %d, want %d", n, len(buf))
	}
	if reported != int64(len(buf)) {
		t.Fatalf("reported request bytes = %d, want %d", reported, len(buf))
	}
}

func TestAddSentRollbackDoesNotReduceNetworkMeter(t *testing.T) {
	job := &Job{M: NewMeter()}
	ctx := &JobContext{Parent: job, Prog: ProgState{Total: 100}}
	AddSent(ctx, 80)
	AddSent(ctx, -80)
	if ctx.Prog.Sent != 0 {
		t.Fatalf("logical progress after rollback = %d, want 0", ctx.Prog.Sent)
	}
	if got := job.M.Total(); got != 80 {
		t.Fatalf("network meter total after rollback = %d, want 80", got)
	}
}

func TestAddProgressSentDoesNotAffectNetworkMeter(t *testing.T) {
	job := &Job{M: NewMeter()}
	ctx := &JobContext{Parent: job, Prog: ProgState{Total: 100}}
	AddProgressSent(ctx, 100)
	if ctx.Prog.Sent != 100 {
		t.Fatalf("logical progress for an existing remote object = %d, want 100", ctx.Prog.Sent)
	}
	if got := job.M.Total(); got != 0 {
		t.Fatalf("existing remote object contributed %d network bytes, want 0", got)
	}
}

func TestDynamicLimiterRejectsAlreadyCanceledContext(t *testing.T) {
	l := NewDynamicLimiter(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.Acquire(ctx); err == nil {
		t.Fatal("Acquire admitted work with an already-canceled context")
	}
	active, limit := l.Stats()
	if active != 0 || limit != 1 {
		t.Fatalf("active/limit = %d/%d, want 0/1", active, limit)
	}
}


func TestTuneActionAllowedUsesLatestSettings(t *testing.T) {
	if tuneActionAllowed(map[string]string{"auto_tune": "false", "target_mbps": "350"}, 10) {
		t.Fatal("disabled auto tuning must reject a pending increase")
	}
	if tuneActionAllowed(map[string]string{"auto_tune": "true", "target_mbps": "100"}, 95) {
		t.Fatal("a newly reached target must reject a stale increase")
	}
	if !tuneActionAllowed(map[string]string{"auto_tune": "true", "target_mbps": "100"}, 94) {
		t.Fatal("an unmet latest target should allow the pending increase")
	}
	if !tuneActionAllowed(map[string]string{"auto_tune": "true", "target_mbps": "0"}, 0) {
		t.Fatal("zero target should retain exploratory tuning")
	}
}

func TestDecideBandwidthTune(t *testing.T) {
	tests := []struct {
		name string
		enabled bool
		target, speed float64
		active, fileLimit, parallel, maxFiles, maxParallel int
		wantState, wantAction string
		wantFileLimit, wantParallel int
	}{
		{
			name: "holds when target threshold is reached",
			enabled: true, target: 100, speed: 95,
			active: 3, fileLimit: 3, parallel: 4, maxFiles: 8, maxParallel: 8,
			wantState: "holding", wantFileLimit: 3, wantParallel: 4,
		},
		{
			name: "increases file concurrency before multipart",
			enabled: true, target: 100, speed: 94.9,
			active: 4, fileLimit: 4, parallel: 4, maxFiles: 8, maxParallel: 8,
			wantState: "increasing-files", wantAction: "files", wantFileLimit: 6, wantParallel: 4,
		},
		{
			name: "zero target explores available file concurrency",
			enabled: true, target: 0, speed: 999,
			active: 2, fileLimit: 4, parallel: 4, maxFiles: 8, maxParallel: 8,
			wantState: "increasing-parts", wantAction: "parts", wantFileLimit: 4, wantParallel: 5,
		},
		{
			name: "file concurrency increase is clamped to max jobs",
			enabled: true, target: 100, speed: 10,
			active: 7, fileLimit: 7, parallel: 4, maxFiles: 8, maxParallel: 8,
			wantState: "increasing-files", wantAction: "files", wantFileLimit: 8, wantParallel: 4,
		},
		{
			name: "multipart concurrency never exceeds its ceiling",
			enabled: true, target: 100, speed: 10,
			active: 2, fileLimit: 4, parallel: 8, maxFiles: 4, maxParallel: 8,
			wantState: "task-limited", wantFileLimit: 4, wantParallel: 8,
		},
		{
			name: "reaches limit when both concurrency ceilings are full",
			enabled: true, target: 100, speed: 10,
			active: 4, fileLimit: 4, parallel: 8, maxFiles: 4, maxParallel: 8,
			wantState: "limit-reached", wantFileLimit: 4, wantParallel: 8,
		},
		{
			name: "disabled tuner leaves manual concurrency unchanged",
			enabled: false, target: 100, speed: 1,
			active: 4, fileLimit: 4, parallel: 4, maxFiles: 8, maxParallel: 8,
			wantState: "manual", wantFileLimit: 4, wantParallel: 4,
		},
		{
			name: "waits while no file is actively uploading",
			enabled: true, target: 100, speed: 1,
			active: 0, fileLimit: 1, parallel: 4, maxFiles: 8, maxParallel: 8,
			wantState: "waiting", wantFileLimit: 1, wantParallel: 4,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decideBandwidthTune(
				tt.enabled, tt.target, tt.speed,
				tt.active, tt.fileLimit, tt.parallel, tt.maxFiles, tt.maxParallel,
			)
			if got.state != tt.wantState {
				t.Errorf("state = %q, want %q", got.state, tt.wantState)
			}
			if got.action != tt.wantAction {
				t.Errorf("action = %q, want %q", got.action, tt.wantAction)
			}
			if got.fileLimit != tt.wantFileLimit {
				t.Errorf("fileLimit = %d, want %d", got.fileLimit, tt.wantFileLimit)
			}
			if got.parallel != tt.wantParallel {
				t.Errorf("parallel = %d, want %d", got.parallel, tt.wantParallel)
			}
		})
	}
}


func TestShouldWaitForUploadActivityIncludesIdleJobs(t *testing.T) {
	tests := []struct {
		name string
		enabled, recent bool
		want                  bool
	}{
		{name: "idle auto tuner waits", enabled: true, recent: false, want: true},
		{name: "active transfer can tune", enabled: true, recent: true, want: false},
		{name: "manual mode does not wait", enabled: false, recent: false, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldWaitForUploadActivity(tt.enabled, tt.recent); got != tt.want {
				t.Fatalf("shouldWaitForUploadActivity(%v, %v) = %v, want %v", tt.enabled, tt.recent, got, tt.want)
			}
		})
	}
}

// TestMeterHasRecentActivityPreventsStaleThroughputTuning verifies that a
// previously active transfer is not mistaken for current upload activity.
func TestMeterHasRecentActivityPreventsStaleThroughputTuning(t *testing.T) {
	m := NewMeter()
	if m.HasRecentActivity(10 * time.Second) {
		t.Fatal("new meter must not report upload activity")
	}
	m.Add(1024)
	if !m.HasRecentActivity(10 * time.Second) {
		t.Fatal("recently acknowledged bytes must count as upload activity")
	}

	m.mu.Lock()
	m.q[len(m.q)-1].t = nowSec() - 20
	m.mu.Unlock()
	if m.HasRecentActivity(10 * time.Second) {
		t.Fatal("stale upload activity must not be treated as current")
	}
}
