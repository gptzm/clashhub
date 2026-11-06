package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestManualFallbackProtectionSimulation(t *testing.T) {
	for _, tc := range []struct {
		name, mode, main, ai, activeMain, activeAI, global string
		protect                                            bool
	}{
		{"main rule", "rule", "node", "", "node", "old", "主代理", true},
		{"ai rule", "rule", "", "node", "old", "node", "主代理", true},
		{"ai global ignored", "global", "", "node", "old", "node", "主代理", false},
		{"main global", "global", "node", "", "node", "old", "主代理", true},
		{"inactive", "rule", "node", "", "old", "old", "主代理", false},
		{"unavailable", "rule", "missing", "", "old", "old", "主代理", false},
		{"auto", "rule", "", "", "old", "old", "主代理", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			if err := os.MkdirAll(appSupportDir(), 0700); err != nil {
				t.Fatal(err)
			}
			if err := saveRoutingMode(tc.mode); err != nil {
				t.Fatal(err)
			}
			prefs := map[string]string{}
			if tc.main != "" {
				prefs["主代理"] = tc.main
			}
			if tc.ai != "" {
				prefs["AI主代理"] = tc.ai
			}
			if err := saveNodeSelection(manualNodePath(), prefs); err != nil {
				t.Fatal(err)
			}
			controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/configs" {
					fmt.Fprintf(w, `{"mode":%q}`, tc.mode)
					return
				}
				if r.URL.Path == "/proxies" {
					_ = json.NewEncoder(w).Encode(proxySnapshot{Proxies: map[string]proxyInfo{
						"主代理":    {Type: "Selector", Now: tc.activeMain, All: []string{"old", "node"}},
						"AI主代理":  {Type: "Selector", Now: tc.activeAI, All: []string{"old", "node"}},
						"GLOBAL": {Type: "Selector", Now: tc.global},
						"old":    {Type: "Trojan"}, "node": {Type: "Trojan"},
					}})
					return
				}
				http.NotFound(w, r)
			}))
			defer controller.Close()
			var fallbackCalls atomic.Int32
			fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fallbackCalls.Add(1)
				w.WriteHeader(nodeProbeExpectedCode(r.URL.RequestURI()))
			}))
			defer fallback.Close()
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(502) }))
			defer primary.Close()
			cfg := defaultConfig()
			cfg.Controller = strings.TrimPrefix(controller.URL, "http://")
			cfg.PrimaryAddr = strings.TrimPrefix(primary.URL, "http://")
			cfg.FallbackAddr = strings.TrimPrefix(fallback.URL, "http://")
			cfg.ProbeURL = "http://example.invalid/"
			cfg.RequireJapan = false
			cfg.ProbeTimeoutSec = 1
			cfg.PrimaryTimeoutMS = 1000
			cfg.Notify = false
			a := &App{cfg: cfg, log: log.New(io.Discard, "", 0), state: &State{}, mode: ModePrimary, primaryFails: 1, stopCh: make(chan struct{}), transports: map[string]*http.Transport{}}
			a.healthProbe()
			got, _ := a.snapshotMode()
			if (got == ModePrimary) != tc.protect {
				t.Fatalf("health mode %s protect %v", got, tc.protect)
			}
			if !tc.protect {
				return
			}
			before := fallbackCalls.Load()
			_, _, _ = a.dialForTarget(context.Background(), "example.invalid:443")
			if fallbackCalls.Load() != before {
				t.Fatal("CONNECT used fallback for protected selection")
			}
			a.setMode(ModeProbing, "test startup")
			a.enterFallbackIfMode("startup failed", ModeProbing)
			got, _ = a.snapshotMode()
			if got != ModePrimary {
				t.Fatalf("startup mode %s", got)
			}
		})
	}
}

