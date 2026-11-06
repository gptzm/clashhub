package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestIncrementalLowerSourceHealthyWhileHigherPending(t *testing.T) {
	writeSourcesEnvironment(t, []sourceDef{{ID: "slow-source", Label: "Slow", Type: "profile", Enabled: true}, {ID: "fast-source", Label: "Fast", Type: "profile", Enabled: true}}, map[string]string{"fast": "fast-source", "slow": "slow-source"}, "")
	slowStarted := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var mu sync.Mutex
	selected := "FALLBACK-LOCAL"
	puts := 0
	selectedFast := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/proxies":
			mu.Lock()
			defer mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"proxies": map[string]any{
				"JP-ALL": map[string]any{"type": "URLTest", "all": []string{"fast", "slow"}},
				"fast":   map[string]any{"type": "Http"}, "slow": map[string]any{"type": "Http"}, "FALLBACK-LOCAL": map[string]any{"type": "Http"},
				"主代理": map[string]any{"type": "Selector", "all": []string{"fast", "slow", "FALLBACK-LOCAL"}, "now": selected},
			}})
		case r.Method == http.MethodPut:
			var v map[string]string
			_ = json.NewDecoder(r.Body).Decode(&v)
			mu.Lock()
			selected = v["name"]
			puts++
			mu.Unlock()
			if v["name"] == "fast" {
				select {
				case selectedFast <- struct{}{}:
				default:
				}
			}
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/slow/delay"):
			close(slowStarted)
			<-release
			w.WriteHeader(http.StatusServiceUnavailable) // still uncertain
		case strings.HasSuffix(r.URL.Path, "/fast/delay"):
			<-slowStarted
			_, _ = io.WriteString(w, `{"delay":10}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer func() { unblock(); server.Close() }()
	p, err := newNodeProber(strings.TrimPrefix(server.URL, "http://"), "s", "http://example.invalid/probe", 10*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.selectionPath = filepath.Join(t.TempDir(), "selection")
	done := make(chan error, 1)
	go func() { done <- p.cycle(context.Background()) }()
	select {
	case <-selectedFast:
	case <-time.After(3 * time.Second):
		t.Fatal("fast node was not selected while slow node was blocked")
	}
	select {
	case err := <-done:
		t.Fatalf("cycle completed while slow probe blocked: %v", err)
	default:
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if selected != "fast" || puts != 1 {
		t.Fatalf("selection=%q PUTs=%d; want fast and one PUT", selected, puts)
	}
}

func TestIncrementalSelectsHealthyWhileActivePending(t *testing.T) {
	writeSourcesEnvironment(t, []sourceDef{{ID: "source", Label: "Source", Type: "profile", Enabled: true}}, map[string]string{"active": "source", "alternative": "source"}, "")
	started, release := make(chan struct{}), make(chan struct{})
	var startedOnce sync.Once
	// Simulate a stale source mapping: the whole pool must still be incremental.
	if err := os.Remove(nodeLabelsPath()); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var mu sync.Mutex
	selected := "active"
	writes := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/proxies":
			mu.Lock()
			defer mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"proxies": map[string]any{
				"JP-ALL": map[string]any{"type": "URLTest", "all": []string{"active", "alternative"}},
				"active": map[string]any{"type": "Http"}, "alternative": map[string]any{"type": "Http"},
				"主代理": map[string]any{"type": "Selector", "all": []string{"active", "alternative"}, "now": selected},
			}})
		case r.Method == http.MethodPut:
			var v map[string]string
			_ = json.NewDecoder(r.Body).Decode(&v)
			mu.Lock()
			selected = v["name"]
			mu.Unlock()
			writes <- v["name"]
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/active/delay"):
			startedOnce.Do(func() { close(started) })
			<-release
			w.WriteHeader(http.StatusUnauthorized) // controller uncertainty
		case strings.HasSuffix(r.URL.Path, "/alternative/delay"):
			<-started
			_, _ = io.WriteString(w, `{"delay":10}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer func() { unblock(); server.Close() }()
	p, err := newNodeProber(strings.TrimPrefix(server.URL, "http://"), "s", "http://example.invalid/probe", 5*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.selectionPath = filepath.Join(t.TempDir(), "selection")
	done := make(chan error, 1)
	go func() { done <- p.cycle(context.Background()) }()
	<-started
	// Give a healthy active node its bounded chance to complete.
	select {
	case name := <-writes:
		t.Fatalf("switched %q before active grace", name)
	case <-time.After(activeProbeGrace / 2):
	}
	select {
	case name := <-writes:
		if name != "alternative" {
			t.Fatalf("selected %q", name)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pending active blocked healthy alternative")
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPendingActiveSuccessfulWithinGraceRemainsSticky(t *testing.T) {
	writeSourcesEnvironment(t, []sourceDef{{ID: "source", Enabled: true}}, map[string]string{"active": "source", "alternative": "source"}, "")
	started, release := make(chan struct{}), make(chan struct{})
	var startedOnce sync.Once
	var mu sync.Mutex
	selected, puts := "active", 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/proxies" {
			mu.Lock()
			defer mu.Unlock()
			_ = json.NewEncoder(w).Encode(proxySnapshot{Proxies: map[string]proxyInfo{"JP-ALL": {Type: "URLTest", All: []string{"active", "alternative"}}, "active": {Type: "Http"}, "alternative": {Type: "Http"}, "主代理": {Type: "Selector", All: []string{"active", "alternative"}, Now: selected}}})
			return
		}
		if r.Method == http.MethodPut {
			mu.Lock()
			puts++
			mu.Unlock()
			w.WriteHeader(204)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/active/delay") {
			startedOnce.Do(func() { close(started) })
			<-release
			_, _ = io.WriteString(w, `{"delay":400}`)
			return
		}
		<-started
		_, _ = io.WriteString(w, `{"delay":20}`)
	}))
	defer srv.Close()
	p, err := newNodeProber(strings.TrimPrefix(srv.URL, "http://"), "s", "http://example.invalid", time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.selectionPath = filepath.Join(t.TempDir(), "selection")
	done := make(chan error, 1)
	go func() { done <- p.cycle(context.Background()) }()
	<-started
	time.Sleep(100 * time.Millisecond)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if puts != 0 {
		t.Fatalf("healthy sticky node switched %d times", puts)
	}
}

func TestIncrementalKeepsFirstHealthySlowNode(t *testing.T) {
	writeSourcesEnvironment(t, []sourceDef{{ID: "source", Label: "Source", Type: "profile", Enabled: true}}, map[string]string{"a": "source", "b": "source", "c": "source"}, "")
	releaseB, releaseC := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(releaseB); close(releaseC) }) }
	defer unblock()
	var mu sync.Mutex
	selected := "FALLBACK-LOCAL"
	puts := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/proxies" {
			mu.Lock()
			defer mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"proxies": map[string]any{
				"JP-ALL": map[string]any{"type": "URLTest", "all": []string{"a", "b", "c"}},
				"a":      map[string]any{"type": "Http"}, "b": map[string]any{"type": "Http"}, "c": map[string]any{"type": "Http"}, "FALLBACK-LOCAL": map[string]any{"type": "Http"},
				"主代理": map[string]any{"type": "Selector", "all": []string{"a", "b", "c", "FALLBACK-LOCAL"}, "now": selected},
			}})
			return
		}
		if r.Method == http.MethodPut {
			var v map[string]string
			_ = json.NewDecoder(r.Body).Decode(&v)
			mu.Lock()
			selected = v["name"]
			mu.Unlock()
			puts <- v["name"]
			w.WriteHeader(204)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/a/delay"):
			_, _ = io.WriteString(w, `{"delay":900}`)
		case strings.HasSuffix(r.URL.Path, "/b/delay"):
			<-releaseB
			_, _ = io.WriteString(w, `{"delay":800}`)
		case strings.HasSuffix(r.URL.Path, "/c/delay"):
			<-releaseC
			_, _ = io.WriteString(w, `{"delay":700}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer func() { unblock(); srv.Close() }()
	p, err := newNodeProber(strings.TrimPrefix(srv.URL, "http://"), "s", "http://example.invalid", 5*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.selectionPath = filepath.Join(t.TempDir(), "selection")
	done := make(chan error, 1)
	go func() { done <- p.cycle(context.Background()) }()
	select {
	case n := <-puts:
		if n != "a" {
			t.Fatalf("first selection %q", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("first result not selected")
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-puts:
		t.Fatalf("repeated same-source switch to %q", n)
	default:
	}
}

func TestController503DoesNotCauseFallback(t *testing.T) {
	writeSourcesEnvironment(t, []sourceDef{{ID: "source", Label: "Source", Type: "profile", Enabled: true}}, map[string]string{"a": "source", "b": "source"}, "")
	var mu sync.Mutex
	puts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/proxies" {
			_ = json.NewEncoder(w).Encode(map[string]any{"proxies": map[string]any{
				"JP-ALL": map[string]any{"type": "URLTest", "all": []string{"a", "b"}}, "a": map[string]any{"type": "Http"}, "b": map[string]any{"type": "Http"}, "FALLBACK-LOCAL": map[string]any{"type": "Http"},
				"主代理": map[string]any{"type": "Selector", "all": []string{"a", "b", "FALLBACK-LOCAL"}, "now": "a"},
			}})
			return
		}
		if r.Method == http.MethodPut {
			mu.Lock()
			puts++
			mu.Unlock()
			w.WriteHeader(204)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	p, err := newNodeProber(strings.TrimPrefix(srv.URL, "http://"), "s", "http://example.invalid", time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.selectionPath = filepath.Join(t.TempDir(), "selection")
	if err := p.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if puts != 0 {
		t.Fatalf("controller outage caused %d selector writes", puts)
	}
}

func TestHealthyOrdinaryScanRecoversBeforeBlockedAI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAI := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAI()
	var mu sync.Mutex
	selected := map[string]string{"主代理": "FALLBACK-LOCAL", "AI主代理": "FALLBACK-LOCAL"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/proxies" {
			mu.Lock()
			defer mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"proxies": map[string]any{"JP-ALL": map[string]any{"type": "URLTest", "all": []string{"n"}}, "n": map[string]any{"type": "Http"}, "FALLBACK-LOCAL": map[string]any{"type": "Http"}, "主代理": map[string]any{"type": "Selector", "all": []string{"n", "FALLBACK-LOCAL"}, "now": selected["主代理"]}, "AI主代理": map[string]any{"type": "Selector", "all": []string{"n", "FALLBACK-LOCAL"}, "now": selected["AI主代理"]}}})
			return
		}
		if r.Method == "PUT" {
			var v map[string]string
			json.NewDecoder(r.Body).Decode(&v)
			mu.Lock()
			selected[strings.TrimPrefix(r.URL.Path, "/proxies/")] = v["name"]
			mu.Unlock()
			w.WriteHeader(204)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/delay") {
			if strings.Contains(r.URL.Query().Get("url"), "/ai") {
				<-release
			}
			io.WriteString(w, `{"delay":10}`)
			return
		}
		w.WriteHeader(nodeProbeExpectedCode(r.URL.RequestURI()))
	}))
	defer func() { releaseAI(); server.Close() }()
	cfg := defaultConfig()
	cfg.PrimaryAddr = strings.TrimPrefix(server.URL, "http://")
	cfg.ProbeURL = "http://example.invalid/probe"
	cfg.RequireJapan = false
	cfg.ProbeTimeoutSec = 1
	cfg.RecoveryInitSec = 1
	a := &App{cfg: cfg, log: log.New(io.Discard, "", 0), state: &State{}, mode: ModeFallback, lastSwitch: time.Now().Add(-time.Hour), nextRecovery: time.Now().Add(time.Hour), stopCh: make(chan struct{})}
	p, _ := newNodeProber(cfg.PrimaryAddr, "s", cfg.ProbeURL, 5*time.Second, nil)
	p.aiURL = "http://example.invalid/ai"
	p.app = a
	p.selectionPath = filepath.Join(t.TempDir(), "selection")
	done := make(chan error, 1)
	go func() { done <- p.cycle(context.Background()) }()
	deadline := time.Now().Add(4 * time.Second)
	for {
		mode, _ := a.snapshotMode()
		if mode == ModePrimary {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ordinary health did not recover while AI blocked")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Do not leave the controller handler blocked during test cleanup.
	releaseAI()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
