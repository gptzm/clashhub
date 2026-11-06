package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNodeHealthCycle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var active, max atomic.Int32
	var count atomic.Int32
	secret := "sensitive-secret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("missing auth")
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/proxies" {
			proxies := map[string]any{"FALLBACK-LOCAL": map[string]any{"type": "Http"}, "JP-ALL": map[string]any{"type": "URLTest", "all": []string{"node/one", "bad", "nested", "FALLBACK-LOCAL", "n1", "n2", "n3", "n4", "n5"}}, "BACKUP-AUTO": map[string]any{"type": "URLTest", "all": []string{"bad"}}, "nested": map[string]any{"type": "Selector", "all": []string{"n1"}}}
			for _, n := range []string{"node/one", "bad", "n1", "n2", "n3", "n4", "n5"} {
				proxies[n] = map[string]any{"type": "Shadowsocks"}
			}
			json.NewEncoder(w).Encode(map[string]any{"proxies": proxies})
			return
		}
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := max.Load()
			if n <= old || max.CompareAndSwap(old, n) {
				break
			}
		}
		count.Add(1)
		// The ordinary probe carries the configured URL; AI checks carry their
		// own targets and independent fallback attempts use a longer budget.
		if q := r.URL.Query().Get("timeout"); q != "100" && q != "2000" {
			t.Error("incorrect query")
		}
		time.Sleep(15 * time.Millisecond)
		if strings.Contains(r.URL.EscapedPath(), "bad/delay") {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"message":"An error occurred in the delay test"}`))
			return
		}
		if strings.Contains(r.URL.EscapedPath(), "node%2Fone/delay") {
			fmt.Fprint(w, `{"delay":42}`)
			return
		}
		fmt.Fprint(w, `{"delay":10}`)
	}))
	defer srv.Close()
	store, err := OpenStats(filepath.Join(t.TempDir(), "stats.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := &statsIntegration{store: store, logf: func(string, ...any) { t.Error("unexpected stats error") }}
	p, err := newNodeProber(strings.TrimPrefix(srv.URL, "http://"), secret, "https://example.com/test?a=1", 100*time.Millisecond, s)
	if err != nil {
		t.Fatal(err)
	}
	// No app and no explicit targets: only the ordinary route runs.
	if err := p.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 7 JP nodes (1 ordinary + 2 openai checks) plus the AI route also probing
	// FALLBACK-LOCAL (2) and the failing node retrying 2 independent targets:
	// 7*3 + 2 + 1 = 24 requests.
	if count.Load() != 24 || max.Load() > nodeProbeConcurrency {
		t.Fatalf("count=%d max=%d", count.Load(), max.Load())
	}
	hours := store.QueryHealth48Hours()
	if len(hours) < 7 {
		t.Fatalf("hours=%+v", hours)
	}
	for _, h := range hours {
		// The 503 body marks a node-side delay-test failure; it is recorded.
		if !strings.HasPrefix(h.Name, "node-") || strings.Contains(h.Name, secret) {
			t.Fatal("unsafe label")
		}
	}
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStats(store.path)
	if err != nil || len(reopened.QueryHealth48Hours()) < 7 {
		t.Fatalf("persist: %v", err)
	}
}

func TestNodeHealthAutomaticSelectionSkipsDirectRouting(t *testing.T) {
	for _, tc := range []struct {
		name      string
		startMode string
		flip      bool
	}{
		{name: "direct at scan start", startMode: "direct"},
		{name: "switches to direct before commit", startMode: "rule", flip: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			if err := saveRoutingMode(tc.startMode); err != nil {
				t.Fatal(err)
			}
			var puts atomic.Int32
			started := make(chan struct{})
			release := make(chan struct{})
			var startOnce sync.Once
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/proxies" {
					fmt.Fprint(w, `{"proxies":{"JP-ALL":{"type":"URLTest","all":["n1"]},"主代理":{"type":"Selector","all":["n1","FALLBACK-LOCAL"],"now":"FALLBACK-LOCAL"},"n1":{"type":"Http"},"FALLBACK-LOCAL":{"type":"Http"}}}`)
					return
				}
				if r.Method == http.MethodPut {
					puts.Add(1)
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/delay") {
					if tc.flip {
						startOnce.Do(func() { close(started) })
						<-release
					}
					fmt.Fprint(w, `{"delay":100}`)
					return
				}
				http.NotFound(w, r)
			}))
			defer srv.Close()

			p, err := newNodeProber(strings.TrimPrefix(srv.URL, "http://"), "s", "https://example.com", time.Second, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer p.client.CloseIdleConnections()
			p.selectionPath = filepath.Join(t.TempDir(), "selection")
			p.app = &App{engineGeneration: 1, stopCh: make(chan struct{})}
			done := make(chan error, 1)
			go func() { done <- p.cycle(context.Background()) }()
			if tc.flip {
				select {
				case <-started:
				case <-time.After(2 * time.Second):
					t.Fatal("node probe did not start")
				}
				if err := saveRoutingMode("direct"); err != nil {
					t.Fatal(err)
				}
				p.app.routingGate.Lock()
				p.app.routingGeneration++
				p.app.routingGate.Unlock()
				close(release)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if got := puts.Load(); got != 0 {
				t.Fatalf("automatic selector PUTs while direct routing was active: %d", got)
			}
		})
	}
}

func TestNodeProbeTimeoutAndCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	store, _ := OpenStats(filepath.Join(t.TempDir(), "stats.json"))
	p, err := newNodeProber(strings.TrimPrefix(srv.URL, "http://"), "secret", "https://example.com", 20*time.Millisecond, &statsIntegration{store: store, logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	p.probe(context.Background(), "timeout", "gstatic", "https://example.com", 204)
	rows := store.QueryHealth48Hours()
	if len(rows) != 0 {
		t.Fatalf("controller timeout treated as node failure: %+v", rows)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.probe(ctx, "cancelled", "gstatic", "https://example.com", 204)
	if len(store.QueryHealth48Hours()) != 0 {
		t.Fatal("shutdown cancellation recorded")
	}
}

func TestNodeProbeAllowsControllerCommunicationMargin(t *testing.T) {
	const probeTimeout = 100 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("timeout") != "100" {
			t.Error("controller must receive node timeout without margin")
		}
		time.Sleep(180 * time.Millisecond)
		fmt.Fprint(w, `{"delay":90}`)
	}))
	defer srv.Close()
	store, err := OpenStats(filepath.Join(t.TempDir(), "stats.json"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := newNodeProber(strings.TrimPrefix(srv.URL, "http://"), "secret", "https://example.com", probeTimeout, &statsIntegration{store: store, logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	result := p.probe(context.Background(), "slow-response", "gstatic", "https://example.com", 204)
	p.publishProbe(0, "slow-response", "gstatic", result)
	rows := store.QueryHealth48Hours()
	if len(rows) != 1 || rows[0].Successes != 1 {
		t.Fatalf("controller response incorrectly timed out: %+v", rows)
	}
}

func TestNodeProberRejectsUnsafeControllerAndURL(t *testing.T) {
	for _, controller := range []string{"example.com:80", "0.0.0.0:80", "localhost:80"} {
		if _, err := newNodeProber(controller, "s", "https://example.com", time.Second, nil); err == nil {
			t.Fatalf("accepted %s", controller)
		}
	}
	if _, err := newNodeProber("127.0.0.1:1", "s", "https://user:pass@example.com", time.Second, nil); err == nil {
		t.Fatal("accepted credentials")
	}
	if url.PathEscape("node/one") != "node%2Fone" {
		t.Fatal("unexpected escaping")
	}
}

func TestStickySelectionIndependentAndFailureKeepsFallbackLast(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate config.json/node-labels reads
	var mu sync.Mutex
	now := map[string]string{"主代理": "FALLBACK-LOCAL", "AI主代理": "FALLBACK-LOCAL"}
	selectorPuts := map[string]int{}
	delays := map[string]map[string]int{"n1": {"gstatic": 400, "ai": 800}, "n2": {"gstatic": 100, "ai": 200}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/proxies" {
			json.NewEncoder(w).Encode(map[string]any{"proxies": map[string]any{
				"FALLBACK-LOCAL": map[string]any{"type": "Http"}, "JP-ALL": map[string]any{"type": "URLTest", "all": []string{"n1", "n2"}},
				"主代理":   map[string]any{"type": "Selector", "all": []string{"FALLBACK-LOCAL", "n1", "n2"}, "now": now["主代理"]},
				"AI主代理": map[string]any{"type": "Selector", "all": []string{"FALLBACK-LOCAL", "n1", "n2"}, "now": now["AI主代理"]},
				"n1":    map[string]any{"type": "Http"}, "n2": map[string]any{"type": "Http"},
			}})
			return
		}
		if r.Method == http.MethodPut {
			var v map[string]string
			json.NewDecoder(r.Body).Decode(&v)
			group := strings.TrimPrefix(r.URL.Path, "/proxies/")
			now[group] = v["name"]
			selectorPuts[group]++
			w.WriteHeader(204)
			return
		}
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/proxies/"), "/delay")
		kind := "gstatic"
		if r.URL.Query().Get("expected") == "401" {
			kind = "ai"
		}
		delay := delays[name][kind]
		if r.URL.Query().Get("url") == chatGPTProbeURL {
			delay = 100
		}
		if delay < 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"message":"An error occurred in the delay test"}`)
			return
		}
		if delay == 0 {
			w.WriteHeader(401)
			return
		}
		fmt.Fprintf(w, `{"delay":%d}`, delay)
	}))
	defer server.Close()
	p, err := newNodeProber(strings.TrimPrefix(server.URL, "http://"), "s", "https://example.com", time.Second, nil)
	p.aiTargets = []string{"openai"}
	if err != nil {
		t.Fatal(err)
	}
	p.aiURL = "https://example.com/ai"
	p.selectionPath = filepath.Join(t.TempDir(), "selection.json")
	if err := saveNodeSelection(p.selectionPath, map[string]string{"主代理": "n1", "AI主代理": "n1"}); err != nil {
		t.Fatal(err)
	}
	if err := p.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if now["主代理"] != "n1" || now["AI主代理"] != "n2" {
		t.Fatalf("sticky decisions: %v", now)
	}
	delays["n1"]["gstatic"] = -1
	delays["n2"]["gstatic"] = -1
	delays["n2"]["ai"] = 0
	if err := p.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Ordinary node failure is fallback-last: the node-health scan must not
	// select an unverified external fallback; the outer routing policy owns it.
	if now["主代理"] != "n1" || now["AI主代理"] != "n1" {
		t.Fatalf("failure decisions: %v", now)
	}
	if selectorPuts["主代理"] != 1 {
		t.Fatalf("ordinary selector PUTs = %d, want only the initial node selection", selectorPuts["主代理"])
	}
	delays["n1"]["gstatic"] = 300
	delays["n2"]["gstatic"] = 100
	if err := p.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if now["主代理"] != "n1" {
		t.Fatalf("last node not restored: %v", now)
	}
	if info, err := os.Stat(p.selectionPath); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("selection permissions: %v %v", info, err)
	}
}

