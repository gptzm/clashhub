package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Capture is deliberately opt-in and never restores an active session on restart.
// Paths may themselves contain sensitive data; queries, headers and bodies are never recorded.
type captureEvent struct {
	Time       time.Time `json:"time"`
	Kind       string    `json:"kind"`
	Port       string    `json:"port"`
	Result     string    `json:"result"`
	DurationMS int64     `json:"duration_ms"`
	Upload     int64     `json:"upload_bytes"`
	Download   int64     `json:"download_bytes"`
	Method     string    `json:"method,omitempty"`
	Path       string    `json:"path,omitempty"`
	Status     int       `json:"status,omitempty"`
}
type captureState struct {
	mu           sync.Mutex
	ioMu         sync.Mutex
	closed       bool
	writerActive bool
	revision     uint64
	enabled      bool
	domain       string
	generation   uint64
	expires      time.Time
	maxEvents    int
	events       []captureEvent
	timer        *time.Timer
	storageError bool
}
type captureConfig struct {
	Enabled         bool   `json:"enabled"`
	Domain          string `json:"domain"`
	Decrypt         bool   `json:"decrypt"`
	DurationSeconds int    `json:"duration_seconds"`
	MaxEvents       int    `json:"max_events"`
}

func captureDomain(s string) bool {
	if len(s) == 0 || len(s) > 253 || s != strings.ToLower(s) || net.ParseIP(s) != nil {
		return false
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return false
	}
	for _, p := range parts {
		if len(p) == 0 || len(p) > 63 || p[0] == '-' || p[len(p)-1] == '-' {
			return false
		}
		for _, c := range p {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
func captureAuthority(target string) (string, string) {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		host = target
		port = ""
	}
	return strings.ToLower(host), port
}
func (c *captureState) match(target string) (uint64, string, bool) {
	h, p := captureAuthority(target)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.enabled || !time.Now().Before(c.expires) || h != c.domain {
		return 0, "", false
	}
	return c.generation, p, true
}
func captureDir() string { return filepath.Join(appSupportDir(), "capture") }
func captureRemove() error {
	st, err := os.Lstat(captureDir())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode().Perm() != 0700 || st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("capture directory must be private")
	}
	entries, err := os.ReadDir(captureDir())
	if err != nil {
		return err
	}
	var result error
	for _, entry := range entries {
		if entry.Name() == "events.json" || strings.HasPrefix(entry.Name(), ".events-") {
			if err := os.Remove(filepath.Join(captureDir(), entry.Name())); err != nil && !os.IsNotExist(err) {
				result = errors.Join(result, err)
			}
		}
	}
	return result
}
func capturePersist(events []captureEvent) error {
	dir := captureDir()
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() || st.Mode().Perm() != 0700 || st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("capture directory must be private")
	}
	b, err := json.Marshal(events)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".events-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, "events.json"))
}
func (c *captureState) record(gen uint64, e captureEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.enabled || c.closed || gen != c.generation || !time.Now().Before(c.expires) {
		return
	}
	c.events = append(c.events, e)
	if len(c.events) > c.maxEvents {
		c.events = append([]captureEvent(nil), c.events[len(c.events)-c.maxEvents:]...)
	}
	c.revision++
	if !c.writerActive {
		c.writerActive = true
		go c.flush()
	}
}

