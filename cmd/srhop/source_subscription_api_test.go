package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCustomSubscriptionSaveAndEditKeepsURLPrivate(t *testing.T) {
	home := fakeHome(t)
	writeConfigFile(t, home, defaultConfig())
	const secretURL = "https://provider.example/subscription?token=private-token"
	var registered atomic.Int32
	s := &statsIntegration{registerSubscription: func(_ context.Context, src SourceConfig, refresh bool) bool {
		if src.Type != SourceTypeSubscription || !refresh {
			t.Errorf("unexpected registration: %+v refresh=%v", src, refresh)
		}
		registered.Add(1)
		return true
	}}
	urlValue := secretURL
	body, _ := json.Marshal(subscriptionSourceRequest{Action: "save", Label: "My provider", URL: &urlValue})
	req := httptest.NewRequest(http.MethodPost, "/api/sources/subscriptions", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.serveSubscriptionSources(w, req)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), secretURL) || strings.Contains(w.Body.String(), "private-token") {
		t.Fatalf("response leaked URL or failed: %d %s", w.Code, w.Body.String())
	}
	var saved struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil || !strings.HasPrefix(saved.ID, customSubscriptionSourcePrefix) {
		t.Fatalf("invalid save response: %+v err=%v", saved, err)
	}
	urlPath := subscriptionURLCachePath(saved.ID)
	gotURL, err := os.ReadFile(urlPath)
	if err != nil || strings.TrimSpace(string(gotURL)) != secretURL {
		t.Fatalf("private URL cache missing: %v", err)
	}
	if mode := (mustStat(t, urlPath).Mode().Perm()); mode != 0600 {
		t.Fatalf("URL permissions = %o", mode)
	}
	configBytes, err := os.ReadFile(configPath())
	if err != nil || strings.Contains(string(configBytes), secretURL) || strings.Contains(string(configBytes), "private-token") {
		t.Fatal("subscription URL was persisted in config.json")
	}
	src, ok, err := readCustomSubscriptionSource(configPath(), saved.ID)
	if err != nil || !ok || src.Label != "My provider" || src.Type != SourceTypeSubscription {
		t.Fatalf("source was not persisted: %+v ok=%v err=%v", src, ok, err)
	}
	// A blank URL during edit retains the saved secret and stable source id.
	editBody, _ := json.Marshal(subscriptionSourceRequest{Action: "save", ID: saved.ID, Label: "Renamed provider"})
	editReq := httptest.NewRequest(http.MethodPost, "/api/sources/subscriptions", strings.NewReader(string(editBody)))
	editReq.Header.Set("Content-Type", "application/json")
	editW := httptest.NewRecorder()
	s.serveSubscriptionSources(editW, editReq)
	if editW.Code != http.StatusOK {
		t.Fatalf("edit failed: %d %s", editW.Code, editW.Body.String())
	}
	gotURL, err = os.ReadFile(urlPath)
	if err != nil || strings.TrimSpace(string(gotURL)) != secretURL {
		t.Fatal("blank URL edit did not preserve the saved URL")
	}
	if registered.Load() != 2 {
		t.Fatalf("runtime registrations = %d", registered.Load())
	}
}

