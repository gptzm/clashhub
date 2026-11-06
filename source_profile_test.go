package main

import (
	"context"
	"encoding/json"
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

// makeFlClashDB creates a minimal FlClash-style database whose profiles table
// holds the given "id|type|label|url|last_update|subscription_info" rows.
func makeFlClashDB(t *testing.T, db string, rows ...string) {
	t.Helper()
	script := `import sqlite3,sys
c=sqlite3.connect(sys.argv[1])
c.execute('create table if not exists profiles (id integer primary key, type text, label text, url text, last_update_date integer, subscription_info text)')
c.execute('delete from profiles')
c.executemany('insert into profiles values (?,?,?,?,?,?)',[tuple(r.split('|')) for r in sys.argv[2:]])
c.commit(); c.close()`
	args := append([]string{"-c", script, db}, rows...)
	if b, err := exec.Command("python3", args...).CombinedOutput(); err != nil {
		t.Fatalf("fixture %s: %v", b, err)
	}
}

// newTestProfileRefresher points a profile refresher at a temporary FlClash
// layout: profile file at profilePath, database and private caches in a
// temporary directory. HOME is isolated before construction because the
// private URL-hash cache is derived from appSupportDir at refresh time.
func newTestProfileRefresher(t *testing.T, sourceID, profilePath string) *profileRefresher {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	r := newProfileRefresher(SourceConfig{ID: sourceID, Label: "大机场 Big Airport", Type: SourceTypeProfile, Path: profilePath, Enabled: true}, time.Hour)
	r.dbPath = filepath.Join(dir, "flclash.sqlite")
	r.backupPath = filepath.Join(dir, "sources", sourceID+".yaml.bak")
	r.userinfoPath = filepath.Join(dir, "sources", sourceID+".userinfo")
	return r
}

func writeTestProfile(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(path, perm)
}

func TestFlClashProfileURLReader(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "database.sqlite")
	makeFlClashDB(t, db,
		"10000001|url|大机场 Big Airport|https://sub1.example.invalid/token-B4|1793376689|{}",
		"111|file|Local|.|0|{}",
		"222|url|Bad|ftp://nope.example/x|0|{}",
		"333|url|Second|https://sub2.example.invalid/token-C7|1793376819|{}",
	)
	urls, err := readFlClashProfileURLs(db)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(urls) != 2 || urls["10000001"] != "https://sub1.example.invalid/token-B4" || urls["333"] != "https://sub2.example.invalid/token-C7" {
		t.Fatalf("urls: %#v", urls)
	}
	// Missing database: classified error, and never created.
	if _, err := readFlClashProfileURLs(filepath.Join(dir, "none.sqlite")); err == nil || err.Error() != "db-missing" {
		t.Fatalf("missing db: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "none.sqlite")); !os.IsNotExist(statErr) {
		t.Fatal("read created the database")
	}
	// Present but schemaless database: classified, not panicked.
	broken := filepath.Join(dir, "broken.sqlite")
	script := `import sqlite3,sys
c=sqlite3.connect(sys.argv[1]); c.execute('create table other (x integer)'); c.commit(); c.close()`
	if b, err := exec.Command("python3", "-c", script, broken).CombinedOutput(); err != nil {
		t.Fatalf("fixture %s: %v", b, err)
	}
	if _, err := readFlClashProfileURLs(broken); err == nil || err.Error() != "no-url" {
		t.Fatalf("broken db: %v", err)
	}
}

func TestProfileRefreshSuccess(t *testing.T) {
	var mu sync.Mutex
	var gotUA string
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		gotUA = r.Header.Get("User-Agent")
		mu.Unlock()
		w.Header().Set("subscription-userinfo", "upload=142782550683; download=16382610231; total=2199023255552; expire=1824713407")
		_, _ = w.Write([]byte(clashYAML))
	}))
	defer srv.Close()

	dir := t.TempDir()
	profilePath := filepath.Join(dir, "profiles", "10000001.yaml")
	writeTestProfile(t, profilePath, "proxies:\n- {name: old, type: http, server: old.example.invalid, port: 1}\n", 0o644)
	r := newTestProfileRefresher(t, "big-airport", profilePath)
	makeFlClashDB(t, r.dbPath, "10000001|url|大机场|"+srv.URL+"/secret-path-9e2c|0|{}")
	var updated int
	r.OnUpdate = func() { updated++ }

	out := captureOutput(t, func() { r.RefreshOnce(context.Background()) })
	if strings.Contains(out, srv.URL) || strings.Contains(out, "secret-path-9e2c") {
		t.Fatal("subscription URL leaked to stdout/stderr")
	}
	mu.Lock()
	ua, count := gotUA, hits
	mu.Unlock()
	if count != 1 || ua != "clash.meta/v1.19.2" {
		t.Fatalf("fetch: hits=%d ua=%q", count, ua)
	}

	st := r.Snapshot()
	if st.Status != "ok" || st.Error != "" || st.Proxies != 3 || st.Rules != 1 || st.LastSuccess == nil {
		t.Fatalf("status: %+v", st)
	}
	// FlClash's source profile is read-only. The fetched payload is stored in
	// srhop's private URL-hash cache, which the generator can consume.
	original, err := os.ReadFile(profilePath)
	if err != nil || string(original) != "proxies:\n- {name: old, type: http, server: old.example.invalid, port: 1}\n" {
		t.Fatalf("source profile changed: %q %v", original, err)
	}
	if info, err := os.Stat(profilePath); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("source profile mode: %v %v", info, err)
	}
	cached, err := os.ReadFile(r.cachePath)
	if err != nil || string(cached) != clashYAML {
		t.Fatalf("private cache: %q %v", cached, err)
	}
	if info, err := os.Stat(r.cachePath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private cache mode: %v %v", info, err)
	}
	if _, err := os.Stat(r.backupPath); !os.IsNotExist(err) {
		t.Fatalf("unexpected source backup: %v", err)
	}
	// The subscription-userinfo header is mirrored privately, 0600.
	ub, err := os.ReadFile(r.userinfoPath)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(r.userinfoPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("userinfo mode: %v %v", info, err)
	}
	var ui profileUserinfo
	if err := json.Unmarshal(ub, &ui); err != nil {
		t.Fatal(err)
	}
	if ui.Upload == nil || *ui.Upload != 142782550683 || ui.Total == nil || *ui.Total != 2199023255552 || ui.Expire == nil || *ui.Expire != 1824713407 || ui.FetchedAt <= 0 {
		t.Fatalf("userinfo: %+v", ui)
	}
	if strings.Contains(string(ub), "secret-path") || strings.Contains(string(ub), "http") {
		t.Fatal("userinfo cache leaked the subscription url")
	}
	// No temporary files are left behind in the source profile directory.
	entries, _ := os.ReadDir(filepath.Dir(profilePath))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".profile-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
	if updated != 1 {
		t.Fatalf("OnUpdate calls: %d", updated)
	}
}

