package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSubscriptionReadOnlyAndMatching(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "Library", "Application Support", "com.follow.clash")
	if err := os.MkdirAll(filepath.Join(root, "profiles"), 0700); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(root, "database.sqlite")
	script := `import sqlite3,sys
c=sqlite3.connect(sys.argv[1]); c.execute('create table profiles (id integer, label text, subscription_info text, last_update_date integer, url text, age_secret_key text)'); c.execute('insert into profiles values (?,?,?,?,?,?)',(123,'Example','{"upload":1,"download":2,"total":100,"expire":1700000000,"secret":"hidden"}',42,'https://secret.example','secret-key')); c.commit(); c.close()`
	if b, err := exec.Command("python3", "-c", script, db).CombinedOutput(); err != nil {
		t.Fatalf("fixture %s %v", b, err)
	}
	paths := []string{filepath.Join(root, "profiles", "123.yaml"), filepath.Join(root, "profiles", "123.yaml"), filepath.Join(root, "profiles", "bad.yaml"), filepath.Join(home, "other", "123.yaml"), filepath.Join(home, "Library", "Application Support", "com.abysswalker.office91", "profiles", "456.yaml")}
	targets := subscriptionTargets(paths, home)
	if len(targets) != 2 {
		t.Fatalf("targets: %#v", targets)
	}
	p := readSubscription(targets[0])
	if p.Status != "ok" || p.Name == nil || *p.Name != "Example" || p.SubscriptionInfo.Total == nil || *p.SubscriptionInfo.Total != 100 || p.LastUpdateDate == nil || *p.LastUpdateDate != 42 {
		t.Fatalf("profile: %#v", p)
	}
	b, _ := json.Marshal(p)
	if strings.Contains(string(b), "secret") || strings.Contains(string(b), "https") {
		t.Fatal("secret leaked")
	}
	if p = readSubscription(targets[1]); p.Status != "unknown" || p.Name != nil {
		t.Fatalf("missing database: %#v", p)
	}
	if p = readSubscription(subscriptionTarget{"flclash", "999", db}); p.Status != "unknown" {
		t.Fatal("missing row should be unknown")
	}
}

func TestSubscriptionCacheAsync(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	gate := make(chan struct{})
	c := &subscriptionCache{targets: []subscriptionTarget{{source: "flclash", id: "1"}}, profiles: []subscriptionProfile{{Source: "flclash", ID: "1", Status: "unknown"}}}
	c.read = func(t subscriptionTarget) subscriptionProfile {
		<-gate
		p := unknownSubscription(t)
		p.Status = "ok"
		return p
	}
	w := httptest.NewRecorder()
	c.serveHTTP(w, httptest.NewRequest("GET", "/api/subscriptions", nil))
	if !strings.Contains(w.Body.String(), `"refreshing":true`) || !strings.Contains(w.Body.String(), `"status":"unknown"`) {
		t.Fatal(w.Body.String())
	}
	close(gate)
	deadline := time.Now().Add(time.Second)
	for {
		c.mu.Lock()
		done := !c.loading
		c.mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("refresh stuck")
		}
		time.Sleep(time.Millisecond)
	}
	w = httptest.NewRecorder()
	c.serveHTTP(w, httptest.NewRequest("GET", "/api/subscriptions", nil))
	if !strings.Contains(w.Body.String(), `"status":"ok"`) {
		t.Fatal(w.Body.String())
	}
	w = httptest.NewRecorder()
	c.serveHTTP(w, httptest.NewRequest(http.MethodPut, "/api/subscriptions", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatal(w.Code)
	}
}

func TestJiuyiDiscoveryIndependentAndFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := defaultConfig()
	cfg.Office91Enabled = true
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "Library", "Application Support", "com.abysswalker.office91")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "database.sqlite")
	script := `import sqlite3,sys
c=sqlite3.connect(sys.argv[1]); c.execute('create table profiles (id integer, label text, subscription_info text, last_update_date integer, url text, age_secret_key text)'); c.execute('insert into profiles values (?,?,?,?,?,?)',(456,'玖壹','{"upload":1,"download":2,"total":100,"expire":1700000000,"private":"sensitive"}',42,'https://secret.example','secret-key')); c.commit(); c.close()`
	if b, err := exec.Command("python3", "-c", script, db).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %s %v", b, err)
	}
	c := newSubscriptionCache(Config{})
	c.refresh()
	if c.discoveryStatus != "ok" || len(c.profiles) != 1 || c.profiles[0].ID != "456" || c.profiles[0].Source != "jiuyi" || c.profiles[0].SubscriptionInfo.Total == nil || *c.profiles[0].SubscriptionInfo.Total != 100 {
		t.Fatalf("discovery: %#v", c.profiles)
	}
	b, _ := json.Marshal(c.profiles)
	if strings.Contains(string(b), "secret") || strings.Contains(string(b), "sensitive") {
		t.Fatal("sensitive data leaked")
	}
	c.targets = []subscriptionTarget{{source: "jiuyi", id: "456", db: db}}
	c.refresh()
	if len(c.profiles) != 1 {
		t.Fatalf("duplicate: %#v", c.profiles)
	}
	c.targets = nil
	if err := os.Remove(db); err != nil {
		t.Fatal(err)
	}
	c.refresh()
	if c.discoveryStatus != "unknown" || len(c.profiles) != 1 || c.profiles[0].Status != "unknown" || c.profiles[0].SubscriptionInfo.Total != nil {
		t.Fatalf("failure: %#v", c.profiles)
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatal("read-only discovery created database")
	}
}

func TestSubscriptionAPIAuthentication(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(appSupportDir(), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.SourceProfiles = nil
	h, err := (&App{cfg: cfg}).nodeAPIHandler()
	if err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(filepath.Join(appSupportDir(), "api-token"))
	if err != nil {
		t.Fatal(err)
	}
	call := func(auth, origin string) int {
		r := httptest.NewRequest("GET", "http://"+cfg.HTTPListen+"/api/subscriptions", nil)
		r.URL.Scheme = ""
		r.URL.Host = ""
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("Authorization", auth)
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if got := call("", ""); got != 401 {
		t.Fatal(got)
	}
	if got := call("Bearer "+strings.TrimSpace(string(token)), "https://evil.example"); got != 403 {
		t.Fatal(got)
	}
	if got := call("Bearer "+strings.TrimSpace(string(token)), ""); got != 200 {
		t.Fatal(got)
	}
}
