package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHealthProbeTriesFreshAlternativeBeforeFallback(t *testing.T) {
	for _, tc := range []struct{ failAlternative, startup, nested bool }{{false, false, false}, {true, false, false}, {false, true, true}, {true, true, true}} {
		failAlternative := tc.failAlternative
		t.Run(fmt.Sprintf("alternativeFails=%v_startup=%v_nested=%v", failAlternative, tc.startup, tc.nested), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			if err := os.MkdirAll(appSupportDir(), 0700); err != nil {
				t.Fatal(err)
			}
			if err := saveRoutingMode("rule"); err != nil {
				t.Fatal(err)
			}
			var selected atomic.Value
			selected.Store("n1")
			if tc.nested {
				selected.Store("auto")
			}
			controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/configs":
					fmt.Fprint(w, `{"mode":"rule"}`)
				case r.Method == "GET" && r.URL.Path == "/proxies":
					_ = json.NewEncoder(w).Encode(proxySnapshot{Proxies: map[string]proxyInfo{"主代理": {Type: "Selector", Now: selected.Load().(string), All: []string{"n1", "n2", "auto"}}, "auto": {Type: "URLTest", Now: "n1", All: []string{"n1"}}, "JP-ALL": {Type: "URLTest", All: []string{"n1", "n2"}}, "n1": {Type: "Trojan"}, "n2": {Type: "Trojan"}}})
				case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/proxies/"):
					var data struct {
						Name string `json:"name"`
					}
					if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
						t.Error(err)
					}
					selected.Store(data.Name)
					w.WriteHeader(204)
				default:
					http.NotFound(w, r)
				}
			}))
			defer controller.Close()
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if selected.Load().(string) == "n2" && !failAlternative {
					w.WriteHeader(nodeProbeExpectedCode(r.URL.String()))
					return
				}
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					conn.Close()
				}
			}))
			defer primary.Close()
			var fallbackCalls atomic.Int32
			fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fallbackCalls.Add(1)
				w.WriteHeader(nodeProbeExpectedCode(r.URL.String()))
			}))
			defer fallback.Close()
			cfg := defaultConfig()
			cfg.Controller = strings.TrimPrefix(controller.URL, "http://")
			cfg.PrimaryAddr = strings.TrimPrefix(primary.URL, "http://")
			cfg.FallbackAddr = strings.TrimPrefix(fallback.URL, "http://")
			cfg.ProbeURL = "http://example.invalid/generate_204"
			cfg.ProbeTimeoutSec = 1
			cfg.RequireJapan = true // Legacy setting must not block healthy non-JP paths.
			cfg.Notify = false
			store, err := OpenStats(filepath.Join(t.TempDir(), "stats.json"))
			if err != nil {
				t.Fatal(err)
			}
			d := newNodeDiagnostics()
			now := time.Now()
			d.results["n2"] = map[string]nodeDiagnosticResult{"gstatic": {Status: "ok", DelayMS: 100, CheckedAt: now, ExpiresAt: now.Add(time.Minute)}}
			a := &App{cfg: cfg, log: log.New(io.Discard, "", 0), state: &State{}, mode: ModePrimary, primaryFails: 1, stopCh: make(chan struct{}), stats: &statsIntegration{store: store, diagnostics: d, logf: func(string, ...any) {}}}
			if tc.startup {
				a.mode = ModeProbing
				a.enterFallbackIfMode("startup failed", ModeProbing)
			} else {
				a.healthProbe()
			}
			got, _ := a.snapshotMode()
			if selected.Load().(string) != "n2" {
				t.Fatalf("alternative was not selected: %v", selected.Load())
			}
			if failAlternative {
				if got != ModeFallback || fallbackCalls.Load() == 0 {
					t.Fatalf("mode=%s fallback calls=%d", got, fallbackCalls.Load())
				}
			} else {
				if got != ModePrimary || fallbackCalls.Load() != 0 {
					t.Fatalf("mode=%s fallback calls=%d", got, fallbackCalls.Load())
				}
				b, err := os.ReadFile(filepath.Join(appSupportDir(), "node-selection.json"))
				if err != nil || !strings.Contains(string(b), "n2") {
					t.Fatalf("selection was not persisted: %v", err)
				}
			}
		})
	}
}

func TestFullPrimaryProbeCancellationAndStaleResult(t *testing.T) {
	for _, stop := range []bool{true, false} {
		t.Run(fmt.Sprintf("stop=%v", stop), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			if err := os.MkdirAll(appSupportDir(), 0700); err != nil {
				t.Fatal(err)
			}
			if err := saveRoutingMode("rule"); err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case entered <- struct{}{}:
				default:
				}
				select {
				case <-release:
					w.WriteHeader(nodeProbeExpectedCode(r.URL.String()))
				case <-r.Context().Done():
				}
			}))
			defer server.Close()
			cfg := defaultConfig()
			cfg.PrimaryAddr = strings.TrimPrefix(server.URL, "http://")
			cfg.ProbeURL = "http://example.invalid/generate_204"
			cfg.RequireJapan = false
			cfg.ProbeTimeoutSec = 5
			a := &App{cfg: cfg, stopCh: make(chan struct{}), probeMS: 123}
			done := make(chan bool, 1)
			go func() { ok, _ := a.probePrimaryFull(); done <- ok }()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("probe did not start")
			}
			if stop {
				close(a.stopCh)
			} else {
				a.engineMu.Lock()
				a.selectionGeneration++
				a.engineMu.Unlock()
				close(release)
			}
			select {
			case ok := <-done:
				if ok {
					t.Fatal("cancelled or stale probe succeeded")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("probe did not stop")
			}
			a.mu.Lock()
			ms := a.probeMS
			a.mu.Unlock()
			if ms != 123 {
				t.Fatalf("stale probe published latency: %d", ms)
			}
		})
	}
}

func TestEgressBudgetReservesReadbackTime(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct{ remaining, want time.Duration }{{9 * time.Second, 4 * time.Second}, {2 * time.Second, 1500 * time.Millisecond}, {400 * time.Millisecond, -100 * time.Millisecond}} {
		ctx, cancel := context.WithDeadline(context.Background(), now.Add(tc.remaining))
		got := egressTimeBudget(ctx, now)
		cancel()
		if got != tc.want {
			t.Fatalf("remaining %s: got %s want %s", tc.remaining, got, tc.want)
		}
	}
}