func TestProfileRefreshFailureKeepsProfile(t *testing.T) {
	code := http.StatusOK
	body := []byte(clashYAML)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("subscription-userinfo", "upload=1; download=2; total=3; expire=4")
		w.WriteHeader(code)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	profilePath := filepath.Join(dir, "profiles", "10000002.yaml")
	writeTestProfile(t, profilePath, "proxies:\n- {name: old, type: http, server: old.example.invalid, port: 1}\n", 0o644)
	r := newTestProfileRefresher(t, "ktm-cloud", profilePath)
	makeFlClashDB(t, r.dbPath, "10000002|url|KTM|"+srv.URL+"|0|{}")
	r.RefreshOnce(context.Background())
	if st := r.Snapshot(); st.Status != "ok" {
		t.Fatalf("initial: %+v", st)
	}
	goodProfile, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	goodCache, err := os.ReadFile(r.cachePath)
	if err != nil {
		t.Fatal(err)
	}
	goodUI, err := os.ReadFile(r.userinfoPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.backupPath); !os.IsNotExist(err) {
		t.Fatalf("unexpected source backup: %v", err)
	}

	for _, tc := range []struct {
		name string
		code int
		body []byte
		want string
	}{
		{"http 403", http.StatusForbidden, nil, "http-403"},
		{"non-yaml payload", http.StatusOK, []byte("<html>not yaml</html>\n"), "not-clash"},
		{"empty proxies", http.StatusOK, []byte("proxies: []\n"), "empty"},
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
			cur, err := os.ReadFile(profilePath)
			if err != nil || string(cur) != string(goodProfile) {
				t.Fatalf("profile changed on failure: %q %v", cur, err)
			}
			// The good private cache and userinfo must not be touched by a
			// failed refresh.
			curCache, err := os.ReadFile(r.cachePath)
			if err != nil || string(curCache) != string(goodCache) {
				t.Fatalf("cache changed on failure: %q %v", curCache, err)
			}
			curUI, err := os.ReadFile(r.userinfoPath)
			if err != nil || string(curUI) != string(goodUI) {
				t.Fatalf("userinfo changed on failure: %q %v", curUI, err)
			}
		})
	}

	// The previous profile survives server loss too.
	srv.Close()
	r.RefreshOnce(context.Background())
	if st := r.Snapshot(); st.Status != "error" || st.Error != "network" {
		t.Fatalf("network: %+v", st)
	}
	cur, _ := os.ReadFile(profilePath)
	if string(cur) != string(goodProfile) {
		t.Fatal("profile not preserved after network failure")
	}
	curCache, _ := os.ReadFile(r.cachePath)
	if string(curCache) != string(goodCache) {
		t.Fatal("private cache not preserved after network failure")
	}
}

