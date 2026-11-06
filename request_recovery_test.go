package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPrimaryFailurePermitsOrdinaryFallbackRetry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := saveNodeSelection(nodePrioritiesPath(), map[string]string{"a": "disabled"}); err != nil {
		t.Fatal(err)
	}
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/configs" {
			_, _ = io.WriteString(w, `{"mode":"rule"}`)
			return
		}
		_, _ = io.WriteString(w, `{"proxies":{"主代理":{"type":"Selector","all":["a"],"now":"a"},"AI主代理":{"type":"Selector","all":["b"],"now":"b"},"a":{"type":"Trojan"},"b":{"type":"Trojan"}}}`)
	}))
	defer controller.Close()
	var calls atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) }))
	defer fallback.Close()
	cfg := defaultConfig()
	cfg.Controller = strings.TrimPrefix(controller.URL, "http://")
	cfg.PrimaryAddr = "127.0.0.1:1"
	cfg.FallbackAddr = strings.TrimPrefix(fallback.URL, "http://")
	a := &App{cfg: cfg, mode: ModePrimary, log: log.New(io.Discard, "", 0), transports: map[string]*http.Transport{}}
	w := httptest.NewRecorder()
	a.handleHTTP(w, httptest.NewRequest(http.MethodGet, "http://example.test/", nil))
	if w.Code != 200 || calls.Load() != 1 {
		t.Fatalf("status=%d fallback calls=%d", w.Code, calls.Load())
	}
}

func TestFallbackSafeHTTPRetriesPrimary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var calls atomic.Int32
	p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) }))
	defer p.Close()
	f := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { c, _, _ := w.(http.Hijacker).Hijack(); c.Close() }))
	defer f.Close()
	a := &App{cfg: Config{PrimaryAddr: strings.TrimPrefix(p.URL, "http://"), FallbackAddr: strings.TrimPrefix(f.URL, "http://")}, mode: ModeFallback, log: log.New(io.Discard, "", 0), transports: map[string]*http.Transport{}}
	for _, method := range []string{"GET", "HEAD", "OPTIONS", "TRACE"} {
		w := httptest.NewRecorder()
		a.handleHTTP(w, httptest.NewRequest(method, "http://example.test/", nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d", method, w.Code)
		}
	}
	if calls.Load() != 4 {
		t.Fatal(calls.Load())
	}
	// A canceled request and stale routing generation never reach the primary.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("GET", "http://example.test/", nil).WithContext(ctx)
	if _, err := a.recoveryPrimaryRoundTrip(r, 0); err == nil {
		t.Fatal("canceled retry accepted")
	}
	if _, err := a.recoveryPrimaryRoundTrip(httptest.NewRequest("GET", "http://example.test/", nil), 1); err == nil {
		t.Fatal("stale retry accepted")
	}
	if calls.Load() != 4 {
		t.Fatal("unsafe retry reached primary")
	}
	if err := saveRoutingMode("direct"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.recoveryPrimaryRoundTrip(httptest.NewRequest("GET", "http://example.test/", nil), 0); err == nil {
		t.Fatal("direct routing retry accepted")
	}
	if _, _, err := a.recoveryPrimaryDial(context.Background(), "example.test:443", 0); err == nil {
		t.Fatal("direct routing CONNECT retry accepted")
	}
	if calls.Load() != 4 {
		t.Fatal("direct request reached proxy")
	}

}
func TestFallbackCONNECTRetriesPrimaryAndCancels(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var calls atomic.Int32
	p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		c, _, _ := w.(http.Hijacker).Hijack()
		defer c.Close()
		io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
		time.Sleep(20 * time.Millisecond)
	}))
	defer p.Close()
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := dead.Addr().String()
	dead.Close()
	a := &App{cfg: Config{PrimaryAddr: strings.TrimPrefix(p.URL, "http://"), FallbackAddr: addr, PrimaryTimeoutMS: 1000}, mode: ModeFallback, log: log.New(io.Discard, "", 0)}
	c, label, err := a.dialForTarget(context.Background(), "example.test:443")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if label != a.cfg.PrimaryAddr || calls.Load() != 1 {
		t.Fatal(label, calls.Load())
	}
	if _, _, err := a.recoveryPrimaryDial(context.Background(), "example.test:443", 1); err == nil {
		t.Fatal("stale generation accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := a.recoveryPrimaryDial(ctx, "example.test:443", 0); err == nil {
		t.Fatal("cancel accepted")
	}
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(100 * time.Millisecond) }))
	defer stalled.Close()
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := dialProxyContext(ctx, strings.TrimPrefix(stalled.URL, "http://"), "example.test:443", time.Second); err == nil {
		t.Fatal("stall accepted")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("context not respected")
	}
}

