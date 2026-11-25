package umi

import (
	"context"
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