func TestProfileRefreshMissingURLClasses(t *testing.T) {
	dir := t.TempDir()
	profilePath := filepath.Join(dir, "profiles", "custom.yaml")
	writeTestProfile(t, profilePath, "proxies: []\n", 0o600)
	r := newTestProfileRefresher(t, "big-airport", profilePath)
	// FlClash database absent entirely.
	r.RefreshOnce(context.Background())
	if st := r.Snapshot(); st.Status != "error" || st.Error != "db-missing" {
		t.Fatalf("db-missing: %+v", st)
	}
	// Database without this profile's id (non-FlClash profile file).
	makeFlClashDB(t, r.dbPath, "10000001|url|Big|https://sub.example.invalid/x|0|{}")
	r.RefreshOnce(context.Background())
	if st := r.Snapshot(); st.Status != "error" || st.Error != "no-profile" {
		t.Fatalf("no-profile: %+v", st)
	}
	cur, err := os.ReadFile(profilePath)
	if err != nil || string(cur) != "proxies: []\n" {
		t.Fatalf("profile changed: %q %v", cur, err)
	}
}

func TestProfileRefreshCreatesPrivateFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(clashYAML))
	}))
	defer srv.Close()
	dir := t.TempDir()
	profilePath := filepath.Join(dir, "profiles", "10000001.yaml")
	r := newTestProfileRefresher(t, "big-airport", profilePath)
	makeFlClashDB(t, r.dbPath, "10000001|url|Big|"+srv.URL+"|0|{}")
	r.RefreshOnce(context.Background())
	if st := r.Snapshot(); st.Status != "ok" || st.Proxies != 3 {
		t.Fatalf("status: %+v", st)
	}
	// First materialization has no source file to back up; it writes only the
	// private cache and leaves the source application untouched.
	if _, err := os.Stat(r.backupPath); !os.IsNotExist(err) {
		t.Fatal("backup fabricated without a previous file")
	}
	if _, err := os.Stat(profilePath); !os.IsNotExist(err) {
		t.Fatalf("source profile materialized: %v", err)
	}
	cached, err := os.ReadFile(r.cachePath)
	if err != nil || string(cached) != clashYAML {
		t.Fatalf("private cache: %q %v", cached, err)
	}
	if info, err := os.Stat(r.cachePath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private cache mode: %v %v", info, err)
	}
}