func TestManualSelectionInvalidatesPendingFallbackRetry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(appSupportDir(), 0700); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release; w.WriteHeader(502) }))
	defer primary.Close()
	var fallbackCalls atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls.Add(1)
		w.WriteHeader(nodeProbeExpectedCode(r.URL.RequestURI()))
	}))
	defer fallback.Close()
	var selected atomic.Value
	selected.Store("old")
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/configs":
			fmt.Fprint(w, `{"mode":"rule"}`)
		case r.URL.Path == "/proxies" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(proxySnapshot{Proxies: map[string]proxyInfo{"主代理": {Type: "Selector", Now: selected.Load().(string), All: []string{"old", "node"}}, "old": {Type: "Trojan"}, "node": {Type: "Trojan"}}})
		case strings.HasPrefix(r.URL.Path, "/proxies/") && r.Method == http.MethodPut:
			selected.Store("node")
			w.WriteHeader(204)
		default:
			http.NotFound(w, r)
		}
	}))
	defer controller.Close()
	cfg := defaultConfig()
	cfg.Controller = strings.TrimPrefix(controller.URL, "http://")
	cfg.PrimaryAddr = strings.TrimPrefix(primary.URL, "http://")
	cfg.FallbackAddr = strings.TrimPrefix(fallback.URL, "http://")
	cfg.PrimaryTimeoutMS = 5000
	cfg.Notify = false
	a := &App{cfg: cfg, log: log.New(io.Discard, "", 0), state: &State{}, mode: ModePrimary, stopCh: make(chan struct{})}
	done := make(chan struct{})
	go func() { defer close(done); _, _, _ = a.dialForTarget(context.Background(), "example.invalid:443") }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("primary not reached")
	}
	r := httptest.NewRequest(http.MethodPut, "/api/nodes", strings.NewReader(`{"group":"主代理","mode":"manual","node":"node"}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.serveNodeSelection(w, r)
	close(release)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("request stuck")
	}
	if fallbackCalls.Load() != 0 {
		t.Fatal("stale request retried on fallback")
	}
}

func TestManualHTTPRetryProtection(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(appSupportDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := saveNodeSelection(manualNodePath(), map[string]string{"AI主代理": "node"}); err != nil {
		t.Fatal(err)
	}
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/configs" {
			fmt.Fprint(w, `{"mode":"rule"}`)
			return
		}
		if r.URL.Path == "/proxies" {
			_ = json.NewEncoder(w).Encode(proxySnapshot{Proxies: map[string]proxyInfo{"AI主代理": {Type: "Selector", Now: "node", All: []string{"node"}}, "node": {Type: "Trojan"}}})
			return
		}
		http.NotFound(w, r)
	}))
	defer controller.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { c, _, _ := w.(http.Hijacker).Hijack(); c.Close() }))
	defer bad.Close()
	var calls atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) }))
	defer fallback.Close()
	cfg := defaultConfig()
	cfg.Controller = strings.TrimPrefix(controller.URL, "http://")
	cfg.PrimaryAddr = strings.TrimPrefix(bad.URL, "http://")
	cfg.FallbackAddr = strings.TrimPrefix(fallback.URL, "http://")
	a := &App{cfg: cfg, mode: ModePrimary, log: log.New(io.Discard, "", 0), transports: map[string]*http.Transport{}}
	w := httptest.NewRecorder()
	a.handleHTTP(w, httptest.NewRequest(http.MethodGet, "http://example.test/", nil))
	if w.Code != 502 || calls.Load() != 0 {
		t.Fatalf("status %d fallback calls %d", w.Code, calls.Load())
	}
	if err := saveNodeSelection(manualNodePath(), map[string]string{}); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	a.handleHTTP(w, httptest.NewRequest(http.MethodGet, "http://example.test/", nil))
	if w.Code != 200 || calls.Load() != 1 {
		t.Fatalf("auto status %d fallback calls %d", w.Code, calls.Load())
	}
}

func TestManualRecoverySimulation(t *testing.T) {
	for _, tc := range []struct {
		name, group, mode, controllerMode, global, want string
		initial                                         Mode
	}{
		{"main rule", "主代理", "rule", "rule", "主代理", "switched_unverified_path", ModeFallback},
		{"ai rule", "AI主代理", "rule", "rule", "主代理", "switched_unverified_path", ModeFallback},
		{"main global", "主代理", "global", "global", "主代理", "switched_unverified_path", ModeFallback},
		{"ai global", "AI主代理", "global", "global", "主代理", "not_attempted", ModeFallback},
		{"global elsewhere", "主代理", "global", "global", "DIRECT", "not_attempted", ModeFallback},
		{"direct", "主代理", "direct", "direct", "主代理", "not_attempted", ModeFallback},
		{"mode mismatch", "主代理", "rule", "global", "主代理", "not_attempted", ModeFallback},
		{"already primary", "主代理", "rule", "rule", "主代理", "not_needed", ModePrimary},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			if err := os.MkdirAll(appSupportDir(), 0700); err != nil {
				t.Fatal(err)
			}
			if err := saveRoutingMode(tc.mode); err != nil {
				t.Fatal(err)
			}
			selected := "old"
			unexpected := 0
			controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/configs":
					fmt.Fprintf(w, `{"mode":%q}`, tc.controllerMode)
				case r.Method == http.MethodGet && r.URL.Path == "/proxies":
					_ = json.NewEncoder(w).Encode(proxySnapshot{Proxies: map[string]proxyInfo{
						tc.group: {Type: "Selector", Now: selected, All: []string{"old", "node"}},
						"GLOBAL": {Type: "Selector", Now: tc.global},
						"old":    {Type: "Trojan"}, "node": {Type: "Trojan"},
					}})
				case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/proxies/"):
					var body struct {
						Name string `json:"name"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					selected = body.Name
					w.WriteHeader(http.StatusNoContent)
				default:
					unexpected++
					http.Error(w, "unexpected probe", 500)
				}
			}))
			defer controller.Close()
			cfg := defaultConfig()
			cfg.Controller = strings.TrimPrefix(controller.URL, "http://")
			cfg.Notify = false
			a := &App{cfg: cfg, log: log.New(io.Discard, "", 0), state: &State{}, mode: tc.initial}
			oldToken, _ := a.snapshotProbeToken()
			// Simulate a long-running background recovery. Manual selection must not wait.
			a.transitionMu.Lock()
			request := httptest.NewRequest(http.MethodPut, "/api/nodes", bytes.NewBufferString(fmt.Sprintf(`{"group":%q,"mode":"manual","node":"node"}`, tc.group)))
			request.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			a.serveNodeSelection(w, request)
			a.transitionMu.Unlock()
			var result nodeGroupStatus
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if w.Code != 200 || result.Mode != "manual" || result.Selected != "node" || result.RecoveryStatus != tc.want {
				t.Fatalf("status %d result %+v", w.Code, result)
			}
			prefs, err := readManualNodes()
			if err != nil || prefs[tc.group] != "node" {
				t.Fatalf("preferences %v: %v", prefs, err)
			}
			got, _ := a.snapshotMode()
			wantMode := tc.initial
			if tc.want == "switched_unverified_path" {
				wantMode = ModePrimary
			}
			if got != wantMode {
				t.Fatalf("mode %s want %s", got, wantMode)
			}
			if unexpected != 0 {
				t.Fatalf("unexpected controller requests: %d", unexpected)
			}
			a.engineMu.Lock()
			a.routingGate.RLock()
			valid := a.probeTokenValidLocked(oldToken)
			a.routingGate.RUnlock()
			a.engineMu.Unlock()
			if valid {
				t.Fatal("old probe token remained valid")
			}
		})
	}
}