func TestRoutingAttemptDoesNotBlockCommit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, kind := range []string{"http", "connect"} {
		t.Run(kind, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-release }))
			defer server.Close()
			a := &App{cfg: Config{PrimaryAddr: strings.TrimPrefix(server.URL, "http://"), PrimaryTimeoutMS: 1000}, transports: map[string]*http.Transport{}}
			done := make(chan error, 1)
			go func() {
				if kind == "http" {
					resp, err := a.recoveryPrimaryRoundTrip(httptest.NewRequest("GET", "http://example.test/", nil), 0)
					if resp != nil {
						resp.Body.Close()
					}
					done <- err
				} else {
					c, _, err := a.recoveryPrimaryDial(context.Background(), "example.test:443", 0)
					if c != nil {
						c.Close()
					}
					done <- err
				}
			}()
			<-started
			committed := make(chan struct{})
			go func() {
				a.routingGate.Lock()
				a.routingGeneration++
				a.cancelRoutingAttemptsLocked()
				a.routingGate.Unlock()
				close(committed)
			}()
			select {
			case <-committed:
			case <-time.After(200 * time.Millisecond):
				close(release)
				t.Fatal("routing commit blocked by IO")
			}
			select {
			case err := <-done:
				if err == nil {
					t.Error("invalidated request succeeded")
				}
			case <-time.After(300 * time.Millisecond):
				close(release)
				t.Fatal("routing attempt not canceled")
			}
			close(release)
		})
	}
}

func TestPrimarySafeHTTPRetriesFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var calls atomic.Int32
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) }))
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { c, _, _ := w.(http.Hijacker).Hijack(); c.Close() }))
	defer bad.Close()
	a := &App{cfg: Config{PrimaryAddr: strings.TrimPrefix(bad.URL, "http://"), FallbackAddr: strings.TrimPrefix(good.URL, "http://")}, mode: ModePrimary, log: log.New(io.Discard, "", 0), transports: map[string]*http.Transport{}}
	for _, method := range []string{"GET", "HEAD", "OPTIONS", "TRACE"} {
		w := httptest.NewRecorder()
		a.handleHTTP(w, httptest.NewRequest(method, "http://example.test/", nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d", method, w.Code)
		}
	}
	for _, method := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		w := httptest.NewRecorder()
		a.handleHTTP(w, httptest.NewRequest(method, "http://example.test/", strings.NewReader("payload")))
		if w.Code != 502 {
			t.Fatalf("%s: %d", method, w.Code)
		}
	}
	if calls.Load() != 4 {
		t.Fatalf("unsafe or repeated retry: %d", calls.Load())
	}
}

func TestAIHTTPTransportFailureWakesScanWithoutUnsafeRetry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := saveRoutingMode("rule"); err != nil {
		t.Fatal(err)
	}
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = c.Close()
		}
	}))
	defer primary.Close()
	var fallbackCalls atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer fallback.Close()
	a := &App{
		cfg:          Config{PrimaryAddr: strings.TrimPrefix(primary.URL, "http://"), FallbackAddr: strings.TrimPrefix(fallback.URL, "http://")},
		mode:         ModePrimary,
		log:          log.New(io.Discard, "", 0),
		transports:   map[string]*http.Transport{},
		nodeScanWake: make(chan struct{}, 1),
	}

	w := httptest.NewRecorder()
	a.handleHTTP(w, httptest.NewRequest(http.MethodGet, "http://api.openai.com/v1/models", nil))
	if w.Code != http.StatusOK || w.Body.String() != "ok" {
		t.Fatalf("safe AI request: %d %q", w.Code, w.Body.String())
	}
	select {
	case <-a.nodeScanWake:
	case <-time.After(time.Second):
		t.Fatal("AI HTTP transport failure did not wake node scan")
	}

	// A non-idempotent request still must not be replayed through fallback. It
	// may share the already queued health signal, but cannot create another one.
	w = httptest.NewRecorder()
	a.handleHTTP(w, httptest.NewRequest(http.MethodPost, "http://api.openai.com/v1/models", strings.NewReader("secret")))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("non-idempotent AI request status = %d, want 502", w.Code)
	}
	if got := fallbackCalls.Load(); got != 1 {
		t.Fatalf("non-idempotent request reached fallback: %d calls", got)
	}
	select {
	case <-a.nodeScanWake:
		t.Fatal("AI failure wake was not throttled")
	default:
	}
}