func TestCurrentSelectorBeatsSavedSelection(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate config.json/node-labels reads
	var selected string
	var selectedMu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/proxies" {
			json.NewEncoder(w).Encode(map[string]any{"proxies": map[string]any{
				"FALLBACK-LOCAL": map[string]any{"type": "Http"}, "JP-ALL": map[string]any{"type": "URLTest", "all": []string{"n1", "n2"}}, "主代理": map[string]any{"type": "Selector", "all": []string{"n1", "n2", "FALLBACK-LOCAL"}, "now": "n2"}, "n1": map[string]any{"type": "Http"}, "n2": map[string]any{"type": "Http"},
			}})
			return
		}
		if r.Method == "PUT" {
			var v map[string]string
			json.NewDecoder(r.Body).Decode(&v)
			selectedMu.Lock()
			selected = v["name"]
			selectedMu.Unlock()
			w.WriteHeader(204)
			return
		}
		fmt.Fprint(w, `{"delay":400}`)
	}))
	defer srv.Close()
	p, _ := newNodeProber(strings.TrimPrefix(srv.URL, "http://"), "s", "https://example.com", time.Second, nil)
	p.selectionPath = filepath.Join(t.TempDir(), "selection")
	saveNodeSelection(p.selectionPath, map[string]string{"主代理": "n1"})
	if err := p.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if selected != "" {
		t.Fatalf("overwrote current with %s", selected)
	}
	b, _ := os.ReadFile(p.selectionPath)
	var saved map[string]string
	json.Unmarshal(b, &saved)
	if saved["主代理"] != "n2" {
		t.Fatal(saved)
	}
}