// Disk operations are serialized separately, never under the proxy's state lock.
func (c *captureState) flush() {
	c.ioMu.Lock()
	defer c.ioMu.Unlock()
	for {
		c.mu.Lock()
		if !c.enabled || c.closed {
			c.writerActive = false
			c.mu.Unlock()
			return
		}
		gen, rev := c.generation, c.revision
		events := append([]captureEvent{}, c.events...)
		c.mu.Unlock()
		err := capturePersist(events)
		c.mu.Lock()
		if err != nil {
			c.storageError = true
			c.stop(true)
			c.writerActive = false
			c.mu.Unlock()
			if captureRemove() != nil {
				c.mu.Lock()
				c.storageError = true
				c.mu.Unlock()
			}
			return
		}
		if gen != c.generation || rev == c.revision || c.closed {
			c.writerActive = false
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
	}
}

// stop only changes memory; callers hold mu and serialize disk cleanup with ioMu.
func (c *captureState) stop(clear bool) {
	c.enabled = false
	c.generation++
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	if clear {
		c.events = nil
	}
}

func (c *captureState) shutdown() error {
	c.mu.Lock()
	c.closed = true
	c.stop(true)
	c.mu.Unlock()
	c.ioMu.Lock()
	defer c.ioMu.Unlock()
	return captureRemove()
}
func (a *App) captureAPI(next http.Handler) (http.Handler, error) {
	host, port, err := net.SplitHostPort(a.cfg.HTTPListen)
	if err != nil {
		return nil, err
	}
	token, err := os.ReadFile(filepath.Join(appSupportDir(), "api-token"))
	if err != nil {
		return nil, err
	}
	auth := "Bearer " + strings.TrimSpace(string(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.IsAbs() || r.URL.Path != "/api/capture" {
			next.ServeHTTP(w, r)
			return
		}
		h, p, e := net.SplitHostPort(r.Host)
		remote, _, re := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(remote)
		if e != nil || h != host || p != port || re != nil || ip == nil || !ip.IsLoopback() || r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" {
			http.Error(w, "forbidden", 403)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(auth)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		a.serveCapture(w, r)
	}), nil
}
func (a *App) serveCapture(w http.ResponseWriter, r *http.Request) {
	c := &a.capture
	switch r.Method {
	case http.MethodGet:
		c.mu.Lock()
		status := struct {
			Enabled          bool           `json:"enabled"`
			Domain           string         `json:"domain"`
			Expires          time.Time      `json:"expires"`
			MaxEvents        int            `json:"max_events"`
			Events           []captureEvent `json:"events"`
			StorageError     bool           `json:"storage_error"`
			DecryptSupported bool           `json:"decrypt_supported"`
		}{c.enabled, c.domain, c.expires, c.maxEvents, append([]captureEvent{}, c.events...), c.storageError, false}
		c.mu.Unlock()
		_ = json.NewEncoder(w).Encode(status)
	case http.MethodDelete:
		c.ioMu.Lock()
		c.mu.Lock()
		c.stop(true)
		c.mu.Unlock()
		err := captureRemove()
		if err != nil {
			c.mu.Lock()
			c.storageError = true
			c.mu.Unlock()
		}
		c.ioMu.Unlock()
		if err != nil {
			http.Error(w, "clear failed", 500)
			return
		}
		w.WriteHeader(204)
	case http.MethodPut:
		if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
			http.Error(w, "JSON required", 415)
			return
		}
		var cfg captureConfig
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		d.DisallowUnknownFields()
		if d.Decode(&cfg) != nil || d.Decode(&struct{}{}) != io.EOF {
			http.Error(w, "invalid request", 400)
			return
		}
		if cfg.Decrypt {
			http.Error(w, "HTTPS decryption is not implemented", 501)
			return
		}
		if cfg.Enabled && (!captureDomain(cfg.Domain) || cfg.DurationSeconds < 1 || cfg.DurationSeconds > 3600 || cfg.MaxEvents < 1 || cfg.MaxEvents > 1000) {
			http.Error(w, "invalid capture settings", 400)
			return
		}
		c.ioMu.Lock()
		c.mu.Lock()
		if c.closed || a.shuttingDown.Load() {
			c.mu.Unlock()
			c.ioMu.Unlock()
			http.Error(w, "shutting down", 503)
			return
		}
		c.stop(true)
		c.domain = ""
		c.expires = time.Time{}
		c.maxEvents = 0
		c.storageError = false
		c.mu.Unlock()
		err := captureRemove()
		if err == nil && cfg.Enabled {
			err = capturePersist([]captureEvent{})
		}
		c.mu.Lock()
		if err != nil || c.closed || a.shuttingDown.Load() {
			c.storageError = err != nil
			c.mu.Unlock()
			cleanupErr := captureRemove()
			c.ioMu.Unlock()
			if cleanupErr != nil {
				c.mu.Lock()
				c.storageError = true
				c.mu.Unlock()
			}
			if err != nil || cleanupErr != nil {
				http.Error(w, "capture storage unavailable or cleanup failed", 500)
			} else {
				http.Error(w, "shutting down", 503)
			}
			return
		}
		if cfg.Enabled {
			c.enabled = true
			c.domain = cfg.Domain
			c.maxEvents = cfg.MaxEvents
			c.expires = time.Now().Add(time.Duration(cfg.DurationSeconds) * time.Second)
			gen := c.generation
			c.timer = time.AfterFunc(time.Duration(cfg.DurationSeconds)*time.Second, func() {
				c.ioMu.Lock()
				defer c.ioMu.Unlock()
				c.mu.Lock()
				if c.generation != gen {
					c.mu.Unlock()
					return
				}
				c.stop(true)
				c.mu.Unlock()
				if captureRemove() != nil {
					c.mu.Lock()
					c.storageError = true
					c.mu.Unlock()
				}
			})
		}
		c.mu.Unlock()
		c.ioMu.Unlock()
		w.WriteHeader(204)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		http.Error(w, "method not allowed", 405)
	}
}

type captureResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *captureResponseWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}
func (w *captureResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

type captureReadCloser struct {
	io.ReadCloser
	count *atomic.Int64
}

func (r *captureReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.count.Add(int64(n))
	return n, err
}
func captureMethod(m string) string {
	switch m {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		return m
	}
	return "OTHER"
}