func TestParseSubscriptionUserinfo(t *testing.T) {
	ui, ok := parseSubscriptionUserinfo("upload=142782550683; download=16382610231; total=2199023255552; expire=1824713407")
	if !ok || ui.Upload == nil || *ui.Upload != 142782550683 || ui.Download == nil || *ui.Download != 16382610231 || ui.Total == nil || *ui.Total != 2199023255552 || ui.Expire == nil || *ui.Expire != 1824713407 {
		t.Fatalf("standard: %+v %v", ui, ok)
	}
	// Case-insensitive keys, stray whitespace, unknown parts ignored.
	ui, ok = parseSubscriptionUserinfo("  UPLOAD=1 ;x=9;  Total = 2 ;EXPIRE=3; junk")
	if !ok || ui.Upload == nil || *ui.Upload != 1 || ui.Total == nil || *ui.Total != 2 || ui.Expire == nil || *ui.Expire != 3 {
		t.Fatalf("loose: %+v %v", ui, ok)
	}
	if ui.Download != nil {
		t.Fatalf("download fabricated: %+v", ui)
	}
	// Absent or unusable headers never fabricate counters.
	for _, header := range []string{"", "  ", "upload=abc; total=3x", "download=-5", "foo=1;bar=2"} {
		if ui, ok := parseSubscriptionUserinfo(header); ok {
			t.Fatalf("header %q parsed as %+v", header, ui)
		}
	}
}