func TestReloadDuringScanDiscardsDecision(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate config.json/node-labels reads
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var writes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/proxies" {
			json.NewEncoder(w).Encode(map[string]any{"proxies": map[string]any{"FALLBACK-LOCAL": map[string]any{"type": "Http"}, "JP-ALL": map[string]any{"type": "URLTest", "all": []string{"n1"}}, "主代理": map[string]any{"type": "Selector", "all": []string{"n1", "FALLBACK-LOCAL"}, "now": "FALLBACK-LOCAL"}, "n1": map[string]any{"type": "Http"}}})
			return
		}
		if r.Method == "PUT" {
			writes.Add(1)
			w.WriteHeader(204)
			return
		}
		entered <- struct{}{}
		<-release
		fmt.Fprint(w, `{"delay":100}`)
	}))
	defer srv.Close()
	p, _ := newNodeProber(strings.TrimPrefix(srv.URL, "http://"), "s", "https://example.com", time.Second, nil)
	p.selectionPath = filepath.Join(t.TempDir(), "selection")
	p.app = &App{}
	done := make(chan error, 1)
	go func() { done <- p.cycle(context.Background()) }()
	<-entered
	p.app.engineMu.Lock()
	p.app.engineGeneration++
	p.app.engineMu.Unlock()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if writes.Load() != 0 {
		t.Fatal("stale scan changed selection")
	}
}

func TestExpiredResultsDoNotBlockFreshCandidate(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate config.json/node-labels reads
	old := nodeResultMaxAge
	// The selection loop coalesces completed probes for 25ms. Leave enough
	// headroom for CI scheduling so the final result remains fresh while the
	// earlier batch is reliably stale when it is evaluated.
	nodeResultMaxAge = 500 * time.Millisecond
	defer func() { nodeResultMaxAge = old }()
	var selected string
	var selectedMu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/proxies" {
			selectedMu.Lock()
			defer selectedMu.Unlock()
			now := "FALLBACK-LOCAL"
			if selected != "" {
				now = selected
			}
			json.NewEncoder(w).Encode(map[string]any{"proxies": map[string]any{"FALLBACK-LOCAL": map[string]any{"type": "Http"}, "JP-ALL": map[string]any{"type": "URLTest", "all": []string{"a", "b", "c", "d", "z"}}, "主代理": map[string]any{"type": "Selector", "all": []string{"a", "b", "c", "d", "z", "FALLBACK-LOCAL"}, "now": now}, "a": map[string]any{"type": "Http"}, "b": map[string]any{"type": "Http"}, "c": map[string]any{"type": "Http"}, "d": map[string]any{"type": "Http"}, "z": map[string]any{"type": "Http"}}})
			return
		}
		if r.Method == "PUT" {
			var v map[string]string
			json.NewDecoder(r.Body).Decode(&v)
			selectedMu.Lock()
			selected = v["name"]
			selectedMu.Unlock()
			w.WriteHeader(204)
			return
		}
		if strings.Contains(r.URL.Path, "/z/") {
			time.Sleep(1100 * time.Millisecond)
		}
		fmt.Fprint(w, `{"delay":100}`)
	}))
	defer srv.Close()
	p, _ := newNodeProber(strings.TrimPrefix(srv.URL, "http://"), "s", "https://example.com", time.Second, nil)
	p.selectionPath = filepath.Join(t.TempDir(), "s")
	if err := p.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if selected != "z" {
		t.Fatalf("fresh candidate not selected: %s", selected)
	}
}

