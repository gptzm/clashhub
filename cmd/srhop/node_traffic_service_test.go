package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFallbackTrafficAttributedOnlyOnObservedDelta(t *testing.T) {
	s, err := openNodeTraffic(filepath.Join(t.TempDir(), "traffic.json"), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	var count uint64
	s.fallbackTraffic = func(n uint64) { count += n }
	now := time.Now()
	names := map[string]string{"FALLBACK-33210": nodeHealthName("FALLBACK-33210"), "other": nodeHealthName("other")}
	c := mihomoConnectionSample{ID: "x", Start: now.Add(-time.Second), Chains: []string{"FALLBACK-33210"}, Upload: 100}
	if err := s.apply("one", now, mihomoConnectionsSnapshot{UploadTotal: 100, Connections: []mihomoConnectionSample{c}}, names); err != nil {
		t.Fatal(err)
	}
	c.Upload += 42
	if err := s.apply("one", now.Add(time.Second), mihomoConnectionsSnapshot{UploadTotal: 142, Connections: []mihomoConnectionSample{c}}, names); err != nil {
		t.Fatal(err)
	}
	if count != 42 {
		t.Fatalf("got %d", count)
	}
	c.Chains = []string{"other"}
	c.Upload += 10
	if err := s.apply("one", now.Add(2*time.Second), mihomoConnectionsSnapshot{UploadTotal: 152, Connections: []mihomoConnectionSample{c}}, names); err != nil {
		t.Fatal(err)
	}
	if err := s.apply("two", now.Add(3*time.Second), mihomoConnectionsSnapshot{UploadTotal: 152, Connections: []mihomoConnectionSample{c}}, names); err != nil {
		t.Fatal(err)
	}
	if count != 42 {
		t.Fatalf("ambiguous or restart bytes counted: %d", count)
	}
}

func TestNodeTrafficPersistenceAndRate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "traffic.json")
	s, err := openNodeTraffic(path, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	// Anchor the synthetic clock to the start of the current minute: the
	// +5s/+7s samples must never land in a minute ahead of the real clock,
	// because openNodeTraffic drops future-minute buckets as invalid, which
	// made this test intermittently lose bytes across minute boundaries.
	now := time.Now().UTC().Truncate(time.Minute)
	names := map[string]string{"node secret label": nodeHealthName("node secret label")}
	c := mihomoConnectionSample{ID: "a", Start: now.Add(-time.Minute), Chains: []string{"node secret label"}, Upload: 10, Download: 20}
	if err = s.apply("one", now, mihomoConnectionsSnapshot{UploadTotal: 100, DownloadTotal: 200, Connections: []mihomoConnectionSample{c}}, names); err != nil {
		t.Fatal(err)
	}
	c.Upload = 30
	c.Download = 60
	if err = s.apply("one", now.Add(5*time.Second), mihomoConnectionsSnapshot{UploadTotal: 130, DownloadTotal: 260, Connections: []mihomoConnectionSample{c}}, names); err != nil {
		t.Fatal(err)
	}
	result := s.query(2880, now.Add(5*time.Second))
	if len(result.Nodes) != 1 || result.Nodes[0].Upload != 20 || result.Nodes[0].Download != 40 || !result.Nodes[0].RateAvailable || *result.Nodes[0].UploadBPS != 4 || result.Global.Upload != 30 {
		t.Fatalf("%+v", result)
	}
	if got := s.query(60, now.Add(25*time.Second)); got.Nodes[0].RateAvailable || got.Nodes[0].UploadBPS != nil {
		t.Fatal(got)
	}
	if err = s.flush(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "secret label") {
		t.Fatal("node name persisted")
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0600 {
		t.Fatal(st.Mode())
	}
	restored, err := openNodeTraffic(path, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	restored.names[nodeHealthName("node secret label")] = "node secret label"
	r := restored.query(2880, now.Add(6*time.Second))
	if len(r.Nodes) != 1 || r.Nodes[0].Upload != 20 || r.Nodes[0].RateAvailable {
		t.Fatal(r)
	}
	// A restart baselines pre-existing connections rather than counting them twice.
	if err = restored.apply("two", now.Add(7*time.Second), mihomoConnectionsSnapshot{UploadTotal: 130, DownloadTotal: 260, Connections: []mihomoConnectionSample{c}}, names); err != nil {
		t.Fatal(err)
	}
	if restored.query(2880, now.Add(7*time.Second)).Nodes[0].Upload != 20 {
		t.Fatal("restart double counted")
	}
	restored.mu.Lock()
	restored.buckets[trafficBucketKey{now.Unix()/60 - 2881, nodeHealthName("old")}] = nodeTrafficBytes{1, 1}
	restored.mu.Unlock()
	restored.query(2880, now)
	restored.mu.Lock()
	_, exists := restored.buckets[trafficBucketKey{now.Unix()/60 - 2881, nodeHealthName("old")}]
	restored.mu.Unlock()
	if exists {
		t.Fatal("retention")
	}
}
func TestNodeTrafficAPI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(appSupportDir(), 0700); err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("a", 64)
	if err := os.WriteFile(filepath.Join(appSupportDir(), "api-token"), []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	traffic, _ := openNodeTraffic(filepath.Join(home, "traffic"), t.Logf)
	s := &statsIntegration{nodeTraffic: traffic}
	request := func(method, path, auth string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:33335"+path, nil)
		r.RemoteAddr = "127.0.0.1:1234"
		r.URL.Scheme = ""
		r.URL.Host = ""
		if auth != "" {
			r.Header.Set("Authorization", "Bearer "+auth)
		}
		w := httptest.NewRecorder()
		s.serveHTTP(w, r)
		return w
	}
	if w := request("GET", "/api/node-traffic", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := request("POST", "/api/node-traffic", token); w.Code != 405 {
		t.Fatal(w.Code)
	}
	for _, v := range []string{"0", "2881", "x"} {
		if w := request("GET", "/api/node-traffic?minutes="+v, token); w.Code != 400 {
			t.Fatal(v, w.Code)
		}
	}
	w := request("GET", "/api/node-traffic?minutes=2880", token)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, w.Body.String())
	}
	var r trafficResponse
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil || r.WindowMinutes != 2880 || r.Coverage != "partial" || r.Nodes == nil {
		t.Fatal(r, err)
	}
}
func TestNodeTrafficControllerValidation(t *testing.T) {
	for _, s := range []string{`{}`, `{"uploadTotal":1,"downloadTotal":2,"connections":[{"id":"x","start":"2026-01-01T00:00:00Z"}]}`} {
		var v mihomoConnectionsSnapshot
		if json.Unmarshal([]byte(s), &v) == nil {
			t.Fatal("accepted", s)
		}
	}
	names, err := trafficProxyNames(map[string]proxyInfo{"n": {Type: "Vless"}, "group": {Type: "Selector"}, "DIRECT": {Type: "Direct"}, "unknown": {Type: "NewProtocol"}})
	if err != nil || len(names) != 1 || names["n"] == "" {
		t.Fatal(names, err)
	}
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "http://example.com/", 302) }))
	defer h.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if trafficGet(context.Background(), client, h.URL, "secret", "/connections", &mihomoConnectionsSnapshot{}) == nil {
		t.Fatal("redirect accepted")
	}
}
func TestNodeTrafficCorruptSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "traffic")
	if err := os.WriteFile(path, []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := openNodeTraffic(path, t.Logf); err == nil {
		t.Fatal("corrupt snapshot accepted")
	}
	b, _ := os.ReadFile(path)
	if string(b) != "bad" {
		t.Fatal("corrupt file overwritten")
	}
}

