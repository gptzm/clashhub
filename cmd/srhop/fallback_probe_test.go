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

func TestFallbackIndependentTargetEvidence(t *testing.T) {
	for _, tc := range []struct {
		name          string
		first, second int
		want          bool
	}{
		{"configured succeeds", 204, 500, true},
		{"EOF then independent succeeds", 0, 200, true},
		{"EOF then wrong status", 0, 204, false},
		{"both fail", 0, 500, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var first, second atomic.Int32
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				status := tc.second
				if r.URL.Host == "gstatic.test" {
					first.Add(1)
					status = tc.first
				} else if r.URL.Host == "independent.test" {
					second.Add(1)
				} else {
					t.Errorf("unexpected target %s", r.URL.Host)
					w.WriteHeader(500)
					return
				}
				if status == 0 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						conn.Close()
					}
					return
				}
				w.WriteHeader(status)
			}))
			defer proxy.Close()
			a := &App{cfg: Config{FallbackAddr: strings.TrimPrefix(proxy.URL, "http://"), ProbeURL: "http://gstatic.test/", ProbeTimeoutSec: 1, RequireJapan: true}}
			if got := a.fallbackUsableTarget("http://independent.test/"); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
			if a.egressOK || a.aiHealthy {
				t.Fatal("fallback evidence invented Japan or AI health")
			}
			if first.Load() != 1 || (tc.first == 204 && second.Load() != 0) || (tc.first != 204 && second.Load() != 1) {
				t.Fatalf("calls first=%d second=%d", first.Load(), second.Load())
			}
		})
	}
}

func TestFallbackProbeUsesProxyAndExpires(t *testing.T) {
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.String() != "http://example.test/generate_204" {
			t.Errorf("unexpected URL %q", r.URL.String())
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer proxy.Close()
	p := &fallbackProbe{}
	if p.snapshot(time.Now()).Status != "pending" {
		t.Fatal("expected pending")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.run(ctx, strings.TrimPrefix(proxy.URL, "http://"), "http://example.test/generate_204", time.Hour, time.Second)
	}()
	deadline := time.Now().Add(time.Second)
	for p.snapshot(time.Now()).Status == "pending" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	r := p.snapshot(time.Now())
	if r.Status != "ok" || r.DelayMS == nil || r.CheckedAt == nil || r.ExpiresAt == nil || calls.Load() != 1 {
		t.Fatalf("unexpected result %+v calls %d", r, calls.Load())
	}
	if got := p.snapshot(*r.ExpiresAt).Status; got != "expired" {
		t.Fatalf("got %s", got)
	}
	for i := 0; i < 100; i++ {
		p.snapshot(time.Now())
	}
	if calls.Load() != 1 {
		t.Fatal("reads triggered requests")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("did not stop")
	}
}

func TestFallbackProbeRejectsNon204AndRedirect(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusFound, http.StatusProxyAuthRequired} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "http://example.test/redirect")
				w.WriteHeader(status)
			}))
			defer proxy.Close()
			p := &fallbackProbe{}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				p.run(ctx, strings.TrimPrefix(proxy.URL, "http://"), "http://example.test/", time.Hour, time.Second)
			}()
			deadline := time.Now().Add(time.Second)
			for p.snapshot(time.Now()).Status == "pending" && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			r := p.snapshot(time.Now())
			if r.Status != "failed" || r.DelayMS != nil || r.CheckedAt == nil {
				t.Fatalf("unexpected result %+v", r)
			}
			cancel()
			<-done
		})
	}
}

func TestFallbackProbeDiagnosticPayload(t *testing.T) {
	d := newNodeDiagnostics()
	s := &statsIntegration{diagnostics: d}
	req := httptest.NewRequest(http.MethodGet, "/api/node-health", nil)
	w := httptest.NewRecorder()
	s.serveNodeDiagnostics(w, req)
	var payload struct {
		Fallback fallbackProbeResult `json:"fallback_probe"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Fallback.Status != "pending" || payload.Fallback.DelayMS != nil || payload.Fallback.CheckedAt != nil {
		t.Fatalf("unexpected payload %s", w.Body.String())
	}
}