func TestAISelectionDoesNotWaitForGeneralScan(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate config.json/node-labels reads
	generalStarted := make(chan struct{}, 1)
	release := make(chan struct{})
	aiChosen := make(chan struct{}, 1)
	var mu sync.Mutex
	now := map[string]string{"主代理": "FALLBACK-LOCAL", "AI主代理": "FALLBACK-LOCAL"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/proxies" {
			mu.Lock()
			defer mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"proxies": map[string]any{"FALLBACK-LOCAL": map[string]any{"type": "Http"}, "JP-ALL": map[string]any{"type": "URLTest", "all": []string{"n1"}}, "主代理": map[string]any{"type": "Selector", "all": []string{"n1", "FALLBACK-LOCAL"}, "now": now["主代理"]}, "AI主代理": map[string]any{"type": "Selector", "all": []string{"n1", "FALLBACK-LOCAL"}, "now": now["AI主代理"]}, "n1": map[string]any{"type": "Http"}}})
			return
		}
		if r.Method == "PUT" {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			now[strings.TrimPrefix(r.URL.Path, "/proxies/")] = body["name"]
			mu.Unlock()
			if strings.Contains(r.URL.Path, "AI主代理") {
				aiChosen <- struct{}{}
			}
			w.WriteHeader(204)
			return
		}
		if r.URL.Query().Get("url") == chatGPTProbeURL {
			fmt.Fprint(w, `{"delay":100}`)
			return
		}
		if r.URL.Query().Get("expected") == "200" {
			generalStarted <- struct{}{}
			<-release
		}
		fmt.Fprint(w, `{"delay":100}`)
	}))
	defer srv.Close()
	p, _ := newNodeProber(strings.TrimPrefix(srv.URL, "http://"), "s", "https://example.com", time.Second, nil)
	p.aiTargets = []string{"openai"}
	p.aiURL = "https://example.com/ai"
	p.selectionPath = filepath.Join(t.TempDir(), "s")
	done := make(chan error, 1)
	go func() { done <- p.cycle(context.Background()) }()
	<-generalStarted
	select {
	case <-aiChosen:
	case <-time.After(time.Second):
		close(release)
		<-done
		t.Fatal("AI waited for general scan")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapCurrentNodePreserved(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate config.json/node-labels reads
	var mu sync.Mutex
	now := map[string]string{"主代理": "BOOT-JP-ALL", "AI主代理": "BOOT-AI-JP-ALL"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/proxies" {
			json.NewEncoder(w).Encode(map[string]any{"proxies": map[string]any{
				"主代理":            map[string]any{"type": "Selector", "all": []string{"BOOT-JP-ALL", "n1", "n2", "FALLBACK-LOCAL"}, "now": now["主代理"]},
				"AI主代理":          map[string]any{"type": "Selector", "all": []string{"BOOT-AI-JP-ALL", "n1", "n2", "FALLBACK-LOCAL"}, "now": now["AI主代理"]},
				"BOOT-JP-ALL":    map[string]any{"type": "Fallback", "all": []string{"JP-ALL", "FALLBACK-LOCAL"}, "now": "JP-ALL"},
				"BOOT-AI-JP-ALL": map[string]any{"type": "Fallback", "all": []string{"AI-JP-ALL", "FALLBACK-LOCAL"}, "now": "AI-JP-ALL"},
				"JP-ALL":         map[string]any{"type": "URLTest", "all": []string{"n1", "n2"}, "now": "n1"},
				"AI-JP-ALL":      map[string]any{"type": "URLTest", "all": []string{"n1", "n2"}, "now": "n2"},
				"n1":             map[string]any{"type": "Http"}, "n2": map[string]any{"type": "Http"}, "FALLBACK-LOCAL": map[string]any{"type": "Http"},
			}})
			return
		}
		if r.Method == http.MethodPut {
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			now[strings.TrimPrefix(r.URL.Path, "/proxies/")] = body["name"]
			w.WriteHeader(204)
			return
		}
		n := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/proxies/"), "/delay")
		ai := r.URL.Query().Get("expected") == "401"
		if (n == "n1" && !ai) || (n == "n2" && ai) {
			fmt.Fprint(w, `{"delay":400}`)
		} else {
			fmt.Fprint(w, `{"delay":100}`)
		}
	}))
	defer srv.Close()
	p, _ := newNodeProber(strings.TrimPrefix(srv.URL, "http://"), "s", "https://example.com", time.Second, nil)
	p.aiTargets = []string{"openai"}
	p.aiURL = "https://example.com/ai"
	p.selectionPath = filepath.Join(t.TempDir(), "s")
	if err := p.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if now["主代理"] != "n1" || now["AI主代理"] != "n2" {
		t.Fatalf("bootstrap active nodes not retained: %v", now)
	}
}

func TestSlowSweepExpiredFailuresRemainUnknown(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate config.json/node-labels reads
	old := nodeResultMaxAge
	nodeResultMaxAge = 35 * time.Millisecond
	defer func() { nodeResultMaxAge = old }()
	var selected string
	var selectedMu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/proxies" {
			selectedMu.Lock()
			defer selectedMu.Unlock()
			now := "a"
			if selected != "" {
				now = selected
			}
			json.NewEncoder(w).Encode(map[string]any{"proxies": map[string]any{
				"JP-ALL": map[string]any{"type": "URLTest", "all": []string{"a", "b", "c", "d", "z"}}, "主代理": map[string]any{"type": "Selector", "all": []string{"a", "b", "c", "d", "z", "FALLBACK-LOCAL"}, "now": now}, "a": map[string]any{"type": "Http"}, "b": map[string]any{"type": "Http"}, "c": map[string]any{"type": "Http"}, "d": map[string]any{"type": "Http"}, "z": map[string]any{"type": "Http"}, "FALLBACK-LOCAL": map[string]any{"type": "Http"},
			}})
			return
		}
		if r.Method == http.MethodPut {
			var v map[string]string
			json.NewDecoder(r.Body).Decode(&v)
			selectedMu.Lock()
			selected = v["name"]
			selectedMu.Unlock()
			w.WriteHeader(204)
			return
		}
		if strings.Contains(r.URL.Path, "/z/") {
			time.Sleep(80 * time.Millisecond)
		}
		w.WriteHeader(503)
	}))
	defer srv.Close()
	p, _ := newNodeProber(strings.TrimPrefix(srv.URL, "http://"), "s", "https://example.com", time.Second, nil)
	p.selectionPath = filepath.Join(t.TempDir(), "s")
	if err := p.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if selected != "" {
		t.Fatalf("expired failure evidence must not trigger fallback: %q", selected)
	}
}

func TestEmptyJPCandidatesNeverFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate config.json/node-labels reads
	for _, jp := range [][]string{nil, {"other"}} {
		t.Run(fmt.Sprint(jp), func(t *testing.T) {
			var puts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					puts.Add(1)
					w.WriteHeader(204)
					return
				}
				if r.URL.Path == "/proxies" {
					json.NewEncoder(w).Encode(map[string]any{"proxies": map[string]any{"JP-ALL": map[string]any{"type": "URLTest", "all": jp}, "主代理": map[string]any{"type": "Selector", "all": []string{"active", "FALLBACK-LOCAL"}, "now": "active"}, "active": map[string]any{"type": "Http"}, "other": map[string]any{"type": "Http"}, "FALLBACK-LOCAL": map[string]any{"type": "Http"}}})
					return
				}
				w.WriteHeader(503)
			}))
			defer srv.Close()
			p, _ := newNodeProber(strings.TrimPrefix(srv.URL, "http://"), "s", "https://example.com", time.Second, nil)
			p.selectionPath = filepath.Join(t.TempDir(), "selection")
			if err := p.cycle(context.Background()); err != nil {
				t.Fatal(err)
			}
			if puts.Load() != 0 {
				t.Fatal("empty candidate set triggered fallback")
			}
		})
	}
}

