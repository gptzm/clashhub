package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestFallbackTrafficThresholdAndDedup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := &App{cfg: Config{PrimaryAddr: "primary:1", FallbackAddr: "fallback:2"}, mode: ModePrimary, nodeScanWake: make(chan struct{}, 1), stopCh: make(chan struct{})}
	if a.fallbackTrafficCallback("primary:1") != nil || a.fallbackTrafficCallback("direct") != nil || a.fallbackTrafficCallback("fallback:2") == nil {
		t.Fatal("incorrect upstream attribution")
	}
	a.recordFallbackTraffic(fallbackTrafficThreshold - 1)
	select {
	case <-a.nodeScanWake:
		t.Fatal("early wake")
	default:
	}
	a.recordFallbackTraffic(1)
	select {
	case <-a.nodeScanWake:
	case <-time.After(time.Second):
		t.Fatal("missing wake")
	}
	for i := 0; i < 20; i++ {
		go a.recordFallbackTraffic(fallbackTrafficThreshold)
	}
	time.Sleep(50 * time.Millisecond)
	select {
	case <-a.nodeScanWake:
		t.Fatal("wake not rate limited")
	default:
	}
	close(a.stopCh)
}

func TestAIFailureScanIsGenerationBoundAndDeduplicated(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := saveRoutingMode("rule"); err != nil {
		t.Fatal(err)
	}
	a := &App{mode: ModePrimary, nodeScanWake: make(chan struct{}, 1)}
	_, token, err := a.snapshotRoutingState()
	if err != nil {
		t.Fatal(err)
	}

	a.scheduleAIFailureScan("api.openai.com:443", token)
	select {
	case <-a.nodeScanWake:
	case <-time.After(time.Second):
		t.Fatal("AI failure did not wake node scan")
	}
	// A burst after the wake was consumed still produces one scan only.
	a.scheduleAIFailureScan("api.openai.com:443", token)
	select {
	case <-a.nodeScanWake:
		t.Fatal("AI failure wake was not throttled")
	case <-time.After(30 * time.Millisecond):
	}
	// Non-AI destinations never trigger the AI health scan.
	a.scheduleAIFailureScan("example.com:443", token)
	select {
	case <-a.nodeScanWake:
		t.Fatal("non-AI failure woke node scan")
	default:
	}

	// The token captured by an old request cannot enqueue a wake after an
	// engine generation change; a new generation can signal independently.
	a.engineMu.Lock()
	a.engineGeneration++
	a.engineMu.Unlock()
	a.scheduleAIFailureScan("api.openai.com:443", token)
	select {
	case <-a.nodeScanWake:
		t.Fatal("stale generation woke node scan")
	default:
	}
	_, current, err := a.snapshotRoutingState()
	if err != nil {
		t.Fatal(err)
	}
	a.scheduleAIFailureScan("api.openai.com:443", current)
	select {
	case <-a.nodeScanWake:
	case <-time.After(time.Second):
		t.Fatal("new generation did not wake node scan")
	}
}

func TestFallbackDialDoesNotRetryPrimary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	calls := make(chan struct{}, 1)
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer primary.Close()
	a := &App{cfg: Config{PrimaryAddr: strings.TrimPrefix(primary.URL, "http://"), FallbackAddr: "127.0.0.1:0", PrimaryTimeoutMS: 10}, log: log.New(io.Discard, "", 0), mode: ModeFallback}
	c, _, err := a.fallbackDial(context.Background(), "example.test:443", 0)
	if c != nil {
		c.Close()
	}
	if err == nil {
		t.Fatal("expected fallback failure")
	}
	select {
	case <-calls:
		t.Fatal("fallback failure retried through primary")
	default:
	}
}

func TestRecoveryDoesNotMutateSelectors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	calls := make(chan string, 16)
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls <- r.Method + " " + r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer controller.Close()
	a := &App{
		cfg: Config{Controller: strings.TrimPrefix(controller.URL, "http://"), PrimaryAddr: "127.0.0.1:0", ProbeURL: "http://example.invalid/", ProbeTimeoutSec: 1, RecoveryInitSec: 1, RecoveryMaxSec: 4},
		log: log.New(io.Discard, "", 0), state: &State{}, mode: ModeFallback,
		stopCh: make(chan struct{}), lastSwitch: time.Now().Add(-time.Hour), nextRecovery: time.Now().Add(-time.Second), recoveryDelay: time.Second,
	}
	a.attemptRecovery()
	if mode, _ := a.snapshotMode(); mode != ModeFallback {
		t.Fatalf("failed probe changed mode to %s", mode)
	}
	select {
	case call := <-calls:
		t.Fatalf("recovery contacted selector controller: %s", call)
	default:
	}
}

