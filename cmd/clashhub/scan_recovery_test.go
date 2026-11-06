package main

import (
	"testing"
	"time"
)

func TestHealthyScanRecoveryGenerationAndCooldown(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := &App{cfg: Config{RecoveryInitSec: 60}, mode: ModeFallback, lastSwitch: time.Now(), nextRecovery: time.Now().Add(time.Hour), engineGeneration: 2, routingGeneration: 3, stopCh: make(chan struct{})}
	original := a.nextRecovery
	a.recoverAfterHealthyScan(1, 3)
	if !a.nextRecovery.Equal(original) {
		t.Fatal("stale engine scan changed recovery")
	}
	a.recoverAfterHealthyScan(2, 2)
	if !a.nextRecovery.Equal(original) {
		t.Fatal("stale routing scan changed recovery")
	}
	a.recoverAfterHealthyScan(2, 3)
	a.mu.Lock()
	got := a.nextRecovery
	a.mu.Unlock()
	if !got.Equal(a.lastSwitch.Add(time.Minute)) {
		t.Fatalf("cooldown not preserved: %v", got)
	}
	a.transitionMu.Lock() // join any launched attempt before touching state
	a.mu.Lock()
	a.mode = ModePrimary
	a.nextRecovery = original
	a.mu.Unlock()
	a.recoverAfterHealthyScan(2, 3)
	a.mu.Lock()
	got = a.nextRecovery
	a.mu.Unlock()
	a.transitionMu.Unlock()
	if !got.Equal(original) {
		t.Fatal("primary mode changed")
	}
}