func TestSelectorReadbackRequired(t *testing.T) {
	for _, mode := range []string{"mismatch", "error"} {
		t.Run(mode, func(t *testing.T) {
			var reads atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/proxies" {
					n := reads.Add(1)
					if mode == "error" && n >= 4 {
						w.WriteHeader(503)
						return
					}
					fmt.Fprint(w, `{"proxies":{"JP-ALL":{"type":"URLTest","all":["node"]},"AI主代理":{"type":"Selector","all":["node","FALLBACK-LOCAL"],"now":"FALLBACK-LOCAL"},"node":{"type":"Http"},"FALLBACK-LOCAL":{"type":"Http"}}}`)
					return
				}
				if r.Method == http.MethodPut {
					w.WriteHeader(204)
					return
				}
				fmt.Fprint(w, `{"delay":12}`)
			}))
			defer srv.Close()
			t.Setenv("HOME", t.TempDir())
			p, _ := newNodeProber(strings.TrimPrefix(srv.URL, "http://"), "s", "https://example.com", time.Second, nil)
			p.aiURL = "https://example.com/ai"
			p.selectionPath = filepath.Join(t.TempDir(), "selection")
			p.app = &App{aiMS: 777, aiHealthy: false}
			if err := p.cycle(context.Background()); err == nil {
				t.Fatal("unverified selector accepted")
			}
			if p.app.aiMS != 0 || p.app.aiCode != 0 || p.app.aiHealthy {
				t.Fatal("unverified AI status published")
			}
			if _, err := os.Stat(p.selectionPath); !os.IsNotExist(err) {
				t.Fatal("unverified selection persisted")
			}
		})
	}
}

func TestResolveActiveNodeRejectsCyclesAndInvalidEdges(t *testing.T) {
	snapshot := proxySnapshot{Proxies: map[string]proxyInfo{"a": {Type: "Selector", All: []string{"b"}, Now: "b"}, "b": {Type: "Fallback", All: []string{"a"}, Now: "a"}}}
	if _, ok := resolveActiveNode(snapshot, "a"); ok {
		t.Fatal("accepted cycle")
	}
	snapshot.Proxies["b"] = proxyInfo{Type: "Fallback", All: []string{"x"}, Now: "y"}
	if _, ok := resolveActiveNode(snapshot, "a"); ok {
		t.Fatal("accepted invalid edge")
	}
}

func TestAISelectionRequiresBothEndpoints(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate config.json/node-labels reads
	for _, tc := range []struct {
		name      string
		api, chat int
		want      string
	}{
		{"both available", 401, 200, "node"},
		{"chatgpt forbidden retains unverified current", 401, 403, "node"},
		{"api unavailable retains unverified current", 403, 200, "node"},
		{"both unavailable retains unverified current", 403, 403, "node"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			now := "node"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.URL.Path == "/proxies" {
					json.NewEncoder(w).Encode(map[string]any{"proxies": map[string]any{
						"JP-ALL": map[string]any{"type": "URLTest", "all": []string{"node"}},
						"AI主代理":  map[string]any{"type": "Selector", "all": []string{"node", "FALLBACK-LOCAL"}, "now": now},
						"node":   map[string]any{"type": "Http"}, "FALLBACK-LOCAL": map[string]any{"type": "Http"},
					}})
					return
				}
				if r.Method == http.MethodPut {
					var v map[string]string
					json.NewDecoder(r.Body).Decode(&v)
					now = v["name"]
					w.WriteHeader(204)
					return
				}
				q := r.URL.Query()
				status := 204
				if q.Get("url") == chatGPTProbeURL {
					status = tc.chat
				} else if q.Get("url") == "https://api.openai.com/v1/models" {
					status = tc.api
				}
				if q.Get("expected") != fmt.Sprint(status) {
					fmt.Fprint(w, `{"delay":0}`) // definitive node-specific failed measurement
					return
				}
				fmt.Fprint(w, `{"delay":100}`)
			}))
			defer srv.Close()
			p, err := newNodeProber(strings.TrimPrefix(srv.URL, "http://"), "s", "https://example.com", time.Second, nil)
			if err != nil {
				t.Fatal(err)
			}
			p.aiURL = "https://api.openai.com/v1/models"
			p.selectionPath = filepath.Join(t.TempDir(), "selection")
			if err := p.cycle(context.Background()); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if now != tc.want {
				t.Fatalf("selected %q, want %q", now, tc.want)
			}
		})
	}
}

