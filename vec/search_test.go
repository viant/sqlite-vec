package vec

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestIndexBuildWaitCancellation(t *testing.T) {
	e := newCacheEntry()
	if !e.startBuild() {
		t.Fatal("build did not start")
	}
	defer e.finishBuild()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := e.waitForBuild(ctx); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("wait ignored cancellation")
	}
}

func TestSearchCancellationBeforeDatabaseAccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Search(ctx, nil, "docs", "tools", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
