package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOldRequestsCannotStartFallbackAfterDirectCommit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(appSupportDir(), 0700); err != nil {
		t.Fatal(err)
	}
	var fallbackCalls atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fallbackCalls.Add(1); w.WriteHeader(200) }))
	defer fallback.Close()
	started, release := make(chan struct{}), make(chan struct{})
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	defer primary.Close()
	cfg := defaultConfig()
	cfg.PrimaryAddr = strings.TrimPrefix(primary.URL, "http://")
	cfg.FallbackAddr = strings.TrimPrefix(fallback.URL, "http://")
	a := &App{cfg: cfg, log: log.New(io.Discard, "", 0), state: &State{}, mode: ModePrimary, transports: map[string]*http.Transport{}}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		a.handleHTTP(w, httptest.NewRequest("GET", "http://destination.test/", nil))
		done <- w
	}()
	<-started
	if err := a.commitRoutingMode("direct", "rule"); err != nil {
		t.Fatal(err)
	}
	close(release)
	w := <-done
	if w.Code != 502 || fallbackCalls.Load() != 0 {
		t.Fatalf("HTTP=%d fallback=%d", w.Code, fallbackCalls.Load())
	}
	if _, _, err := a.fallbackDial(context.Background(), "example.com:443", 0); err == nil || fallbackCalls.Load() != 0 {
		t.Fatal("old tunnel attempted fallback")
	}
	if err := a.commitRoutingMode("rule", "direct"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.fallbackDial(context.Background(), "example.com:443", 0); err == nil {
		t.Fatal("old generation allowed after mode round trip")
	}
}

func TestInFlightFallbackCanceledByDirectCommit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(appSupportDir(), 0700); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-release; w.WriteHeader(200) }))
	// Always release handlers before Server.Close, including fatal assertions.
	defer fallback.Close()
	defer unblock()
	cfg := defaultConfig()
	cfg.FallbackAddr = strings.TrimPrefix(fallback.URL, "http://")
	a := &App{cfg: cfg, log: log.New(io.Discard, "", 0), state: &State{}, mode: ModeFallback, transports: map[string]*http.Transport{}}
	completed := make(chan error, 1)
	go func() {
		resp, err := a.fallbackRoundTrip(httptest.NewRequest("GET", "http://destination.test/", nil), 0)
		if resp != nil {
			resp.Body.Close()
		}
		completed <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request not started")
	}
	commit := make(chan error, 1)
	go func() { commit <- a.commitRoutingMode("direct", "rule") }()
	select {
	case err := <-commit:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("direct commit blocked by pending response headers")
	}
	select {
	case err := <-completed:
		if err == nil {
			t.Fatal("stale attempt unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("attempt did not cancel")
	}
}

func TestEstablishedFallbackHTTPStreamSurvivesDirectCommit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "survived")
	}))
	defer server.Close()
	defer unblock()
	cfg := defaultConfig()
	cfg.FallbackAddr = strings.TrimPrefix(server.URL, "http://")
	a := &App{cfg: cfg, log: log.New(io.Discard, "", 0), state: &State{}, transports: map[string]*http.Transport{}}
	resp, err := a.fallbackRoundTrip(httptest.NewRequest("GET", "http://destination.test/", nil), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := a.commitRoutingMode("direct", "rule"); err != nil {
		t.Fatal(err)
	}
	unblock()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "survived" {
		t.Fatalf("established stream canceled: %q %v", body, err)
	}
}

func TestEstablishedFallbackTunnelSurvivesDirectCommit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
		io.Copy(c, c)
	}))
	defer server.Close()
	cfg := defaultConfig()
	cfg.FallbackAddr = strings.TrimPrefix(server.URL, "http://")
	a := &App{cfg: cfg, log: log.New(io.Discard, "", 0), state: &State{}}
	c, _, err := a.fallbackDial(context.Background(), "destination.test:443", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Second))
	if err := a.commitRoutingMode("direct", "rule"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(c, "ping"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("established tunnel canceled: %q %v", buf, err)
	}
}

func TestStaleHealthProbeCannotCommitAfterRoutingChange(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(appSupportDir(), 0700); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-release; w.WriteHeader(500) }))
	defer primary.Close()
	cfg := defaultConfig()
	cfg.PrimaryAddr = strings.TrimPrefix(primary.URL, "http://")
	cfg.ProbeURL = "http://probe.test/"
	cfg.RequireJapan = false
	a := &App{cfg: cfg, log: log.New(io.Discard, "", 0), state: &State{}, mode: ModePrimary, primaryFails: 1}
	done := make(chan struct{})
	go func() { a.healthProbe(); close(done) }()
	<-started
	if err := a.commitRoutingMode("direct", "rule"); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	mode, _ := a.snapshotMode()
	if mode != ModeProbing || a.primaryFails != 0 {
		t.Fatalf("stale probe committed: mode=%s failures=%d", mode, a.primaryFails)
	}
	if err := a.commitRoutingMode("rule", "direct"); err != nil {
		t.Fatal(err)
	}
	mode, _ = a.snapshotMode()
	if mode != ModeProbing || a.primaryFails != 0 || !a.nextRecovery.IsZero() {
		t.Fatal("old fallback state survived mode change")
	}
}