func TestAIConnectFailureWakesScan(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := saveRoutingMode("rule"); err != nil {
		t.Fatal(err)
	}
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = c.Close()
		}
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
	}))
	defer fallback.Close()
	a := &App{
		cfg:          Config{PrimaryAddr: strings.TrimPrefix(primary.URL, "http://"), FallbackAddr: strings.TrimPrefix(fallback.URL, "http://"), PrimaryTimeoutMS: 500},
		mode:         ModePrimary,
		log:          log.New(io.Discard, "", 0),
		nodeScanWake: make(chan struct{}, 1),
	}
	c, label, err := a.dialForTarget(context.Background(), "api.openai.com:443")
	if err != nil {
		t.Fatal(err)
	}
	if c != nil {
		_ = c.Close()
	}
	if label != a.cfg.FallbackAddr {
		t.Fatalf("route = %q, want fallback %q", label, a.cfg.FallbackAddr)
	}
	select {
	case <-a.nodeScanWake:
	case <-time.After(time.Second):
		t.Fatal("AI CONNECT failure did not wake node scan")
	}
}

func TestOrdinaryHTTPFailureWakesScanButResponseDoesNot(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(fmt.Sprintf("disconnect=%v", disconnect), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			if err := saveRoutingMode("rule"); err != nil {
				t.Fatal(err)
			}
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if disconnect {
					c, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						_ = c.Close()
					}
					return
				}
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer primary.Close()
			var fallbackCalls atomic.Int32
			fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fallbackCalls.Add(1); w.WriteHeader(200) }))
			defer fallback.Close()
			a := &App{cfg: Config{PrimaryAddr: strings.TrimPrefix(primary.URL, "http://"), FallbackAddr: strings.TrimPrefix(fallback.URL, "http://")}, mode: ModePrimary, log: log.New(io.Discard, "", 0), transports: map[string]*http.Transport{}, nodeScanWake: make(chan struct{}, 1)}
			w := httptest.NewRecorder()
			a.handleHTTP(w, httptest.NewRequest(http.MethodPost, "http://example.com/test", strings.NewReader("body")))
			if fallbackCalls.Load() != 0 {
				t.Fatal("POST must not be replayed")
			}
			select {
			case <-a.nodeScanWake:
				if !disconnect {
					t.Fatal("HTTP 503 response woke scanner")
				}
			default:
				if disconnect {
					t.Fatal("ordinary transport failure did not wake scanner")
				}
			}
		})
	}
}

func TestOrdinaryConnectFailureWakesScan(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := saveRoutingMode("rule"); err != nil {
		t.Fatal(err)
	}
	a := &App{cfg: Config{PrimaryAddr: "127.0.0.1:1", FallbackAddr: "127.0.0.1:1", PrimaryTimeoutMS: 100}, mode: ModePrimary, log: log.New(io.Discard, "", 0), nodeScanWake: make(chan struct{}, 1)}
	if c, _, _ := a.dialForTarget(context.Background(), "example.com:443"); c != nil {
		c.Close()
	}
	select {
	case <-a.nodeScanWake:
	default:
		t.Fatal("ordinary CONNECT failure did not wake scanner")
	}
}

func TestConcurrentFailureSignalsAreCoalesced(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := saveRoutingMode("rule"); err != nil {
		t.Fatal(err)
	}
	a := &App{nodeScanWake: make(chan struct{}, 1)}
	token, ok := a.snapshotProbeToken()
	if !ok {
		t.Fatal("route unavailable")
	}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); a.scheduleFailureScan(token) }()
	}
	wg.Wait()
	select {
	case <-a.nodeScanWake:
	default:
		t.Fatal("no scan signal")
	}
	a.scheduleFailureScan(token)
	select {
	case <-a.nodeScanWake:
		t.Fatal("not throttled")
	default:
	}
}