func TestTrafficBySourceAggregation(t *testing.T) {
	defs := []sourceDef{
		{ID: "big-airport", Label: "Big", Type: "profile", Enabled: true},
		{ID: "office91", Label: "Office", Type: "vpn", Enabled: true},
	}
	writeSourcesEnvironment(t, defs, map[string]string{"b1": "big-airport", "o1": "office91"}, "")
	idx := loadSourceIndex()
	if !idx.bound || idx.fallbackPort != 33210 {
		t.Fatalf("%+v", idx)
	}
	s, err := openNodeTraffic(filepath.Join(t.TempDir(), "traffic.json"), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	names := map[string]string{
		"b1":             nodeHealthName("b1"),
		"o1":             nodeHealthName("o1"),
		"FALLBACK-33210": nodeHealthName("FALLBACK-33210"),
		"x1":             nodeHealthName("x1"), // live node without labels entry
	}
	conns := []mihomoConnectionSample{
		{ID: "1", Start: now.Add(-time.Minute), Chains: []string{"b1"}, Upload: 10, Download: 20},
		{ID: "2", Start: now.Add(-time.Minute), Chains: []string{"o1"}, Upload: 100, Download: 200},
		{ID: "3", Start: now.Add(-time.Minute), Chains: []string{"FALLBACK-33210"}, Upload: 1000, Download: 2000},
		{ID: "4", Start: now.Add(-time.Minute), Chains: []string{"x1"}, Upload: 7, Download: 8},
	}
	if err := s.apply("one", now, mihomoConnectionsSnapshot{UploadTotal: 1117, DownloadTotal: 2228, Connections: conns}, names); err != nil {
		t.Fatal(err)
	}
	conns[0].Upload, conns[0].Download = 30, 60
	conns[1].Upload, conns[1].Download = 150, 300
	conns[2].Upload, conns[2].Download = 1500, 3000
	conns[3].Upload, conns[3].Download = 10, 14
	at := now.Add(5 * time.Second)
	if err := s.apply("one", at, mihomoConnectionsSnapshot{UploadTotal: 1690, DownloadTotal: 3374, Connections: conns}, names); err != nil {
		t.Fatal(err)
	}
	// History for a node that no longer exists stays unknown.
	s.mu.Lock()
	s.buckets[trafficBucketKey{at.Unix() / 60, nodeHealthName("ghost")}] = nodeTrafficBytes{5, 5}
	s.mu.Unlock()
	perSource, portNode, unknown := s.trafficBySource(idx, 60, at)
	if perSource["big-airport"] != (nodeTrafficBytes{20, 40}) {
		t.Fatalf("big-airport: %+v", perSource)
	}
	if perSource["office91"] != (nodeTrafficBytes{50, 100}) {
		t.Fatalf("office91 node bytes must not absorb the port fallback: %+v", perSource)
	}
	if portNode != (nodeTrafficBytes{500, 1000}) {
		t.Fatalf("port fallback node bytes: %+v", portNode)
	}
	if unknown != (nodeTrafficBytes{8, 11}) {
		t.Fatalf("unknown bytes (unmapped live node + history): %+v", unknown)
	}
	// Stale labels put all node traffic into unknown; the port fallback node
	// keeps its own attribution because it never depends on labels.
	writeSourcesEnvironment(t, defs, nil, "0123456789abcdef")
	stale := loadSourceIndex()
	if stale.bound {
		t.Fatal("stale generation bound")
	}
	perSource, portNode, unknown = s.trafficBySource(stale, 60, at)
	if len(perSource) != 0 || portNode != (nodeTrafficBytes{500, 1000}) || unknown != (nodeTrafficBytes{78, 151}) {
		t.Fatalf("stale attribution: %+v %+v %+v", perSource, portNode, unknown)
	}
}