func TestProfileUserinfoMerge(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "Library", "Application Support", "com.follow.clash")
	if err := os.MkdirAll(filepath.Join(root, "profiles"), 0o755); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(root, "database.sqlite")
	makeFlClashDB(t, db,
		"123|url|Example|https://secret.example/token-D8|42|{\"upload\":1,\"download\":2,\"total\":100,\"expire\":1700000000}",
	)
	profilePath := filepath.Join(root, "profiles", "123.yaml")
	profileURL := "https://secret.example/token-D8"
	urlDigest := filepath.Base(flclashCachePath("123", profileURL))
	cfg := Config{
		SourceProfiles: []string{profilePath},
		Sources: []SourceConfig{
			{ID: "big-airport", Label: "大机场", Type: SourceTypeProfile, Path: profilePath, Enabled: true},
		},
	}
	writeUserInfo := func(fetchedAt int64) {
		b, err := json.Marshal(profileUserinfo{
			subscriptionInfo: subscriptionInfo{Upload: i64(11), Download: i64(22), Total: i64(33), Expire: i64(44)},
			FetchedAt:        fetchedAt,
			ProfileID:        "123",
			URLDigest:        urlDigest,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := writePrivateFile(profileUserinfoPath("big-airport"), string(b)+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	refresh := func() subscriptionProfile {
		c := newSubscriptionCache(cfg)
		c.refresh()
		if len(c.profiles) != 1 {
			t.Fatalf("profiles: %#v", c.profiles)
		}
		return c.profiles[0]
	}

	// Cache strictly newer than the database record: it wins and is marked.
	writeUserInfo(100)
	p := refresh()
	if p.RefreshSource != "direct" || p.SubscriptionInfo.Upload == nil || *p.SubscriptionInfo.Upload != 11 || *p.SubscriptionInfo.Total != 33 {
		t.Fatalf("fresh cache: %#v", p)
	}
	b, _ := json.Marshal(p)
	if strings.Contains(string(b), "secret") || strings.Contains(string(b), "https") {
		t.Fatal("subscription url leaked through the merge")
	}
	// Cache not newer (older or equal): database record wins.
	writeUserInfo(42)
	p = refresh()
	if p.RefreshSource != "" || p.SubscriptionInfo.Upload == nil || *p.SubscriptionInfo.Upload != 1 || *p.SubscriptionInfo.Total != 100 {
		t.Fatalf("stale cache: %#v", p)
	}
	writeUserInfo(41)
	if p = refresh(); p.RefreshSource != "" || *p.SubscriptionInfo.Upload != 1 {
		t.Fatalf("older cache: %#v", p)
	}
	// Missing or corrupt cache: database record wins.
	if err := os.Remove(profileUserinfoPath("big-airport")); err != nil {
		t.Fatal(err)
	}
	if p = refresh(); p.RefreshSource != "" || *p.SubscriptionInfo.Total != 100 {
		t.Fatalf("missing cache: %#v", p)
	}
	if err := writePrivateFile(profileUserinfoPath("big-airport"), "not json"); err != nil {
		t.Fatal(err)
	}
	if p = refresh(); p.RefreshSource != "" || *p.SubscriptionInfo.Total != 100 {
		t.Fatalf("corrupt cache: %#v", p)
	}
	// Disabled sources never merge a stale cache file.
	cfg.Sources[0].Enabled = false
	writeUserInfo(100)
	if p = refresh(); p.RefreshSource != "" || *p.SubscriptionInfo.Total != 100 {
		t.Fatalf("disabled source: %#v", p)
	}
	// A database row that cannot be read is reported unknown and keeps no
	// fabricated usage data.
	cfg.Sources[0].Enabled = true
	if err := os.Remove(db); err != nil {
		t.Fatal(err)
	}
	if p = refresh(); p.Status != "unknown" || p.RefreshSource != "" || p.SubscriptionInfo.Total != nil {
		t.Fatalf("unreadable database: %#v", p)
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatal("read-only merge created the database")
	}
}

func i64(v int64) *int64 { return &v }

func TestProfileRefreshersFromConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := defaultConfig()
	profileDir := filepath.Join(userHome(), "Library", "Application Support", "com.follow.clash", "profiles")
	bigPath, ktmPath := filepath.Join(profileDir, "10000001.yaml"), filepath.Join(profileDir, "10000002.yaml")
	cfg.Sources = []SourceConfig{
		{ID: "big-airport", Label: "First", Type: SourceTypeProfile, Path: bigPath, Enabled: true},
		{ID: "ktm-cloud", Label: "Second", Type: SourceTypeProfile, Path: ktmPath, Enabled: true},
	}
	refreshers := newProfileRefreshers(cfg)
	if len(refreshers) != 2 || refreshers[0].sourceID != "big-airport" || refreshers[1].sourceID != "ktm-cloud" {
		t.Fatalf("refreshers: %#v", refreshers)
	}
	if refreshers[0].interval != 6*time.Hour || refreshers[0].Interval() != 6*time.Hour {
		t.Fatalf("interval: %v", refreshers[0].interval)
	}
	if refreshers[0].profilePath != bigPath || refreshers[1].profilePath != ktmPath {
		t.Fatalf("profile paths: %v %v", refreshers[0].profilePath, refreshers[1].profilePath)
	}
	if refreshers[0].profileID() != "10000001" {
		t.Fatalf("profile id: %q", refreshers[0].profileID())
	}
	if refreshers[0].backupPath != profileBackupPath("big-airport") || refreshers[0].userinfoPath != profileUserinfoPath("big-airport") {
		t.Fatalf("cache paths: %v %v", refreshers[0].backupPath, refreshers[0].userinfoPath)
	}
	// Disabled and non-profile sources are skipped.
	cfg.Sources[0].Enabled = false
	refreshers = newProfileRefreshers(cfg)
	if len(refreshers) != 1 || refreshers[0].sourceID != "ktm-cloud" {
		t.Fatalf("disabled filter: %#v", refreshers)
	}
	// Unset hours fall back to the default.
	cfg.SubscriptionRefreshHours = 0
	if r := newProfileRefreshers(cfg); len(r) != 1 || r[0].interval != 6*time.Hour {
		t.Fatalf("default interval: %#v", r)
	}
	cfg.SubscriptionRefreshHours = 2
	if r := newProfileRefreshers(cfg); len(r) != 1 || r[0].interval != 2*time.Hour {
		t.Fatalf("configured interval: %#v", r)
	}
	if got := subscriptionRefreshInterval(Config{}); got != 6*time.Hour {
		t.Fatalf("interval default: %v", got)
	}
}
