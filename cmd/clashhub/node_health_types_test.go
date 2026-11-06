package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNodeProbeTypesAreSeparate(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate config.json/node-labels reads
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/proxies" {
			fmt.Fprint(w, `{"proxies":{"JP-ALL":{"type":"URLTest","all":["n"]},"n":{"type":"Http"}}}`)
			return
		}
		if r.URL.Query().Get("url") == chatGPTProbeURL {
			if r.URL.Query().Get("expected") != "200" {
				t.Error("ChatGPT must require HTTP 200")
			}
			fmt.Fprint(w, `{"delay":15}`)
			return
		}
		switch r.URL.Query().Get("expected") {
		case "200", "204", "401":
			// 401 answers delay 0: the first openai attempt fails and the
			// chatgpt fallback attempt succeeds, exercising multi-attempt logic.
			if r.URL.Query().Get("expected") == "401" {
				fmt.Fprint(w, `{"delay":0}`)
			} else {
				fmt.Fprint(w, `{"delay":12}`)
			}
		case "0": // non-probe controller request (e.g. /proxies variants)
			fmt.Fprint(w, `{}`)
		default:
			t.Errorf("unexpected expected status %q url %q", r.URL.Query().Get("expected"), r.URL.Query().Get("url"))
		}
	}))
	defer srv.Close()
	store, err := OpenStats(filepath.Join(t.TempDir(), "stats.json"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := newNodeProber(strings.TrimPrefix(srv.URL, "http://"), "secret", "https://example.com", time.Second, &statsIntegration{store: store, logf: func(string, ...any) { t.Error("stats error") }})
	if err != nil {
		t.Fatal(err)
	}
	a := &App{cfg: defaultConfig(), log: log.New(io.Discard, "", 0), state: &State{}, stopCh: make(chan struct{})}
	p.app = a
	if err := p.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows := store.QueryHealth48Hours()
	// gstatic plus the openai target (its two attempts merge into one bucket).
	if len(rows) != 2 {
		t.Fatalf("expected separate types: %+v", rows)
	}
	for _, row := range rows {
		switch row.Name {
		case nodeHealthName("n") + "-gstatic":
			if row.Successes != 1 {
				t.Fatalf("gstatic: %+v", row)
			}
		case nodeHealthName("n") + "-openai":
			if row.Successes != 1 || row.Failures != 1 {
				t.Fatalf("openai: %+v", row)
			}
		default:
			t.Fatalf("unexpected name: %s", row.Name)
		}
	}
}
