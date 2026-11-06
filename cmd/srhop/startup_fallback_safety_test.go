package main

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProbeExpiryRequiresUsableFallback(t *testing.T) {
	for _, good := range []bool{false, true} {
		t.Run(map[bool]string{false: "dead", true: "healthy"}[good], func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if good {
					w.WriteHeader(204)
				} else {
					w.WriteHeader(502)
				}
			}))
			defer proxy.Close()
			cfg := defaultConfig()
			cfg.FallbackAddr = strings.TrimPrefix(proxy.URL, "http://")
			cfg.ProbeURL = "http://example.invalid/probe"
			cfg.ProbeTimeoutSec = 1
			a := &App{cfg: cfg, log: log.New(io.Discard, "", 0), state: &State{}, mode: ModeProbing, stopCh: make(chan struct{})}
			a.enterFallbackIfMode("probe expired", ModeProbing)
			got, _ := a.snapshotMode()
			want := ModePrimary
			if good {
				want = ModeFallback
			}
			if got != want {
				t.Fatalf("got %s want %s", got, want)
			}
		})
	}
}
