package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNodeDiagnosticsSnapshotAndSecurity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(appSupportDir(), 0700); err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("a", 64)
	if err := os.WriteFile(filepath.Join(appSupportDir(), "api-token"), []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	d := newNodeDiagnostics()
	s := &statsIntegration{diagnostics: d}
	p := &nodeProber{stats: s}
	at := time.Now().Add(-3 * time.Minute)
	p.recordNodeProbe("node A", "gstatic", probeResult{delay: 42, at: at})
	p.recordNodeProbe("node A", "ai", probeResult{uncertain: true, at: time.Now()})
	p.recordNodeProbe("node A", "chatgpt", probeResult{failed: true, at: time.Now()})
	p.recordNodeProbe("node A", "openai", probeResult{delay: 21, at: time.Now()})
	request := func(auth, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://127.0.0.1:33335/api/node-health", nil)
		r.URL.Scheme = ""
		r.URL.Host = ""
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("Authorization", auth)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		s.serveHTTP(w, r)
		return w
	}
	if w := request("", ""); w.Code != 401 {
		t.Fatalf("missing token: %d", w.Code)
	}
	if w := request("Bearer "+token, "https://evil.invalid"); w.Code != 403 {
		t.Fatalf("origin: %d", w.Code)
	}
	w := request("Bearer "+token, "")
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	var data struct {
		Nodes []nodeDiagnosticRow `json:"nodes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Nodes) != 1 || !data.Nodes[0].Normal.Stale || data.Nodes[0].AI.Status != "error" || data.Nodes[0].ChatGPT == nil || data.Nodes[0].ChatGPT.Status != "failed" || data.Nodes[0].Targets["openai"].DelayMS != 21 {
		t.Fatalf("unexpected snapshot %+v", data.Nodes)
	}
	p.recordNodeProbe("node A", "gstatic", probeResult{failed: true, at: at.Add(-time.Second)})
	if d.results["node A"]["gstatic"].Status != "ok" {
		t.Fatal("older result overwrote newer")
	}
}
func TestNodeProbeSlotsBounded(t *testing.T) {
	releases := []func(){}
	for i := 0; i < nodeProbeConcurrency; i++ {
		release, ok := acquireSourceProbe(context.Background(), "source-a")
		if !ok {
			t.Fatal("acquire failed")
		}
		releases = append(releases, release)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, ok := acquireSourceProbe(ctx, "source-a")
	if ok {
		t.Fatal("exceeded limit")
	}
	other, ok := acquireSourceProbe(context.Background(), "source-b")
	if !ok {
		t.Fatal("different source blocked")
	}
	other()
	other() // release is safe to call more than once
	for _, release := range releases {
		release()
	}
	release, ok := acquireSourceProbe(context.Background(), "source-a")
	if !ok {
		t.Fatal("released slot was not reusable")
	}
	release()
	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if release, ok := acquireSourceProbe(cancelled, "source-a"); ok {
		release()
		t.Fatal("already cancelled context acquired a slot")
	}
	nodeProbeLimiter.Lock()
	remaining := len(nodeProbeLimiter.buckets)
	nodeProbeLimiter.Unlock()
	if remaining != 0 {
		t.Fatalf("limiter retained %d idle buckets", remaining)
	}
}
func TestManualDiagnosticScanNeverSelects(t *testing.T) {
	var puts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			puts.Add(1)
			w.WriteHeader(204)
			return
		}
		if r.URL.Path == "/proxies" {
			fmt.Fprint(w, `{"proxies":{"JP-ALL":{"type":"URLTest","all":["node"]},"node":{"type":"Shadowsocks"}}}`)
			return
		}
		fmt.Fprint(w, `{"delay":12}`)
	}))
	defer srv.Close()
	store, err := OpenStats(filepath.Join(t.TempDir(), "stats.json"))
	if err != nil {
		t.Fatal(err)
	}
	d := newNodeDiagnostics()
	s := &statsIntegration{store: store, diagnostics: d, logf: func(string, ...any) {}}
	a := &App{cfg: Config{Controller: strings.TrimPrefix(srv.URL, "http://"), ProbeURL: "https://example.com", AIProbeURL: "https://example.com/ai", ProbeTimeoutSec: 1}, stats: s, stopCh: make(chan struct{})}
	d.bind(a)
	r := httptest.NewRequest("POST", "http://localhost/api/node-health/probe", nil)
	w := httptest.NewRecorder()
	s.serveNodeDiagnostics(w, r)
	if w.Code != 202 {
		t.Fatalf("status %d", w.Code)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		d.mu.Lock()
		running := d.running
		result := d.results["node"]
		scanErr := d.err
		d.mu.Unlock()
		if !running {
			if scanErr != "" || result["gstatic"].Status != "ok" || result["openai"].Status != "ok" {
				t.Fatalf("results %v, error %q", result, scanErr)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("scan timed out")
		}
		time.Sleep(time.Millisecond)
	}
	if puts.Load() != 0 {
		t.Fatal("diagnostic scan changed selector")
	}
}

func TestScanReloadInvalidatesConcurrentResults(t *testing.T) {
	for _, mode := range []string{"periodic", "manual"} {
		t.Run(mode, func(t *testing.T) {
			blocked := make(chan struct{})
			release := make(chan struct{})
			var once atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/proxies" {
					fmt.Fprint(w, `{"proxies":{"JP-ALL":{"type":"URLTest","all":["node"]},"node":{"type":"Shadowsocks"}}}`)
					return
				}
				if strings.Contains(r.URL.Query().Get("url"), "api.openai.com") {
					if once.CompareAndSwap(false, true) {
						close(blocked)
					}
					<-release
				}
				fmt.Fprint(w, `{"delay":12}`)
			}))
			defer srv.Close()
			store, err := OpenStats(filepath.Join(t.TempDir(), "stats.json"))
			if err != nil {
				t.Fatal(err)
			}
			d := newNodeDiagnostics()
			s := &statsIntegration{store: store, diagnostics: d, logf: func(string, ...any) {}}
			a := &App{cfg: Config{Controller: strings.TrimPrefix(srv.URL, "http://"), ProbeURL: "https://example.com", AIProbeURL: "https://example.com/ai", ProbeTimeoutSec: 5, AITargets: []string{"openai"}}, stats: s, stopCh: make(chan struct{})}
			d.bind(a)
			done := make(chan error, 1)
			if mode == "manual" {
				go func() { d.scan(a); done <- nil }()
			} else {
				p, err := newNodeProber(a.cfg.Controller, "secret", a.cfg.ProbeURL, 5*time.Second, s)
				if err != nil {
					t.Fatal(err)
				}
				p.app = a
				p.aiURL = a.cfg.AIProbeURL
				go func() { done <- p.cycle(context.Background()) }()
			}
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			select {
			case <-blocked:
			case <-time.After(3 * time.Second):
				t.Fatal("AI probe did not start")
			}
			reloaded := make(chan struct{})
			reloadDone := make(chan struct{})
			go func() {
				a.engineMu.Lock()
				close(reloaded) // reload owns engineMu before the probe completes
				a.advanceEngineGenerationLocked()
				a.engineMu.Unlock()
				close(reloadDone)
			}()
			select {
			case <-reloaded:
			case <-time.After(3 * time.Second):
				t.Fatal("reload did not acquire engine lock")
			}
			close(release)
			select {
			case <-reloadDone:
			case <-time.After(3 * time.Second):
				t.Fatal("reload did not finish")
			}
			d.mu.Lock()
			count := len(d.results)
			d.mu.Unlock()
			if count != 0 {
				t.Fatal("reload did not invalidate snapshot")
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("scan did not finish")
			}
			for _, row := range store.QueryHealth48Hours() {
				if row.Name == nodeHealthName("node")+"-ai" {
					t.Fatal("old generation was written to health history")
				}
			}
			d.mu.Lock()
			defer d.mu.Unlock()
			if len(d.results) != 0 {
				t.Fatalf("old generation published after reload: %+v", d.results)
			}
			if mode == "manual" && d.err != errNodeDiagnosticReload.Error() {
				t.Fatalf("missing reload error: %q", d.err)
			}
		})
	}
}

func TestNodeDiagnosticPublicationGeneration(t *testing.T) {
	d := newNodeDiagnostics()
	s := &statsIntegration{diagnostics: d}
	a := &App{stats: s}
	p := &nodeProber{app: a, stats: s}
	a.engineMu.Lock()
	a.advanceEngineGenerationLocked()
	current := a.engineGeneration
	a.engineMu.Unlock()
	p.publishNodeProbe(current, "node", "gstatic", probeResult{delay: 9, at: time.Now()})
	p.publishNodeProbe(current-1, "node", "ai", probeResult{delay: 8, at: time.Now()})
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.results["node"]["gstatic"].DelayMS != 9 || len(d.results["node"]) != 1 {
		t.Fatalf("unexpected results: %+v", d.results)
	}
}

func TestNodeDiagnosticsMethods(t *testing.T) {
	d := newNodeDiagnostics()
	s := &statsIntegration{diagnostics: d}
	d.running = true
	d.app = &App{}
	r := httptest.NewRequest(http.MethodPost, "http://localhost/api/node-health/probe", nil)
	w := httptest.NewRecorder()
	s.serveNodeDiagnostics(w, r)
	if w.Code != 409 {
		t.Fatalf("got %d", w.Code)
	}
	r = httptest.NewRequest(http.MethodPost, "http://localhost/api/node-health", nil)
	w = httptest.NewRecorder()
	s.serveNodeDiagnostics(w, r)
	if w.Code != 405 {
		t.Fatalf("got %d", w.Code)
	}
}
