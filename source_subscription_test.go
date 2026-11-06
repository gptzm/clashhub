package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// makeJiuyiDB creates a minimal 玖壹-style database whose profiles table holds
// the given subscription URL.
func makeJiuyiDB(t *testing.T, db, subURL string) {
	t.Helper()
	script := `import sqlite3,sys
c=sqlite3.connect(sys.argv[1])
c.execute('create table if not exists profiles (id integer, label text, url text)')
c.execute('delete from profiles')
c.execute('insert into profiles values (?,?,?)',(1,'玖壹officeXNB',sys.argv[2]))
c.commit(); c.close()`
	if b, err := exec.Command("python3", "-c", script, db, subURL).CombinedOutput(); err != nil {
		t.Fatalf("fixture %s: %v", b, err)
	}
}

func enableOffice91ModuleForTest(t *testing.T) {
	t.Helper()
	cfg := defaultConfig()
	cfg.Office91Enabled = true
	cfg.Sources = append(cfg.Sources, SourceConfig{ID: sourceIDOffice91, Label: "Office91", Type: SourceTypeSubscription, Enabled: true})
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("save Office91 test config: %v", err)
	}
}

func newTestRefresher(t *testing.T, sourceID string) *subscriptionRefresher {
	t.Helper()
	dir := t.TempDir()
	// Keep the private URL/YAML/userinfo caches under the test home. The
	// production refresher deliberately derives those paths from HOME.
	t.Setenv("HOME", dir)
	if sourceID == sourceIDOffice91 {
		enableOffice91ModuleForTest(t)
	}
	r := newSubscriptionRefresher(SourceConfig{ID: sourceID, Label: "玖壹 Office91", Type: SourceTypeSubscription, Enabled: true}, time.Hour)
	r.dbPath = filepath.Join(dir, "jiuyi.sqlite")
	r.urlPath = filepath.Join(dir, "sources", sourceID+".url")
	r.yamlPath = filepath.Join(dir, "sources", sourceID+".yaml")
	r.userinfoPath = filepath.Join(dir, "sources", sourceID+".userinfo")
	// The production constructor intentionally fails closed until the 玖壹
	// signing protocol is verified. These tests cover generic URL/cache
	// behavior; signed requests have dedicated tests with an injected signer.
	r.signHeaders = nil
	return r
}

// captureOutput swaps the process stdout/stderr for the duration of fn and
// returns everything written, so tests can prove the subscription URL never
// reaches any output stream.
func captureOutput(t *testing.T, fn func()) string {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = w, w
	var once sync.Once
	var got string
	done := make(chan struct{})
	go func() {
		b, _ := io.ReadAll(r)
		got = string(b)
		close(done)
	}()
	defer func() {
		once.Do(func() {
			os.Stdout, os.Stderr = oldOut, oldErr
			w.Close()
			<-done
		})
	}()
	fn()
	once.Do(func() {
		os.Stdout, os.Stderr = oldOut, oldErr
		w.Close()
		<-done
	})
	return got
}

const clashYAML = `proxies:
- {name: 日本 Tokyo-1, type: http, server: jp1.example.invalid, port: 8080}
- {name: 日本 Osaka-2, type: http, server: jp2.example.invalid, port: 8080}
- {name: 香港 HK-1, type: http, server: hk1.example.invalid, port: 8080}
rules:
- MATCH,日本 Tokyo-1
`

func TestJiuyiURLReaderImmutableReadOnly(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "database.sqlite")
	makeJiuyiDB(t, db, "https://sub.example.invalid/token-A9")
	u, err := readJiuyiSubscriptionURL(db)
	if err != nil || u != "https://sub.example.invalid/token-A9" {
		t.Fatalf("read: %q %v", u, err)
	}
	// Missing database: classified error, and never created.
	if _, err := readJiuyiSubscriptionURL(filepath.Join(dir, "none.sqlite")); err == nil || err.Error() != "db-missing" {
		t.Fatalf("missing db: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "none.sqlite")); !os.IsNotExist(statErr) {
		t.Fatal("read created the database")
	}
	// Invalid and empty URL rows are classified, not returned.
	makeJiuyiDB(t, db, "ftp://not-http.example/xyz")
	if _, err := readJiuyiSubscriptionURL(db); err == nil || err.Error() != "invalid-url" {
		t.Fatalf("invalid url: %v", err)
	}
	empty := filepath.Join(dir, "empty.sqlite")
	script := `import sqlite3,sys
c=sqlite3.connect(sys.argv[1]); c.execute('create table profiles (id integer, label text, url text)'); c.commit(); c.close()`
	if b, err := exec.Command("python3", "-c", script, empty).CombinedOutput(); err != nil {
		t.Fatalf("fixture %s: %v", b, err)
	}
	if _, err := readJiuyiSubscriptionURL(empty); err == nil || err.Error() != "no-profile" {
		t.Fatalf("no profile: %v", err)
	}
}

