package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fastAlternativeSafetyFixture struct {
	app      *App
	puts     *atomic.Int32
	selected *atomic.Value
	close    func()
}

func newFastAlternativeSafetyFixture(t *testing.T, nodes []string) fastAlternativeSafetyFixture {
	t.Helper()
	selected := &atomic.Value{}
	selected.Store("n1")
	puts := &atomic.Int32{}
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/proxies" {
			all := append([]string(nil), nodes...)
			proxies := map[string]proxyInfo{
				"主代理":    {Type: "Selector", Now: selected.Load().(string), All: all},
				"JP-ALL": {Type: "URLTest", All: all},
			}
			for _, name := range nodes {
				proxies[name] = proxyInfo{Type: "Trojan"}
			}
			_ = json.NewEncoder(w).Encode(proxySnapshot{Proxies: proxies})
			return
		}
		if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/proxies/") {
			var body struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "invalid selector body", http.StatusBadRequest)
				return
			}
			puts.Add(1)
			selected.Store(body.Name)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))

	cfg := defaultConfig()
	cfg.Controller = strings.TrimPrefix(controller.URL, "http://")
	cfg.ProbeURL = "https://example.com"
	cfg.ProbeTimeoutSec = 1
	diagnostics := newNodeDiagnostics()
	app := &App{
		cfg:              cfg,
		engineGeneration: 0,
		stopCh:           make(chan struct{}),
		stats:            &statsIntegration{diagnostics: diagnostics},
	}
	return fastAlternativeSafetyFixture{
		app:      app,
		puts:     puts,
		selected: selected,
		close:    controller.Close,
	}
}

func setFastAlternativeHealth(d *nodeDiagnostics, nodes ...string) {
	now := time.Now()
	for i, name := range nodes {
		d.results[name] = map[string]nodeDiagnosticResult{
			"gstatic": {
				Status:    "ok",
				DelayMS:   int64(20 + i),
				CheckedAt: now,
				ExpiresAt: now.Add(time.Minute),
			},
		}
	}
}

func TestSelectFreshAlternativeSafetyGates(t *testing.T) {
	t.Run("manual ordinary node never PUTs", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		if err := saveRoutingMode("rule"); err != nil {
			t.Fatal(err)
		}
		fixture := newFastAlternativeSafetyFixture(t, []string{"n1", "n2"})
		defer fixture.close()
		setFastAlternativeHealth(fixture.app.stats.diagnostics, "n2")
		if err := saveNodeSelection(manualNodePath(), map[string]string{"主代理": "n1"}); err != nil {
			t.Fatal(err)
		}

		_, choice := fixture.app.selectFreshAlternative(context.Background(), probeToken{}, map[string]bool{})
		if choice != "" {
			t.Fatalf("manual ordinary node was replaced with %q", choice)
		}
		if got := fixture.puts.Load(); got != 0 {
			t.Fatalf("manual ordinary node caused %d selector PUTs", got)
		}
	})

	t.Run("direct routing never PUTs", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		if err := saveRoutingMode("direct"); err != nil {
			t.Fatal(err)
		}
		fixture := newFastAlternativeSafetyFixture(t, []string{"n1", "n2"})
		defer fixture.close()
		setFastAlternativeHealth(fixture.app.stats.diagnostics, "n2")

		_, choice := fixture.app.selectFreshAlternative(context.Background(), probeToken{}, map[string]bool{})
		if choice != "" {
			t.Fatalf("direct routing selected %q", choice)
		}
		if got := fixture.puts.Load(); got != 0 {
			t.Fatalf("direct routing caused %d selector PUTs", got)
		}
	})

	t.Run("stale generation never PUTs", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		if err := saveRoutingMode("rule"); err != nil {
			t.Fatal(err)
		}
		fixture := newFastAlternativeSafetyFixture(t, []string{"n1", "n2"})
		defer fixture.close()
		setFastAlternativeHealth(fixture.app.stats.diagnostics, "n2")
		fixture.app.routingGeneration = 1

		_, choice := fixture.app.selectFreshAlternative(context.Background(), probeToken{}, map[string]bool{})
		if choice != "" {
			t.Fatalf("stale routing generation selected %q", choice)
		}
		if got := fixture.puts.Load(); got != 0 {
			t.Fatalf("stale routing generation caused %d selector PUTs", got)
		}
	})
}

func TestSelectFreshAlternativeSafetyRespectsPriorityPolicy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		priorities map[string]string
		want       string
	}{
		{name: "disabled is excluded", priorities: map[string]string{"disabled": "disabled", "medium": "medium"}, want: "medium"},
		{name: "high beats low", priorities: map[string]string{"low": "low", "high": "high"}, want: "high"},
		{name: "available low remains usable", priorities: map[string]string{"low": "low"}, want: "low"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			if err := saveRoutingMode("rule"); err != nil {
				t.Fatal(err)
			}
			nodes := []string{"n1"}
			healthy := make([]string, 0, len(tc.priorities))
			for name := range tc.priorities {
				nodes = append(nodes, name)
				healthy = append(healthy, name)
			}
			fixture := newFastAlternativeSafetyFixture(t, nodes)
			defer fixture.close()
			setFastAlternativeHealth(fixture.app.stats.diagnostics, healthy...)
			if err := saveNodeSelection(nodePrioritiesPath(), tc.priorities); err != nil {
				t.Fatal(err)
			}

			_, choice := fixture.app.selectFreshAlternative(context.Background(), probeToken{}, map[string]bool{})
			if choice != tc.want {
				t.Fatalf("selected %q, want %q", choice, tc.want)
			}
			if got := fixture.puts.Load(); got != 1 {
				t.Fatalf("selector PUTs = %d, want 1", got)
			}
		})
	}
}

func TestSelectFreshAlternativeWithoutDiagnosticsReturnsImmediately(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	app := &App{}
	want := probeToken{routing: 3, engine: 4, selection: 5, aiSelection: 6}
	started := time.Now()
	got, choice := app.selectFreshAlternative(context.Background(), want, map[string]bool{})
	if choice != "" {
		t.Fatalf("selected %q without diagnostics", choice)
	}
	if got != want {
		t.Fatalf("token changed without diagnostics: got %+v want %+v", got, want)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("without diagnostics took %s", elapsed)
	}
}
