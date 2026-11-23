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
