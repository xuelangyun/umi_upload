package umi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestScanStatisticsRespectCanceledAppContext(t *testing.T) {
	oldCtx, oldCancel := AppCtx, AppCancel
	ctx, cancel := context.WithCancel(context.Background())
	AppCtx, AppCancel = ctx, cancel
	t.Cleanup(func() {
		cancel()
		AppCtx, AppCancel = oldCtx, oldCancel
	})
	cancel()

	if got := Stats(t.TempDir(), "session", true); got != (SessionStats{}) {
		t.Fatalf("Stats returned partial work after cancellation: %#v", got)
	}
	if start, end := UMIFrameTimes(t.TempDir()); start != nil || end != nil {
		t.Fatalf("UMIFrameTimes returned timestamps after cancellation: %v, %v", start, end)
	}
	if got := Inspect(nil, nil, false); got != nil {
		t.Fatalf("Inspect returned a dataset after cancellation: %#v", got)
	}
}


func TestInventoryAndSnapshotRespectCanceledAppContext(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "payload.bin"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	oldCtx, oldCancel := AppCtx, AppCancel
	ctx, cancel := context.WithCancel(context.Background())
	AppCtx, AppCancel = ctx, cancel
	t.Cleanup(func() {
		cancel()
		AppCtx, AppCancel = oldCtx, oldCancel
	})
	cancel()

	if _, err := Inventory(root); !errors.Is(err, context.Canceled) {
		t.Fatalf("Inventory error = %v, want context.Canceled", err)
	}
	if _, err := Snapshot(root, 2, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Snapshot error = %v, want context.Canceled", err)
	}
}