func TestPrimaryProbeDoesNotOverwriteDualAIHealth(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "gstatic.test" {
			t.Errorf("unexpected AI request %s", r.URL)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	a := &App{cfg: Config{PrimaryAddr: strings.TrimPrefix(srv.URL, "http://"), ProbeURL: "http://gstatic.test/probe", AIProbeURL: "http://ai.test/probe", ProbeTimeoutSec: 1}, log: log.New(io.Discard, "", 0), state: &State{}, aiHealthy: false, aiMS: 33, aiCode: 401}
	if ok, reason := a.probePrimaryFull(); !ok {
		t.Fatal(reason)
	}
	if a.aiHealthy || a.aiMS != 33 || a.aiCode != 401 {
		t.Fatalf("AI health overwritten: %+v", a.state)
	}
}

func TestIndependentConfirmationDoesNotInventPrimaryOrAIHealth(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Host {
		case "gstatic.test":
			c, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				c.Close()
			}
		case "confirm.test":
			w.WriteHeader(http.StatusOK)
		default:
			// Every ordinary target is unreachable here; only the explicit
			// confirmation endpoint answers.
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	defer srv.Close()
	a := &App{cfg: Config{PrimaryAddr: strings.TrimPrefix(srv.URL, "http://"), ProbeURL: "http://gstatic.test/", ProbeTimeoutSec: 1}, stopCh: make(chan struct{}), log: log.New(io.Discard, "", 0), state: &State{}, aiHealthy: false}
	if ok, _ := a.probePrimaryFull(); ok {
		t.Fatal("EOF reported as success")
	}
	if ok, reason := a.confirmEngineFailureTarget("http://confirm.test/"); !ok || !strings.Contains(reason, "cloudflare") {
		t.Fatalf("confirmation: %v %s", ok, reason)
	}
	if a.aiHealthy {
		t.Fatal("confirmation invented AI health")
	}
	if ok, _ := a.confirmEngineFailureTarget(a.cfg.ProbeURL); ok {
		t.Fatal("same target confirmed itself")
	}
}

func TestFallbackInvalidatesAIHealth(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := &App{cfg: Config{RecoveryInitSec: 1}, log: log.New(io.Discard, "", 0), state: &State{}, mode: ModePrimary, aiHealthy: true, aiMS: 4, aiCode: 401}
	a.enterFallback("failure")
	st, err := readState()
	if err != nil || st.AIHealthy || st.AIMs != 0 || st.AICode != 0 {
		t.Fatalf("AI health not invalidated: %+v, %v", st, err)
	}
}

func TestPersistUnverifiedAIHealthIsUnknown(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := &App{log: log.New(io.Discard, "", 0), state: &State{AIHealthKnown: true}, mode: ModePrimary, aiHealthy: true, aiCode: 401, aiMS: 19}
	if err := a.persist(); err != nil {
		t.Fatal(err)
	}
	st, err := readState()
	if err != nil || st.AIHealthKnown || st.AIHealthy || st.AICode != 0 || st.AIMs != 0 {
		t.Fatalf("unverified state: %+v, %v", st, err)
	}
}

func TestEveryHealthRouteTransitionInvalidatesAI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := &App{log: log.New(io.Discard, "", 0), state: &State{}, mode: ModePrimary}
	for _, next := range []Mode{ModeFallback, ModePrimary, ModeProbing, ModePrimary} {
		a.mu.Lock()
		a.aiHealthy, a.aiMS, a.aiCode = true, 23, 401
		before := a.aiGeneration
		a.mu.Unlock()
		a.setMode(next, "test")
		st, err := readState()
		if err != nil || st.AIHealthy || st.AIMs != 0 || st.AICode != 0 || a.aiGeneration == before {
			t.Fatalf("transition %s: %+v %v", next, st, err)
		}
	}
}

