package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func waitCaptureEvents(t *testing.T, want int) []byte {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(filepath.Join(captureDir(), "events.json"))
		var events []captureEvent
		if err == nil && json.Unmarshal(b, &events) == nil && len(events) == want {
			return b
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("capture events not persisted")
	return nil
}

func TestCaptureHTTPDoesNotPersistSensitiveFields(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(appSupportDir(), 0700); err != nil {
		t.Fatal(err)
	}
	a := &App{blockedDomains: map[string]struct{}{"example.com": {}}}
	w := httptest.NewRecorder()
	cfg := httptest.NewRequest("PUT", "/api/capture", strings.NewReader(`{"enabled":true,"domain":"example.com","duration_seconds":60,"max_events":2}`))
	cfg.Header.Set("Content-Type", "application/json")
	a.serveCapture(w, cfg)
	if w.Code != 204 {
		t.Fatal(w.Code)
	}
	r := httptest.NewRequest("POST", "http://example.com/private-secret?token=query-secret", strings.NewReader("body-secret"))
	r.Header.Set("Cookie", "cookie-secret")
	r.Header.Set("Authorization", "bearer-secret")
	resp := httptest.NewRecorder()
	a.handleHTTP(resp, r)
	if resp.Code != 403 {
		t.Fatal(resp.Code)
	}
	b := waitCaptureEvents(t, 1)
	for _, s := range []string{"query-secret", "body-secret", "cookie-secret", "bearer-secret"} {
		if bytes.Contains(b, []byte(s)) {
			t.Fatalf("leaked %s", s)
		}
	}
	var recorded []captureEvent
	if err := json.Unmarshal(b, &recorded); err != nil || len(recorded) != 1 || recorded[0].Path != "/private-secret" || recorded[0].Method != "POST" || recorded[0].Status != 403 {
		t.Fatalf("unexpected event: %s, %v", b, err)
	}
	if err := a.capture.shutdown(); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureSettingsAndPrivacy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(appSupportDir(), 0700); err != nil {
		t.Fatal(err)
	}
	a := &App{}
	request := func(method, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, "/api/capture", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		a.serveCapture(w, r)
		return w
	}
	for _, domain := range []string{"*.example.com", "example.com.", "127.0.0.1", "Example.com", "example.com:443", "localhost", "-a.example"} {
		b, _ := json.Marshal(captureConfig{Enabled: true, Domain: domain, DurationSeconds: 2, MaxEvents: 2})
		if w := request("PUT", string(b)); w.Code != 400 {
			t.Fatalf("domain %q: %d", domain, w.Code)
		}
	}
	if w := request("PUT", `{"enabled":true,"domain":"example.com","decrypt":true,"duration_seconds":5,"max_events":2}`); w.Code != 501 {
		t.Fatal(w.Code)
	}
	if w := request("PUT", `{"enabled":true,"domain":"example.com","duration_seconds":5,"max_events":2}`); w.Code != 204 {
		t.Fatal(w.Code, w.Body)
	}
	gen, port, ok := a.capture.match("EXAMPLE.COM:443")
	if !ok || port != "443" {
		t.Fatal("match")
	}
	for _, h := range []string{"sub.example.com:443", "example.com.evil:443", "127.0.0.1:443"} {
		if _, _, ok := a.capture.match(h); ok {
			t.Fatal(h)
		}
	}
	for i := 0; i < 3; i++ {
		a.capture.record(gen, captureEvent{Time: time.Now(), Kind: "http", Port: "80", Method: "GET", Status: 200})
	}
	b := waitCaptureEvents(t, 2)
	var events []captureEvent
	if json.Unmarshal(b, &events) != nil || len(events) != 2 {
		t.Fatalf("events: %s", b)
	}
	st, err := os.Stat(filepath.Join(captureDir(), "events.json"))
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal(st, err)
	}
	if w := request("DELETE", ""); w.Code != 204 {
		t.Fatal(w.Code)
	}
	a.capture.record(gen, captureEvent{Kind: "stale"})
	if _, err := os.Stat(filepath.Join(captureDir(), "events.json")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
func TestCaptureAPIAuth(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(appSupportDir(), 0700); err != nil {
		t.Fatal(err)
	}
	a := &App{cfg: Config{HTTPListen: "127.0.0.1:33333"}}
	base, err := a.nodeAPIHandler()
	if err != nil {
		t.Fatal(err)
	}
	h, err := a.captureAPI(base)
	if err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(filepath.Join(appSupportDir(), "api-token"))
	if err != nil {
		t.Fatal(err)
	}
	call := func(auth, origin, remote string) int {
		r := httptest.NewRequest("GET", "/api/capture", nil)
		r.Host = "127.0.0.1:33333"
		r.RemoteAddr = remote
		r.Header.Set("Authorization", auth)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	auth := "Bearer " + strings.TrimSpace(string(token))
	if call("", "", "127.0.0.1:2") != 401 || call(auth, "https://evil.test", "127.0.0.1:2") != 403 || call(auth, "", "192.0.2.1:2") != 403 || call(auth, "", "127.0.0.1:2") != 200 {
		t.Fatal("API guard")
	}
}

func TestCaptureCleanupAndShutdown(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(captureDir(), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".events-orphan", "events.json", "unrelated"} {
		if err := os.WriteFile(filepath.Join(captureDir(), name), []byte("secret"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := captureRemove(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".events-orphan", "events.json"} {
		if _, err := os.Stat(filepath.Join(captureDir(), name)); !os.IsNotExist(err) {
			t.Fatalf("%s remains: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(captureDir(), "unrelated")); err != nil {
		t.Fatal(err)
	}
	a := &App{}
	if err := a.capture.shutdown(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("PUT", "/api/capture", strings.NewReader(`{"enabled":true,"domain":"example.com","duration_seconds":5,"max_events":2}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.serveCapture(w, r)
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
	if _, err := os.Stat(filepath.Join(captureDir(), "events.json")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestCaptureStorageFailureClearsMemory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(appSupportDir(), 0700); err != nil {
		t.Fatal(err)
	}
	a := &App{}
	r := httptest.NewRequest("PUT", "/api/capture", strings.NewReader(`{"enabled":true,"domain":"example.com","duration_seconds":60,"max_events":2}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.serveCapture(w, r)
	if w.Code != 204 {
		t.Fatal(w.Code)
	}
	gen, _, _ := a.capture.match("example.com:80")
	if err := os.Chmod(captureDir(), 0755); err != nil {
		t.Fatal(err)
	}
	a.capture.record(gen, captureEvent{Path: "/secret"})
	deadline := time.Now().Add(3 * time.Second)
	for {
		a.capture.mu.Lock()
		done := a.capture.storageError && !a.capture.writerActive
		enabled, count := a.capture.enabled, len(a.capture.events)
		a.capture.mu.Unlock()
		if done {
			if enabled || count != 0 {
				t.Fatal("failure retained capture")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no storage error")
		}
		time.Sleep(time.Millisecond)
	}
	if err := os.Chmod(captureDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := a.capture.shutdown(); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureEventJSONFields(t *testing.T) {
	b, err := json.Marshal(captureEvent{Kind: "connection", Port: "443", Result: "completed"})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`"kind":"connection"`, `"port":"443"`, `"result":"completed"`} {
		if !bytes.Contains(b, []byte(s)) {
			t.Fatalf("missing %s: %s", s, b)
		}
	}
}

func TestCaptureDiskLockDoesNotBlockProxy(t *testing.T) {
	a := &App{}
	a.capture.mu.Lock()
	a.capture.enabled = true
	a.capture.domain = "example.com"
	a.capture.expires = time.Now().Add(time.Minute)
	a.capture.maxEvents = 4
	a.capture.mu.Unlock()
	a.capture.ioMu.Lock()
	done := make(chan struct{})
	go func() {
		gen, _, ok := a.capture.match("example.com:80")
		if ok {
			a.capture.record(gen, captureEvent{Method: "GET"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		a.capture.ioMu.Unlock()
		t.Fatal("proxy blocked on disk lock")
	}
	a.capture.mu.Lock()
	a.capture.closed = true
	a.capture.stop(true)
	a.capture.mu.Unlock()
	a.capture.ioMu.Unlock()
}

type blockingCaptureWriter struct {
	header  http.Header
	entered chan struct{}
	release chan struct{}
}

func (w *blockingCaptureWriter) Header() http.Header { return w.header }
func (w *blockingCaptureWriter) WriteHeader(int)     {}
func (w *blockingCaptureWriter) Write(p []byte) (int, error) {
	close(w.entered)
	<-w.release
	return len(p), nil
}

func TestCaptureGETDoesNotHoldStateLockDuringWrite(t *testing.T) {
	a := &App{}
	w := &blockingCaptureWriter{header: make(http.Header), entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() { a.serveCapture(w, httptest.NewRequest("GET", "/api/capture", nil)); close(done) }()
	<-w.entered
	matched := make(chan struct{})
	go func() { a.capture.match("example.com:80"); close(matched) }()
	select {
	case <-matched:
	case <-time.After(time.Second):
		close(w.release)
		<-done
		t.Fatal("GET held state lock during response write")
	}
	close(w.release)
	<-done
}

func TestCaptureShutdownRacesEnable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(appSupportDir(), 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		a := &App{}
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			r := httptest.NewRequest("PUT", "/api/capture", strings.NewReader(`{"enabled":true,"domain":"example.com","duration_seconds":60,"max_events":2}`))
			r.Header.Set("Content-Type", "application/json")
			a.serveCapture(httptest.NewRecorder(), r)
		}()
		go func() {
			defer wg.Done()
			<-start
			if err := a.capture.shutdown(); err != nil {
				t.Error(err)
			}
		}()
		close(start)
		wg.Wait()
		if _, err := os.Stat(filepath.Join(captureDir(), "events.json")); !os.IsNotExist(err) {
			t.Fatalf("capture survived shutdown: %v", err)
		}
	}
}

func TestCaptureExpiryAndConcurrentStop(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(appSupportDir(), 0700); err != nil {
		t.Fatal(err)
	}
	a := &App{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/api/capture", bytes.NewBufferString(`{"enabled":true,"domain":"example.com","duration_seconds":1,"max_events":8}`))
	r.Header.Set("Content-Type", "application/json")
	a.serveCapture(w, r)
	if w.Code != 204 {
		t.Fatal(w.Code)
	}
	gen, _, _ := a.capture.match("example.com:443")
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				a.capture.record(gen, captureEvent{Kind: "connection"})
			}
		}()
	}
	wg.Wait()
	time.Sleep(1200 * time.Millisecond)
	a.capture.mu.Lock()
	enabled := a.capture.enabled
	a.capture.mu.Unlock()
	if enabled {
		t.Fatal("still enabled")
	}
	if _, err := os.Stat(filepath.Join(captureDir(), "events.json")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
