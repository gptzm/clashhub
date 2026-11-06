package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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

func TestWatcherGeneratorPreservesProfilesAndCancels(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(genScriptPath()), 0700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "profiles")
	script := "import os,time,pathlib\npathlib.Path(" + strconv.Quote(output) + ").write_text(os.environ.get('SRHOP_SOURCE_PROFILES','missing'))\ntime.sleep(30)\n"
	if err := os.WriteFile(genScriptPath(), []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	a := &App{stopCh: make(chan struct{})}
	done := make(chan error, 1)
	profiles := []string{"/tmp/custom profile.yaml", "/tmp/second.yaml"}
	go func() {
		done <- a.watcherEngineUpdateContext(func(ctx context.Context) error { return runGeneratorWithSourcesContext(ctx, profiles) })
	}()
	deadline := time.Now().Add(3 * time.Second)
	var data []byte
	for {
		data, _ = os.ReadFile(output)
		if len(data) > 0 {
			break
		}
		if time.Now().After(deadline) {
			close(a.stopCh)
			t.Fatal("generator not started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var got []string
	if json.Unmarshal(data, &got) != nil || !reflect.DeepEqual(got, profiles) {
		close(a.stopCh)
		t.Fatalf("profiles changed: %q", data)
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