func TestSubscriptionRefreshSuccess(t *testing.T) {
	var gotUA string
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		gotUA = r.Header.Get("User-Agent")
		mu.Unlock()
		w.Header().Set("Content-Type", "text/yaml")
		_, _ = w.Write([]byte(clashYAML))
	}))
	defer srv.Close()
	r := newTestRefresher(t, "office91")
	makeJiuyiDB(t, r.dbPath, srv.URL+"/secret-path-7f3a/")
	var updated int
	r.OnUpdate = func() { updated++ }

	out := captureOutput(t, func() { r.RefreshOnce(context.Background()) })

	// The URL (host:port and secret path fragment) must never be printed.
	if strings.Contains(out, srv.URL) || strings.Contains(out, "secret-path-7f3a") {
		t.Fatal("subscription URL leaked to stdout/stderr")
	}
	mu.Lock()
	ua, count := gotUA, hits
	mu.Unlock()
	// The API-shaped UA is retained for generic fetches. Signature headers are
	// covered separately by source_subscription_sign_test.go.
	if count != 1 || ua != jiuyiUserAgent {
		t.Fatalf("fetch: hits=%d ua=%q", count, ua)
	}

	st := r.Snapshot()
	if st.Status != "ok" || st.Error != "" || st.Proxies != 3 || st.Rules != 1 || st.LastSuccess == nil {
		t.Fatalf("status: %+v", st)
	}
	// Node cache: 0600, private directory, real nodes extracted.
	info, err := os.Stat(r.yamlPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("yaml mode: %v", info.Mode().Perm())
	}
	b, err := os.ReadFile(r.yamlPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"日本 Tokyo-1", "日本 Osaka-2", "香港 HK-1"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("cache missing node %s", want)
		}
	}
	// URL cache is private too.
	uinfo, err := os.Stat(r.urlPath)
	if err != nil {
		t.Fatal(err)
	}
	if uinfo.Mode().Perm() != 0o600 {
		t.Fatalf("url cache mode: %v", uinfo.Mode().Perm())
	}
	ub, _ := os.ReadFile(r.urlPath)
	if !strings.Contains(string(ub), "secret-path-7f3a") {
		t.Fatal("url cache does not hold the subscription url")
	}
	// Status JSON exposes classes only, never the URL.
	sj, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sj), srv.URL) || strings.Contains(string(sj), "secret-path") {
		t.Fatal("status json leaked the url")
	}
	if updated != 1 {
		t.Fatalf("OnUpdate calls: %d", updated)
	}
}

func TestSubscriptionRefreshFailureKeepsLastGoodCache(t *testing.T) {
	code := http.StatusOK
	body := []byte(clashYAML)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	r := newTestRefresher(t, "office91")
	makeJiuyiDB(t, r.dbPath, srv.URL)

	r.RefreshOnce(context.Background())
	if st := r.Snapshot(); st.Status != "ok" {
		t.Fatalf("initial: %+v", st)
	}
	good, err := os.ReadFile(r.yamlPath)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		code  int
		body  []byte
		want  string
		close bool
	}{
		{"http 500", http.StatusInternalServerError, nil, "http-500", false},
		{"non-clash payload", http.StatusOK, []byte("dm1lc3M6Ly9ub3RfY2xhc2g=\nss://opaque\n"), "not-clash", false},
		{"empty clash", http.StatusOK, []byte("proxies: []\n"), "empty", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body = tc.code, tc.body
			out := captureOutput(t, func() { r.RefreshOnce(context.Background()) })
			if strings.Contains(out, srv.URL) {
				t.Fatal("url leaked on failure")
			}
			st := r.Snapshot()
			if st.Status != "error" || st.Error != tc.want {
				t.Fatalf("status: %+v want error %s", st, tc.want)
			}
			cur, err := os.ReadFile(r.yamlPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(cur) != string(good) {
				t.Fatal("last good cache was not preserved")
			}
		})
	}

	// Server unreachable: network class, cache still preserved.
	srv.Close()
	r.RefreshOnce(context.Background())
	if st := r.Snapshot(); st.Status != "error" || st.Error != "network" {
		t.Fatalf("network: %+v", st)
	}
	cur, _ := os.ReadFile(r.yamlPath)
	if string(cur) != string(good) {
		t.Fatal("last good cache was not preserved after network failure")
	}
}

func TestSubscriptionNonClashNeverCreatesCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("this is just plain text, not a clash config\n"))
	}))
	defer srv.Close()
	r := newTestRefresher(t, "office91")
	makeJiuyiDB(t, r.dbPath, srv.URL)
	r.RefreshOnce(context.Background())
	st := r.Snapshot()
	if st.Status != "error" || st.Error != "not-clash" {
		t.Fatalf("status: %+v", st)
	}
	if _, err := os.Stat(r.yamlPath); !os.IsNotExist(err) {
		t.Fatal("cache fabricated for unusable subscription")
	}
}

func TestSubscriptionURLCacheFallbackWhenDBMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(clashYAML))
	}))
	defer srv.Close()
	r := newTestRefresher(t, "office91")
	// No jiuyi database at all; only the private URL cache.
	if err := writePrivateFile(r.urlPath, srv.URL+"\n"); err != nil {
		t.Fatal(err)
	}
	r.RefreshOnce(context.Background())
	if st := r.Snapshot(); st.Status != "ok" || st.Proxies != 3 {
		t.Fatalf("status: %+v", st)
	}
	if info, err := os.Stat(r.urlPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("url cache: %v %v", info, err)
	}
}

func TestSubscriptionRefreshersFromConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := defaultConfig()
	cfg.Office91Enabled = true
	cfg.Sources = append(cfg.Sources, SourceConfig{ID: sourceIDOffice91, Label: "Office91", Type: SourceTypeSubscription, Enabled: true})
	refreshers := newSubscriptionRefreshers(cfg)
	if len(refreshers) != 1 || refreshers[0].sourceID != "office91" {
		t.Fatalf("refreshers: %#v", refreshers)
	}
	if refreshers[0].interval != 6*time.Hour {
		t.Fatalf("interval: %v", refreshers[0].interval)
	}
	if refreshers[0].YAMLPath() != subscriptionYAMLPath("office91") {
		t.Fatalf("yaml path: %v", refreshers[0].YAMLPath())
	}
	cfg.SubscriptionRefreshHours = 0
	cfg.Sources = append(cfg.Sources, SourceConfig{ID: "sub2", Label: "Second", Type: SourceTypeSubscription, Enabled: true})
	refreshers = newSubscriptionRefreshers(cfg)
	if len(refreshers) != 2 || refreshers[0].interval != 6*time.Hour {
		t.Fatalf("refreshers: %#v", refreshers)
	}
	// Disabled subscription sources are skipped.
	cfg.Sources[0].Enabled = false
	if refreshers = newSubscriptionRefreshers(cfg); len(refreshers) != 1 || refreshers[0].sourceID != "sub2" {
		t.Fatalf("disabled filter: %#v", refreshers)
	}
}

func TestOffice91DisabledSkipsRefresherAndRuntimeReads(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	cfg := defaultConfig() // Office91Enabled is false by default.
	if got := newSubscriptionRefreshers(cfg); len(got) != 0 {
		t.Fatalf("disabled Office91 constructed refreshers: %#v", got)
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("save disabled config: %v", err)
	}
	ref := newSubscriptionRefresher(SourceConfig{ID: sourceIDOffice91, Type: SourceTypeSubscription, Enabled: true}, time.Hour)
	called := false
	ref.resolveURL = func() (string, error) {
		called = true
		return "https://should-not-be-read.invalid/sub", nil
	}
	if err := os.MkdirAll(filepath.Dir(ref.urlPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writePrivateFile(ref.urlPath, "https://cached-secret.invalid/sub\n"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(ref.urlPath)
	if err != nil {
		t.Fatal(err)
	}
	ref.RefreshOnce(context.Background())
	if called {
		t.Fatal("disabled module invoked URL resolver")
	}
	if st := ref.Snapshot(); st.Status != "error" || st.Error != "module-disabled" {
		t.Fatalf("status: %+v", st)
	}
	after, err := os.ReadFile(ref.urlPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("disabled module touched URL cache: err=%v", err)
	}
}

func TestDirectTransportNeverUsesProxy(t *testing.T) {
	client := directSubscriptionClient(5 * time.Second)
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport: %T", client.Transport)
	}
	if tr.Proxy != nil {
		t.Fatal("subscription transport must not use any proxy")
	}
	// End to end: an env-var proxy pointing at a dead port must not be used
	// for a loopback fetch.
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer srv.Close()
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("direct fetch through proxy env: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("body: %q", body)
	}
}
