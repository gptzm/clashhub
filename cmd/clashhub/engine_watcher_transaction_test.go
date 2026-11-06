package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWatcherWaitsForSourceTransaction(t *testing.T) {
	a := &App{stopCh: make(chan struct{})}
	a.sourceRegenMu.Lock()
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- a.watcherEngineUpdate(func() error { close(entered); return nil }) }()
	select {
	case <-entered:
		t.Fatal("watcher crossed active import transaction")
	case <-time.After(25 * time.Millisecond):
	}
	a.sourceRegenMu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("watcher did not proceed")
	}
}

func TestWatcherDoesNotWriteAfterShutdownWhileWaiting(t *testing.T) {
	a := &App{stopCh: make(chan struct{})}
	a.sourceRegenMu.Lock()
	done := make(chan error, 1)
	go func() { done <- a.watcherEngineUpdate(func() error { t.Error("write after shutdown"); return nil }) }()
	a.shuttingDown.Store(true)
	a.sourceRegenMu.Unlock()
	if err := <-done; err == nil {
		t.Fatal("shutdown accepted")
	}
}

func TestWatcherGeneratorCancels(t *testing.T) {
	a := &App{stopCh: make(chan struct{})}
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- a.watcherEngineUpdateContext(func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("generator did not start")
	}
	close(a.stopCh)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("generator survived stop")
	}
}
