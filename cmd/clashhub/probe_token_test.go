package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProbeTokenRejectsEngineAndRoutingChanges(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := &App{}
	token, ok := a.snapshotProbeToken()
	if !ok {
		t.Fatal("initial token rejected")
	}
	a.engineMu.Lock()
	a.routingGate.RLock()
	if !a.probeTokenValidLocked(token) {
		t.Fatal("current token rejected")
	}
	a.engineGeneration++
	if a.probeTokenValidLocked(token) {
		t.Fatal("stale engine accepted")
	}
	a.engineGeneration--
	a.aiSelectionGeneration++
	if !a.probeTokenValidLocked(token) {
		t.Fatal("AI selector change invalidated ordinary recovery")
	}
	a.aiSelectionGeneration--
	a.selectionGeneration++
	if a.probeTokenValidLocked(token) {
		t.Fatal("stale selection accepted")
	}
	a.selectionGeneration--
	a.routingGeneration++
	if a.probeTokenValidLocked(token) {
		t.Fatal("stale routing accepted")
	}
	a.routingGate.RUnlock()
	a.engineMu.Unlock()
}

func TestAISelectorInvalidatesRequestFallbackNotOrdinaryHealth(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := &App{}
	token, ok := a.snapshotProbeToken()
	if !ok || !a.allowFallbackRetry(context.Background(), "unknown.example:443", token) {
		t.Fatal("current request token rejected")
	}
	a.engineMu.Lock()
	a.aiSelectionGeneration++
	a.routingGate.RLock()
	ordinary := a.probeTokenValidLocked(token)
	a.routingGate.RUnlock()
	a.engineMu.Unlock()
	if !ordinary {
		t.Fatal("AI change invalidated ordinary health")
	}
	if a.allowFallbackRetry(context.Background(), "unknown.example:443", token) {
		t.Fatal("stale request allowed fallback")
	}
}

func TestStaleEngineProbeCannotCommitHealthOrRecovery(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		t.Run(map[bool]string{false: "health", true: "recovery"}[recovery], func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			a := &App{log: log.New(io.Discard, "", 0), state: &State{}, stopCh: make(chan struct{}), mode: ModePrimary, primaryFails: 1, lastSwitch: time.Now().Add(-time.Hour), nextRecovery: time.Now().Add(-time.Second)}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				a.engineMu.Lock()
				a.engineGeneration++
				a.engineMu.Unlock()
				w.WriteHeader(204)
			}))
			defer server.Close()
			a.cfg = defaultConfig()
			a.cfg.PrimaryAddr = strings.TrimPrefix(server.URL, "http://")
			a.cfg.FallbackAddr = a.cfg.PrimaryAddr
			a.cfg.ProbeURL = "http://example.invalid/probe"
			a.cfg.RequireJapan = false
			a.cfg.ProbeTimeoutSec = 1
			before := a.nextRecovery
			if recovery {
				a.mode = ModeFallback
				a.attemptRecovery()
			} else {
				a.healthProbe()
			}
			want := ModePrimary
			if recovery {
				want = ModeFallback
			}
			if a.mode != want || a.primaryFails != 1 || !a.nextRecovery.Equal(before) {
				t.Fatalf("stale probe committed: mode=%s failures=%d retry=%v", a.mode, a.primaryFails, a.nextRecovery)
			}
		})
	}
}

func TestFailedRoutedProbeWakesNodeScan(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		t.Run(map[bool]string{false: "primary-health", true: "fallback-recovery"}[recovery], func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			if err := saveRoutingMode("rule"); err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
			defer srv.Close()
			a := &App{cfg: Config{PrimaryAddr: strings.TrimPrefix(srv.URL, "http://"), FallbackAddr: strings.TrimPrefix(srv.URL, "http://"), ProbeURL: "http://example.invalid/", ProbeTimeoutSec: 1, RecoveryInitSec: 1, RecoveryMaxSec: 4}, log: log.New(io.Discard, "", 0), state: &State{}, mode: ModePrimary, nodeScanWake: make(chan struct{}, 1), stopCh: make(chan struct{}), lastSwitch: time.Now().Add(-time.Hour), nextRecovery: time.Now().Add(-time.Second), recoveryDelay: time.Second}
			if recovery {
				a.mode = ModeFallback
				a.attemptRecovery()
			} else {
				a.healthProbe()
			}
			select {
			case <-a.nodeScanWake:
			case <-time.After(time.Second):
				t.Fatal("failed routed probe did not wake scanner")
			}
		})
	}
}