func TestAIFallbackRequiresBothEndpointsAndConclusiveFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct {
		name                      string
		nodeCode                  int
		fallbackAPI, fallbackChat int
		want                      string
	}{
		{"dual verified", 0, 401, 200, "FALLBACK-LOCAL"},
		{"fallback chat fails", 0, 401, 403, "node"},
		{"fallback api fails", 0, 500, 200, "node"},
		{"node uncertain", 503, 401, 200, "node"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			selected := "node"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.URL.Path == "/proxies" {
					_ = json.NewEncoder(w).Encode(map[string]any{"proxies": map[string]any{
						"JP-ALL": map[string]any{"type": "URLTest", "all": []string{"node"}},
						"AI主代理":  map[string]any{"type": "Selector", "all": []string{"node", "FALLBACK-LOCAL"}, "now": selected},
						"node":   map[string]any{"type": "Http"}, "FALLBACK-LOCAL": map[string]any{"type": "Http"},
					}})
					return
				}
				if r.Method == http.MethodPut {
					var v map[string]string
					_ = json.NewDecoder(r.Body).Decode(&v)
					selected = v["name"]
					w.WriteHeader(204)
					return
				}
				if strings.Contains(r.URL.Path, "/node/delay") {
					if tc.nodeCode == 503 {
						w.WriteHeader(503)
					} else {
						fmt.Fprint(w, `{"delay":0}`)
					}
					return
				}
				status := tc.fallbackAPI
				if r.URL.Query().Get("url") == chatGPTProbeURL {
					status = tc.fallbackChat
				}
				if r.URL.Query().Get("expected") == fmt.Sprint(status) {
					fmt.Fprint(w, `{"delay":25}`)
				} else {
					fmt.Fprint(w, `{"delay":0}`)
				}
			}))
			defer srv.Close()
			p, err := newNodeProber(strings.TrimPrefix(srv.URL, "http://"), "s", "https://example.com", time.Second, nil)
			p.aiTargets = []string{"openai"}
			if err != nil {
				t.Fatal(err)
			}
			p.aiURL = "https://api.openai.com/v1/models"
			p.selectionPath = filepath.Join(t.TempDir(), "selection")
			if err := p.cycle(context.Background()); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if selected != tc.want {
				t.Fatalf("got %s want %s", selected, tc.want)
			}
		})
	}
}

func TestAISelectionSwitchesUncertainActiveOnlyToFreshDualHealthyCandidate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct {
		name          string
		activeAPI     int
		activeChat    int
		candidateAPI  int
		candidateChat int
		want          string
	}{
		{
			name:          "dual healthy candidate permits switch",
			activeAPI:     http.StatusInternalServerError,
			activeChat:    http.StatusInternalServerError,
			candidateAPI:  http.StatusOK,
			candidateChat: http.StatusOK,
			want:          "candidate",
		},
		{
			name:          "uncertain candidate keeps current node",
			activeAPI:     http.StatusInternalServerError,
			activeChat:    http.StatusInternalServerError,
			candidateAPI:  http.StatusOK,
			candidateChat: http.StatusInternalServerError,
			want:          "active",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			now := map[string]string{"主代理": "active", "AI主代理": "active"}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/proxies" {
					mu.Lock()
					defer mu.Unlock()
					json.NewEncoder(w).Encode(map[string]any{"proxies": map[string]any{
						"JP-ALL":         map[string]any{"type": "URLTest", "all": []string{"active", "candidate"}},
						"主代理":            map[string]any{"type": "Selector", "all": []string{"active", "candidate", "FALLBACK-LOCAL"}, "now": now["主代理"]},
						"AI主代理":          map[string]any{"type": "Selector", "all": []string{"active", "candidate", "FALLBACK-LOCAL"}, "now": now["AI主代理"]},
						"active":         map[string]any{"type": "Http"},
						"candidate":      map[string]any{"type": "Http"},
						"FALLBACK-LOCAL": map[string]any{"type": "Http"},
					}})
					return
				}
				if r.Method == http.MethodPut {
					var body map[string]string
					_ = json.NewDecoder(r.Body).Decode(&body)
					mu.Lock()
					now[strings.TrimPrefix(r.URL.Path, "/proxies/")] = body["name"]
					mu.Unlock()
					w.WriteHeader(http.StatusNoContent)
					return
				}

				name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/proxies/"), "/delay")
				if name == "active" || name == "candidate" {
					status := http.StatusOK
					switch {
					case r.URL.Query().Get("url") == chatGPTProbeURL && name == "active":
						status = tc.activeChat
					case r.URL.Query().Get("url") == chatGPTProbeURL && name == "candidate":
						status = tc.candidateChat
					case r.URL.Query().Get("expected") == fmt.Sprint(openAIProbeExpected) && name == "active":
						status = tc.activeAPI
					case r.URL.Query().Get("expected") == fmt.Sprint(openAIProbeExpected) && name == "candidate":
						status = tc.candidateAPI
					}
					if status != http.StatusOK {
						w.WriteHeader(status)
						return
					}
				}
				fmt.Fprint(w, `{"delay":100}`)
			}))
			defer srv.Close()

			p, err := newNodeProber(strings.TrimPrefix(srv.URL, "http://"), "s", "https://example.com", time.Second, nil)
			if err != nil {
				t.Fatal(err)
			}
			p.aiTargets = []string{"openai"}
			p.selectionPath = filepath.Join(t.TempDir(), "selection")
			if err := p.cycle(context.Background()); err != nil {
				t.Fatal(err)
			}

			mu.Lock()
			got := now["AI主代理"]
			mu.Unlock()
			if got != tc.want {
				t.Fatalf("AI selector = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestManualAIHealthTracksBothSitesAndActualMode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		chatOK  bool
		mode    Mode
		routing string
		want    bool
	}{
		{"healthy but unbound", true, ModePrimary, "rule", false},
		{"chat forbidden", false, ModePrimary, "rule", false},
		{"external fallback", true, ModeFallback, "rule", false},
		{"direct", true, ModePrimary, "direct", false},
		{"global", true, ModePrimary, "global", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			if err := saveNodeSelection(manualNodePath(), map[string]string{"AI主代理": "node"}); err != nil {
				t.Fatal(err)
			}
			if err := saveRoutingMode(tc.routing); err != nil {
				t.Fatal(err)
			}
			var puts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/proxies" {
					fmt.Fprint(w, `{"proxies":{"JP-ALL":{"type":"URLTest","all":["node"]},"AI主代理":{"type":"Selector","all":["node","FALLBACK-LOCAL"],"now":"node"},"node":{"type":"Http"},"FALLBACK-LOCAL":{"type":"Http"}}}`)
					return
				}
				if r.Method == http.MethodPut {
					puts.Add(1)
					w.WriteHeader(204)
					return
				}
				if r.URL.Query().Get("url") == chatGPTProbeURL && !tc.chatOK {
					w.WriteHeader(503)
					return
				}
				fmt.Fprint(w, `{"delay":100}`)
			}))
			defer srv.Close()
			p, err := newNodeProber(strings.TrimPrefix(srv.URL, "http://"), "s", "https://example.com", time.Second, nil)
			if err != nil {
				t.Fatal(err)
			}
			p.aiURL = "https://example.com/ai"
			p.selectionPath = filepath.Join(t.TempDir(), "selection")
			p.app = &App{mode: tc.mode, aiHealthy: true, aiMS: 777, aiCode: 401, state: &State{}, log: log.New(io.Discard, "", 0)}
			if err := p.cycle(context.Background()); err != nil {
				t.Fatal(err)
			}
			if p.app.aiHealthy != tc.want {
				t.Fatalf("AI health = %v, want %v", p.app.aiHealthy, tc.want)
			}
			if !tc.want && (p.app.aiMS != 0 || p.app.aiCode != 0) {
				t.Fatal("stale measurement retained")
			}
			if puts.Load() != 0 {
				t.Fatal("manual selector changed")
			}
		})
	}
}