func TestCleanupPersistsOff(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := &App{
		cfg:    Config{PrimaryAddr: "primary:1"},
		log:    log.New(io.Discard, "", 0),
		state:  &State{Active: true, Ready: true, PID: 42},
		mode:   ModePrimary,
		stopCh: make(chan struct{}),
	}
	a.requestStop()
	a.cleanup()
	a.setMode(ModeFallback, "late probe")
	if err := a.persist(); err != nil {
		t.Fatal(err)
	}
	st, err := readState()
	if err != nil {
		t.Fatal(err)
	}
	if st == nil || st.Mode != "off" || st.Upstream != "" || st.Active || st.Ready || st.PID != 0 {
		t.Fatalf("unexpected persisted state: %+v", st)
	}
}

func TestFallbackStateAndRecoveryBackoff(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := &App{
		cfg: Config{PrimaryAddr: "127.0.0.1:0", ProbeURL: "http://example.invalid/", AIProbeURL: "http://example.invalid/", ProbeTimeoutSec: 1, RecoveryInitSec: 2, RecoveryMaxSec: 5},
		log: log.New(io.Discard, "", 0), state: &State{}, mode: ModePrimary, stopCh: make(chan struct{}),
	}
	before := time.Now()
	a.enterFallback("primary unavailable")
	st, err := readState()
	if err != nil || st == nil {
		t.Fatalf("read state: %v", err)
	}
	if st.Mode != "fallback" || st.FallbackReason != "primary unavailable" || st.FallbackFailedAt.Before(before) || st.NextRetry.Sub(st.FallbackFailedAt) != 2*time.Second {
		t.Fatalf("invalid initial fallback state: %+v", st)
	}
	// A manual retry cannot bypass the initial cooldown.
	a.mu.Lock()
	a.nextRecovery = time.Now().Add(-time.Second)
	a.mu.Unlock()
	a.attemptRecovery()
	st, _ = readState()
	if !st.NextRetry.Equal(st.FallbackFailedAt.Add(2*time.Second)) || st.FallbackReason != "primary unavailable" {
		t.Fatalf("cooldown bypassed: %+v", st)
	}
	for _, delay := range []time.Duration{4 * time.Second, 5 * time.Second, 5 * time.Second} {
		a.mu.Lock()
		a.lastSwitch = time.Now().Add(-time.Hour)
		a.nextRecovery = time.Now().Add(-time.Second)
		a.mu.Unlock()
		a.attemptRecovery()
		st, err = readState()
		if err != nil || st == nil || st.Mode != "fallback" || st.FallbackReason == "primary unavailable" || st.NextRetry.Sub(st.FallbackFailedAt) != delay {
			t.Fatalf("invalid retry state (want %v): %+v, %v", delay, st, err)
		}
	}
	a.setMode(ModePrimary, "recovered")
	st, _ = readState()
	if st.FallbackReason != "" || !st.FallbackFailedAt.IsZero() || !st.NextRetry.IsZero() {
		t.Fatalf("stale fallback state: %+v", st)
	}
}

func TestStaleProbingTimeoutDoesNotOverridePrimary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := &App{log: log.New(io.Discard, "", 0), state: &State{}, mode: ModePrimary, stopCh: make(chan struct{})}
	a.enterFallbackIfMode("probe timeout", ModeProbing)
	mode, _ := a.snapshotMode()
	if mode != ModePrimary {
		t.Fatalf("stale timeout changed mode to %s", mode)
	}
}

func TestStartupSignalChild(t *testing.T) {
	if os.Getenv("SRHOP_SIGNAL_TEST_CHILD") != "1" {
		return
	}
	a := &App{log: log.New(io.Discard, "", 0), stopCh: make(chan struct{})}
	_, stop := a.watchSignals()
	defer stop()
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.stopCh:
		if !a.shuttingDown.Load() {
			t.Fatal("shutdown flag not set")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("termination signal did not interrupt startup")
	}
}

func TestStartupSignalIsolated(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestStartupSignalChild$")
	cmd.Env = append(os.Environ(), "SRHOP_SIGNAL_TEST_CHILD=1", "HOME="+t.TempDir())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
}