func TestStaleRecoveryCannotRestoreFallbackAfterDirect(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(appSupportDir(), 0700); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-release; w.WriteHeader(500) }))
	defer primary.Close()
	cfg := defaultConfig()
	cfg.PrimaryAddr = strings.TrimPrefix(primary.URL, "http://")
	cfg.ProbeURL = "http://probe.test/"
	cfg.RequireJapan = false
	cfg.RecoveryInitSec = 1
	a := &App{cfg: cfg, log: log.New(io.Discard, "", 0), state: &State{}, mode: ModeFallback, lastSwitch: time.Now().Add(-time.Hour), nextRecovery: time.Now().Add(-time.Second), recoveryDelay: time.Second}
	done := make(chan struct{})
	go func() { a.attemptRecovery(); close(done) }()
	<-started
	if err := a.commitRoutingMode("direct", "rule"); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	mode, _ := a.snapshotMode()
	if mode != ModeProbing || !a.nextRecovery.IsZero() || a.fallbackReason != "" {
		t.Fatalf("stale recovery committed: %s %s", mode, a.fallbackReason)
	}
}

func TestRoutingModeAPIAndRecovery(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(appSupportDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(engineDataDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(engineConfigPath(), []byte("secret: test-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	mode, selected, fail := "rule", "DIRECT", false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Error("missing controller authentication")
		}
		switch {
		case r.URL.Path == "/configs" && r.Method == "GET":
			json.NewEncoder(w).Encode(map[string]string{"mode": mode})
		case r.URL.Path == "/configs" && r.Method == "PATCH":
			var b struct {
				Mode string `json:"mode"`
			}
			json.NewDecoder(r.Body).Decode(&b)
			if fail {
				http.Error(w, "fail", 500)
				return
			}
			mode = b.Mode
			w.WriteHeader(204)
		case r.URL.Path == "/proxies" && r.Method == "GET":
			json.NewEncoder(w).Encode(proxySnapshot{Proxies: map[string]proxyInfo{"GLOBAL": {Type: "Selector", All: []string{"DIRECT", "主代理"}, Now: selected}}})
		case strings.HasPrefix(r.URL.Path, "/proxies/") && r.Method == "PUT":
			var b struct {
				Name string `json:"name"`
			}
			json.NewDecoder(r.Body).Decode(&b)
			selected = b.Name
			w.WriteHeader(204)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cfg := defaultConfig()
	cfg.Controller = strings.TrimPrefix(srv.URL, "http://")
	cfg.ProbeTimeoutSec = 1
	a := &App{cfg: cfg, log: log.New(io.Discard, "", 0), state: &State{}, mode: ModeFallback}
	h, err := a.nodeAPIHandler()
	if err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(filepath.Join(appSupportDir(), "api-token"))
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, body, auth, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://"+cfg.HTTPListen+"/api/routing-mode", bytes.NewBufferString(body))
		r.URL.Scheme = ""
		r.URL.Host = ""
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("Authorization", auth)
		r.Header.Set("Content-Type", "application/json")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	auth := "Bearer " + strings.TrimSpace(string(token))
	if w := call("GET", "", "", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := call("GET", "", auth, "https://evil"); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w := call("PUT", `{"mode":"invalid"}`, auth, ""); w.Code != 400 {
		t.Fatal(w.Code)
	}
	a.aiHealthy, a.aiMS, a.aiCode = true, 23, 401
	generation := a.engineGeneration
	if w := call("PUT", `{"mode":"global"}`, auth, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"health_mode":"probing"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if a.aiHealthy || a.aiMS != 0 || a.aiCode != 0 || a.engineGeneration <= generation {
		t.Fatal("route switch retained AI health")
	}
	st, err := readState()
	if err != nil || st.AIHealthy {
		t.Fatalf("persisted health: %+v %v", st, err)
	}
	mu.Lock()
	if mode != "global" || selected != "主代理" {
		t.Fatal(mode, selected)
	}
	mode = "rule"
	selected = "DIRECT"
	mu.Unlock()
	a.engineMu.Lock()
	err = a.restoreRoutingModeLocked()
	a.engineMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if mode != "global" || selected != "主代理" {
		t.Fatal(mode, selected)
	}
	fail = true
	mu.Unlock()
	a.aiHealthy, a.aiMS, a.aiCode = true, 23, 401
	if w := call("PUT", `{"mode":"direct"}`, auth, ""); w.Code != 502 || !strings.Contains(w.Body.String(), `"preference_changed":false`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if a.aiHealthy || a.aiMS != 0 || a.aiCode != 0 {
		t.Fatal("failed route change retained AI health")
	}
	if got, err := readRoutingMode(); err != nil || got != "global" {
		t.Fatal(got, err)
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	if w := call("PUT", `{"mode":"direct"}`, auth, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"fallback_uncontrolled":false`) {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestDirectRoutingBypassesFallbackAndKeepsBlocking(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(appSupportDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := saveRoutingMode("direct"); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, e := ln.Accept()
		if e == nil {
			c.Close()
		}
	}()
	a := &App{cfg: defaultConfig(), mode: ModeFallback, log: log.New(io.Discard, "", 0), state: &State{}, transports: map[string]*http.Transport{}, blockedDomains: map[string]struct{}{"blocked.example": {}}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, label, err := a.dialForTarget(ctx, ln.Addr().String())
	if err != nil || label != "direct" {
		t.Fatal(label, err)
	}
	c.Close()
	if a.transportFor("direct").Proxy != nil {
		t.Fatal("direct transport uses a proxy")
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "direct-ok") }))
	defer origin.Close()
	req := httptest.NewRequest(http.MethodGet, origin.URL, nil)
	response := httptest.NewRecorder()
	a.handleHTTP(response, req)
	if response.Code != 200 || response.Body.String() != "direct-ok" {
		t.Fatalf("direct HTTP: %d %s", response.Code, response.Body.String())
	}
	if err := os.WriteFile(routingModePath(), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	a.handleHTTP(response, req)
	if response.Code != 503 {
		t.Fatalf("invalid preference: %d", response.Code)
	}
	r := httptest.NewRequest("CONNECT", "http://blocked.example:443", nil)
	r.Host = "blocked.example:443"
	w := httptest.NewRecorder()
	a.handleConnect(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
}