// TestSourcePrioritySelection exercises source-priority selection: sticky and
// previous nodes are only honored inside the highest-priority enabled source
// with fresh success evidence, descent requires explicit failures of every JP
// node of a source, and unclear or expired evidence holds the selection.
func TestSourcePrioritySelection(t *testing.T) {
	type tc struct {
		name         string
		defs         []sourceDef
		nodeSources  map[string]string
		generation   string
		removeLabels bool
		delays       map[string]int
		active       string
		previous     map[string]string
		manual       map[string]string
		want         string
		wantPuts     int
	}
	defs := []sourceDef{
		{ID: "big-airport", Label: "Big", Type: "profile", Enabled: true},
		{ID: "ktm-cloud", Label: "KTM", Type: "subscription", Enabled: true},
	}
	bigNodes := map[string]string{"n1": "big-airport", "n2": "big-airport", "n3": "ktm-cloud"}
	for _, c := range []tc{
		{
			name: "priority beats faster lower source", defs: defs, nodeSources: bigNodes,
			delays: map[string]int{"n1": 450, "n2": 400, "n3": 50, "n4": 550},
			active: "FALLBACK-LOCAL", want: "n2", wantPuts: 1,
		},
		{
			name: "sticky kept inside top source", defs: defs, nodeSources: bigNodes,
			delays: map[string]int{"n1": 450, "n2": 400, "n3": 50, "n4": 550},
			active: "n2", want: "n2", wantPuts: 0,
		},
		{
			name: "sticky not kept for lower source", defs: defs, nodeSources: bigNodes,
			delays: map[string]int{"n1": 450, "n2": 400, "n3": 50, "n4": 550},
			active: "n3", want: "n2", wantPuts: 1,
		},
		{
			name: "previous honored inside top source", defs: defs, nodeSources: bigNodes,
			delays: map[string]int{"n1": 450, "n2": -1, "n3": 50, "n4": 550},
			active: "n2", previous: map[string]string{"主代理": "n1"}, want: "n1", wantPuts: 1,
		},
		{
			name: "descend only on explicit failure", defs: defs, nodeSources: bigNodes,
			delays: map[string]int{"n1": -1, "n2": -1, "n3": 50, "n4": 550},
			active: "FALLBACK-LOCAL", want: "n3", wantPuts: 1,
		},
		{
			name: "fresh healthy lower source beats uncertain nodes", defs: defs, nodeSources: bigNodes,
			delays: map[string]int{"n1": 0, "n2": 0, "n3": 50, "n4": 550},
			active: "FALLBACK-LOCAL", want: "n3", wantPuts: 1,
		},
		{
			name: "fresh healthy lower source beats uncertain active", defs: defs, nodeSources: bigNodes,
			delays: map[string]int{"n1": 0, "n2": 0, "n3": 50, "n4": 550},
			active: "n1", want: "n3", wantPuts: 1,
		},
		{
			name: "uncertain active retained without healthy alternative", defs: defs, nodeSources: bigNodes,
			delays: map[string]int{"n1": 0, "n2": 0, "n3": 0, "n4": 0},
			active: "n1", want: "n1", wantPuts: 0,
		},
		{
			name: "all target failures retain current node", defs: defs, nodeSources: bigNodes,
			delays: map[string]int{"n1": -1, "n2": -1, "n3": -1, "n4": -1},
			active: "n1", want: "n1", wantPuts: 0,
		},
		{
			name: "healthy unknown node blocks fallback", defs: defs, nodeSources: bigNodes,
			delays: map[string]int{"n1": -1, "n2": -1, "n3": -1, "n4": 600},
			active: "n1", want: "n1", wantPuts: 0,
		},
		{
			name: "disabled top source skipped", defs: []sourceDef{{ID: "big-airport", Label: "Big", Type: "profile", Enabled: false}, defs[1]}, nodeSources: bigNodes,
			delays: map[string]int{"n1": 50, "n2": 450, "n3": 100, "n4": 550},
			active: "FALLBACK-LOCAL", want: "n3", wantPuts: 1,
		},
		{
			name: "stale labels fall back to whole pool", defs: defs, nodeSources: bigNodes,
			generation: "0123456789abcdef",
			delays:     map[string]int{"n1": 450, "n2": 400, "n3": 50, "n4": 550},
			active:     "FALLBACK-LOCAL", want: "n3", wantPuts: 1,
		},
		{
			name: "missing labels fall back to whole pool", defs: defs, nodeSources: bigNodes,
			removeLabels: true,
			delays:       map[string]int{"n1": 450, "n2": 400, "n3": 50, "n4": 550},
			active:       "FALLBACK-LOCAL", want: "n3", wantPuts: 1,
		},
		{
			name: "manual pin crosses sources", defs: defs, nodeSources: bigNodes,
			delays: map[string]int{"n1": 450, "n2": 400, "n3": 50, "n4": 550},
			active: "n3", manual: map[string]string{"主代理": "n3"}, want: "n3", wantPuts: 0,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			writeSourcesEnvironment(t, c.defs, c.nodeSources, c.generation)
			if c.removeLabels {
				if err := os.Remove(nodeLabelsPath()); err != nil {
					t.Fatal(err)
				}
			}
			if len(c.manual) > 0 {
				if err := saveNodeSelection(manualNodePath(), c.manual); err != nil {
					t.Fatal(err)
				}
			}
			var mu sync.Mutex
			now := map[string]string{"主代理": c.active}
			puts := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.URL.Path == "/proxies" {
					json.NewEncoder(w).Encode(map[string]any{"proxies": map[string]any{
						"FALLBACK-LOCAL": map[string]any{"type": "Http"},
						"JP-ALL":         map[string]any{"type": "URLTest", "all": []string{"n1", "n2", "n3", "n4"}},
						"主代理":            map[string]any{"type": "Selector", "all": []string{"FALLBACK-LOCAL", "n1", "n2", "n3", "n4"}, "now": now["主代理"]},
						"n1":             map[string]any{"type": "Http"}, "n2": map[string]any{"type": "Http"}, "n3": map[string]any{"type": "Http"}, "n4": map[string]any{"type": "Http"},
					}})
					return
				}
				if r.Method == http.MethodPut {
					puts++
					var v map[string]string
					_ = json.NewDecoder(r.Body).Decode(&v)
					now["主代理"] = v["name"]
					w.WriteHeader(204)
					return
				}
				name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/proxies/"), "/delay")
				delay := c.delays[name]
				if delay < 0 {
					fmt.Fprint(w, `{"delay":0}`) // explicit failed measurement
					return
				}
				if delay == 0 {
					w.WriteHeader(401) // controller error, not node evidence
					return
				}
				fmt.Fprintf(w, `{"delay":%d}`, delay)
			}))
			defer srv.Close()
			p, err := newNodeProber(strings.TrimPrefix(srv.URL, "http://"), "s", "https://example.com", time.Second, nil)
			if err != nil {
				t.Fatal(err)
			}
			p.selectionPath = filepath.Join(t.TempDir(), "selection.json")
			if c.manual != nil {
				p.app = &App{} // manual preference handling requires an App
			}
			if len(c.previous) > 0 {
				if err := saveNodeSelection(p.selectionPath, c.previous); err != nil {
					t.Fatal(err)
				}
			}
			if err := p.cycle(context.Background()); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if now["主代理"] != c.want {
				t.Fatalf("selected %q, want %q", now["主代理"], c.want)
			}
			if c.wantPuts >= 0 && puts != c.wantPuts {
				t.Fatalf("selector writes = %d, want %d", puts, c.wantPuts)
			}
		})
	}
}