func TestCustomSubscriptionRejectsMissingURLAndCredentialsInAuthority(t *testing.T) {
	home := fakeHome(t)
	writeConfigFile(t, home, defaultConfig())
	s := &statsIntegration{}
	for _, input := range []subscriptionSourceRequest{
		{Action: "save", Label: "Missing URL"},
		{Action: "save", Label: "Credentials", URL: stringPointer("https://user:password@example.com/sub")},
		{Action: "save", Label: "Wrong scheme", URL: stringPointer("file:///tmp/sub.yaml")},
	} {
		b, _ := json.Marshal(input)
		req := httptest.NewRequest(http.MethodPost, "/api/sources/subscriptions", strings.NewReader(string(b)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.serveSubscriptionSources(w, req)
		if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "password") {
			t.Fatalf("input was not safely rejected: code=%d body=%q", w.Code, w.Body.String())
		}
	}
}

func TestCustomSubscriptionRefreshQueuesExistingSource(t *testing.T) {
	home := fakeHome(t)
	cfg := defaultConfig()
	writeConfigFile(t, home, cfg)
	id := customSubscriptionSourcePrefix + "0123456789abcdef"
	if err := writePrivateFile(subscriptionURLCachePath(id), "https://provider.example/sub?token=private\n"); err != nil {
		t.Fatal(err)
	}
	if err := persistCustomSubscriptionSource(configPath(), SourceConfig{ID: id, Label: "Provider", Type: SourceTypeSubscription, Enabled: true}, true); err != nil {
		t.Fatal(err)
	}
	var gotID string
	s := &statsIntegration{registerSubscription: func(_ context.Context, src SourceConfig, refresh bool) bool {
		gotID = src.ID
		return refresh
	}}
	b, _ := json.Marshal(subscriptionSourceRequest{Action: "refresh", ID: id})
	req := httptest.NewRequest(http.MethodPost, "/api/sources/subscriptions", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.serveSubscriptionSources(w, req)
	if w.Code != http.StatusAccepted || gotID != id || !strings.Contains(w.Body.String(), `"status":"queued"`) || strings.Contains(w.Body.String(), "private") {
		t.Fatalf("refresh response: %d %s id=%q", w.Code, w.Body.String(), gotID)
	}
}

type failingRoundTripper struct{}

func (failingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("direct path unavailable")
}

func TestSubscriptionFetchFallsBackToConfiguredProxy(t *testing.T) {
	var proxyCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("subscription-userinfo", "upload=1; download=2; total=10")
		_, _ = io.WriteString(w, "proxies: []\n")
	}))
	defer target.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		if !r.URL.IsAbs() || r.URL.Host == "" {
			http.Error(w, "proxy request was not absolute", 400)
			return
		}
		resp, err := http.Get(r.URL.String())
		if err != nil {
			http.Error(w, "upstream failed", 502)
			return
		}
		defer resp.Body.Close()
		for k, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(k, value)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer proxy.Close()
	direct := &http.Client{Transport: failingRoundTripper{}, Timeout: time.Second}
	proxyAddr := strings.TrimPrefix(proxy.URL, "http://")
	body, info, err := fetchSubscriptionWithProxyRetry(context.Background(), target.URL, http.Header{"User-Agent": {"test"}}, direct, []string{proxyAddr})
	if err != nil || string(body) != "proxies: []\n" || info != "upload=1; download=2; total=10" || proxyCalls.Load() != 1 {
		t.Fatalf("fetch failed: body=%q info=%q err=%v proxyCalls=%d", body, info, err, proxyCalls.Load())
	}
}

func TestGenericSubscriptionRefreshKeepsLastGoodCacheOnFetchFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dst := subscriptionYAMLPath(customSubscriptionSourcePrefix + "0123456789abcdef")
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old-good-cache"), 0600); err != nil {
		t.Fatal(err)
	}
	r := newSubscriptionRefresher(SourceConfig{ID: filepath.Base(dst[:len(dst)-len(filepath.Ext(dst))]), Label: "Provider", Type: SourceTypeSubscription}, time.Hour)
	r.resolveURL = func() (string, error) { return "https://provider.invalid/sub?token=private", nil }
	r.client = &http.Client{Transport: failingRoundTripper{}, Timeout: time.Second}
	r.parseStore = func([]byte, string) (int, int, error) {
		t.Fatal("parseStore called after failed fetch")
		return 0, 0, nil
	}
	r.RefreshOnce(context.Background())
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "old-good-cache" || r.Snapshot().Status != "error" {
		t.Fatalf("cache not retained: %q status=%+v err=%v", got, r.Snapshot(), err)
	}
}

func stringPointer(value string) *string { return &value }

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestCustomRefreshDiscardsStaleOrDisabledResult(t *testing.T) {
	for _, disable := range []bool{false, true} {
		t.Run(map[bool]string{false: "changed URL", true: "disabled"}[disable], func(t *testing.T) {
			home := fakeHome(t)
			started, release := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				<-release
				io.WriteString(w, "proxies: []\n")
			}))
			defer server.Close()
			id := "custom-sub-racetest"
			cfg := defaultConfig()
			cfg.Sources = append(cfg.Sources, SourceConfig{ID: id, Label: "test", Type: SourceTypeSubscription, Enabled: true})
			writeConfigFile(t, home, cfg)
			if err := writePrivateFile(subscriptionURLCachePath(id), server.URL); err != nil {
				t.Fatal(err)
			}
			r := newSubscriptionRefresher(cfg.Sources[len(cfg.Sources)-1], time.Hour)
			var writes atomic.Int32
			r.parseStore = func([]byte, string) (int, int, error) { writes.Add(1); return 1, 0, nil }
			done := make(chan struct{})
			go func() { r.RefreshOnce(context.Background()); close(done) }()
			<-started
			customSubscriptionCommitMu.Lock()
			if disable {
				cfg.Sources[len(cfg.Sources)-1].Enabled = false
				writeConfigFile(t, home, cfg)
			} else if err := writePrivateFile(subscriptionURLCachePath(id), "https://example.com/new"); err != nil {
				t.Fatal(err)
			}
			customSubscriptionCommitMu.Unlock()
			close(release)
			<-done
			if writes.Load() != 0 {
				t.Fatal("stale result was committed")
			}
		})
	}
}
