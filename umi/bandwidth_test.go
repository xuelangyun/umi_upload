package umi

import (
	"context"
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
	files := NewDynamicLimiter(2)
	datasets := NewDynamicLimiter(1)
	registerUploadLimiter(files)
	registerDatasetLimiter(datasets)
	defer func() {
		unregisterUploadLimiter(files)
		unregisterDatasetLimiter(datasets)
		runtimeParallel.Store(oldParallel)
	}()

	ApplyRuntimeConfig(map[string]string{
		"jobs": "5", "ds_jobs": "3", "parallel": "7",
		"max_jobs": "20", "max_parallel": "8",
	})
	_, fileLimit := files.Stats()
	_, datasetLimit := datasets.Stats()
	if fileLimit != 15 { t.Fatalf("file limit = %d, want 15", fileLimit) }
	if datasetLimit != 3 { t.Fatalf("dataset limit = %d, want 3", datasetLimit) }
	if got := CurrentUploadParallel(4); got != 7 { t.Fatalf("parallel = %d, want 7", got) }

	// A lower max_parallel must take effect immediately for subsequent uploads,
	// even when the saved manual parallel value is higher.
	ApplyRuntimeConfig(map[string]string{
		"jobs": "5", "ds_jobs": "3", "parallel": "7",
		"max_jobs": "20", "max_parallel": "5",
	})
	if got := CurrentUploadParallel(4); got != 5 { t.Fatalf("parallel after lowering max_parallel = %d, want 5", got) }
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