func TestAllExcludedPolicyVersusUnknownHealth(t *testing.T) {
	for _, bound := range []bool{false, true} {
		idx := sourceIndex{}
		if bound {
			idx = sourceIndex{bound: true, defs: []sourceDef{{ID: "s", Enabled: true}}, nodeSource: map[string]string{"a": "s", "b": "s"}}
		}
		in := selectNodeChoiceInput{Index: idx, Candidates: []string{"a", "b"}, Active: "a", Previous: "a", Priorities: map[string]string{"a": "disabled", "b": "disabled:high"}, Healthy: func(string) bool { return false }, Failed: func(string) bool { return false }, Delay: func(string) int64 { return 10 }}
		if got := selectNodeChoice(in); got != "" {
			t.Fatalf("bound=%v all excluded: %q", bound, got)
		}
		delete(in.Priorities, "b")
		if got := selectNodeChoice(in); got != "" {
			t.Fatalf("bound=%v unknown remaining must hold: %q", bound, got)
		}
		in.Candidates = nil
		if got := selectNodeChoice(in); got != "" {
			t.Fatalf("bound=%v empty unknown pool: %q", bound, got)
		}
	}
}

// A 504 from mihomo means this node could not fetch the target. It is node
// evidence, and the scan must also try the independent targets before deciding.
func TestNodeProbe504IsTargetFailureAndTriesIndependentTargets(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var attempted sync.Map
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/proxies" {
			fmt.Fprint(w, `{"proxies":{"JP-ALL":{"type":"URLTest","all":["n"]},"n":{"type":"Shadowsocks"}}}`)
			return
		}
		attempted.Store(r.URL.Query().Get("url"), true)
		http.Error(w, "timeout", 504)
	}))
	defer srv.Close()
	p, err := newNodeProber(strings.TrimPrefix(srv.URL, "http://"), "s", "https://example.com/generate_204", 100*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := p.probeGeneration(context.Background(), "n", "gstatic", p.probeURL, nodeProbeExpectedCode(p.probeURL), 0, false, false)
	if !got.failed || got.uncertain {
		t.Fatalf("504 must be a target-level failure: %+v", got)
	}
	for _, target := range ordinaryProbeTargets(p.probeURL) {
		if _, ok := attempted.Load(target); !ok {
			t.Fatalf("independent target %s was not tried", target)
		}
	}
}

// mihomo reports a failed per-node delay test as HTTP 503 with a fixed message.
// That is node evidence, unlike a controller that is itself unavailable.
func TestNodeProbe503DelayTestMessageIsNodeFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/proxies" {
			fmt.Fprint(w, `{"proxies":{"JP-ALL":{"type":"URLTest","all":["n"]},"n":{"type":"Shadowsocks"}}}`)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"message":"An error occurred in the delay test"}`)
	}))
	defer srv.Close()
	p, err := newNodeProber(strings.TrimPrefix(srv.URL, "http://"), "s", "https://example.com/generate_204", 100*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := p.probeGeneration(context.Background(), "n", "gstatic", p.probeURL, nodeProbeExpectedCode(p.probeURL), 0, false, false)
	if !got.failed || got.uncertain {
		t.Fatalf("delay test failure must be node evidence: %+v", got)
	}
}
