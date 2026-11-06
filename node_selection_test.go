package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// writeSourcesEnvironment installs an isolated HOME with a config.json
// sources manifest, a mihomo config.yaml and a node-labels.json binding.
// generation overrides the computed binding prefix; any non-empty value that
// differs from the real one simulates stale labels. nodeSources maps node
// name -> source id; nodes absent from the map stay unattributed.
func writeSourcesEnvironment(t *testing.T, defs []sourceDef, nodeSources map[string]string, generation string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	const engineConfig = "proxies: []\nrules:\n  - MATCH,DIRECT\n"
	if err := os.MkdirAll(engineDataDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(engineConfigPath(), []byte(engineConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	gen := generation
	if gen == "" {
		sum := sha256.Sum256([]byte(engineConfig))
		gen = hex.EncodeToString(sum[:])[:16]
	}
	if err := os.MkdirAll(appSupportDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	labels := nodeLabelsDoc{Generation: gen, Sources: map[string]string{}, Nodes: map[string]nodeLabelEntry{}}
	for _, d := range defs {
		if d.Label != "" {
			labels.Sources[d.ID] = d.Label
		}
	}
	for name, id := range nodeSources {
		labels.Nodes[name] = nodeLabelEntry{SourceID: id}
	}
	b, err := json.Marshal(labels)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nodeLabelsPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{
		"sources":           defs,
		"base_rules_source": "big-airport",
		"fallback_addr":     "127.0.0.1:33210",
		"vpn_service":       "Shadowrocket",
		"source_profiles":   []string{"/profiles/a.yaml", "/profiles/b.yaml"},
	}
	cb, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath(), cb, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSourcePriorityDefaultsAndClearRules(t *testing.T) {
	idx := sourceIndex{bound: true, defs: []sourceDef{{ID: sourceOffice91}, {ID: "other", Type: "vpn"}}, nodeSource: map[string]string{"home": sourceOffice91, "x": sourceOffice91, "other": "other"}, nodeOriginal: map[string]string{"home": "东京家宽", "x": "JP X20"}}
	for node, want := range map[string]string{"home": "medium", "x": "medium", "other": "high"} {
		if got := effectiveNodePriority(nil, idx, node); got != want {
			t.Fatalf("%s: %s", node, got)
		}
	}
	idx.defs[0].DowngradePatterns = []string{}
	if got := effectiveNodePriority(nil, idx, "home"); got != "medium" {
		t.Fatal(got)
	}
	if got := effectiveNodePriority(map[string]string{"home": "high"}, idx, "home"); got != "high" {
		t.Fatal(got)
	}
	writeSourcesEnvironment(t, []sourceDef{{ID: sourceOffice91, Enabled: true}}, nil, "")
	on := true
	patterns := []string{}
	if err := persistSourcesOrder(configPath(), []sourceUpdateEntry{{ID: sourceOffice91, Enabled: &on, DowngradePatterns: &patterns}}); err != nil {
		t.Fatal(err)
	}
	doc, ok := loadNodeSources()
	if !ok || doc.Sources[0].DowngradePatterns == nil || len(doc.Sources[0].DowngradePatterns) != 0 {
		t.Fatalf("%+v", doc)
	}
	_, got := doc.Sources[0].sourcePriorityDefaults()
	if len(got) != 0 {
		t.Fatalf("rules not cleared: %v", got)
	}
}

func TestGroupStatusShowsSources(t *testing.T) {
	defs := []sourceDef{
		{ID: "big-airport", Label: "大机场", Type: "profile", Enabled: true},
		{ID: "ktm-cloud", Label: "KTM", Type: "subscription", Enabled: true},
	}
	writeSourcesEnvironment(t, defs, map[string]string{"n1": "big-airport", "n3": "ktm-cloud"}, "")
	snap := proxySnapshot{Proxies: map[string]proxyInfo{
		"主代理": {Type: "Selector", All: []string{"n1", "n3", "zz"}, Now: "n3"},
		"n1":  {Type: "Shadowsocks"}, "n3": {Type: "Trojan"}, "zz": {Type: "Trojan"},
	}}
	st, ok := groupStatusWithSources(snap, map[string]string{"主代理": "n3"}, "主代理", loadSourceIndex())
	if !ok || st.Mode != "manual" {
		t.Fatalf("%+v %v", st, ok)
	}
	if st.SelectedSource != "ktm-cloud" || st.SelectedSourceLabel != "KTM" || st.ManualNodeSource != "ktm-cloud" || st.ManualNodeSourceLabel != "KTM" {
		t.Fatalf("attribution missing: %+v", st)
	}
	// Unattributed nodes are reported as unknown, never guessed.
	snap.Proxies["主代理"] = proxyInfo{Type: "Selector", All: []string{"n1", "n3", "zz"}, Now: "zz"}
	st, ok = groupStatusWithSources(snap, nil, "主代理", loadSourceIndex())
	if !ok || st.SelectedSource != "unknown" || st.SelectedSourceLabel != "unknown" || st.ManualNodeSource != "" || st.ManualNodeSourceLabel != "" {
		t.Fatalf("unknown attribution: %+v %v", st, ok)
	}
	// Stale labels disable attribution entirely.
	writeSourcesEnvironment(t, defs, map[string]string{"n1": "big-airport"}, "0123456789abcdef")
	stale := loadSourceIndex()
	if stale.bound || stale.reason != sourceStatusLabelsStale {
		t.Fatalf("%+v", stale)
	}
	st, _ = groupStatusWithSources(snap, map[string]string{"主代理": "n3"}, "主代理", stale)
	if st.SelectedSource != "" || st.ManualNodeSource != "" {
		t.Fatalf("stale labels produced attribution: %+v", st)
	}
	// Missing labels are equally inert.
	if err := os.Remove(nodeLabelsPath()); err != nil {
		t.Fatal(err)
	}
	missing := loadSourceIndex()
	if missing.bound || missing.reason != sourceStatusLabelsMissing {
		t.Fatalf("%+v", missing)
	}
}

func TestPersistSourcesOrder(t *testing.T) {
	defs := []sourceDef{
		{ID: "big-airport", Label: "Big", Type: "profile", Enabled: true},
		{ID: "ktm-cloud", Label: "KTM", Type: "subscription", Enabled: true},
		{ID: "office91", Label: "Office", Type: "vpn", Enabled: false},
	}
	writeSourcesEnvironment(t, defs, nil, "")
	on, off := true, false
	reorder := []sourceUpdateEntry{{ID: "ktm-cloud", Enabled: &on}, {ID: "office91", Enabled: &on}, {ID: "big-airport", Enabled: &off}}
	if err := persistSourcesOrder(configPath(), reorder); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(configPath())
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Sources   []sourceDef `json:"sources"`
		BaseRules string      `json:"base_rules_source"`
		VPN       string      `json:"vpn_service"`
		Profiles  []string    `json:"source_profiles"`
		Fallback  string      `json:"fallback_addr"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Sources) != 3 || doc.Sources[0].ID != "ktm-cloud" || doc.Sources[1].ID != "office91" || doc.Sources[2].ID != "big-airport" {
		t.Fatalf("order not persisted: %+v", doc.Sources)
	}
	if !doc.Sources[0].Enabled || !doc.Sources[1].Enabled || doc.Sources[2].Enabled {
		t.Fatalf("enabled flags not persisted: %+v", doc.Sources)
	}
	if doc.BaseRules != "big-airport" || doc.VPN != "Shadowrocket" || len(doc.Profiles) != 2 || doc.Fallback != "127.0.0.1:33210" {
		t.Fatalf("unrelated fields lost: %+v", doc)
	}
	for _, bad := range [][]sourceUpdateEntry{
		{{ID: "nope", Enabled: &on}, {ID: "ktm-cloud", Enabled: &on}, {ID: "office91", Enabled: &on}},
		{{ID: "ktm-cloud", Enabled: &on}},
		{{ID: "ktm-cloud", Enabled: &on}, {ID: "ktm-cloud", Enabled: &on}, {ID: "office91", Enabled: &on}},
		{{ID: "ktm-cloud"}, {ID: "office91", Enabled: &on}, {ID: "big-airport", Enabled: &off}},
		{},
	} {
		if err := persistSourcesOrder(configPath(), bad); !errors.Is(err, errSourcesUpdateRejected) {
			t.Fatalf("accepted %+v: %v", bad, err)
		}
	}
	after, err := os.ReadFile(configPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(b) {
		t.Fatal("rejected update modified config")
	}
	// Per-entry fields beyond id/enabled survive the rewrite.
	custom := `{"sources":[{"id":"a","label":"A","type":"profile","enabled":true,"url":"https://example.com/s"},{"id":"b","enabled":false}],"base_rules_source":"a","vpn_service":"V"}`
	if err := os.WriteFile(configPath(), []byte(custom), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := persistSourcesOrder(configPath(), []sourceUpdateEntry{{ID: "b", Enabled: &on}, {ID: "a", Enabled: &off}}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(configPath())
	var raw struct {
		Sources []map[string]any `json:"sources"`
		VPN     string           `json:"vpn_service"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if raw.Sources[0]["id"] != "b" || raw.Sources[0]["enabled"] != true {
		t.Fatalf("rewrite wrong: %+v", raw.Sources)
	}
	if raw.Sources[1]["id"] != "a" || raw.Sources[1]["url"] != "https://example.com/s" || raw.Sources[1]["enabled"] != false || raw.VPN != "V" {
		t.Fatalf("entry fields lost: %+v", raw.Sources)
	}
}

func TestManualNodeAPIAndRestore(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(appSupportDir(), 0700); err != nil {
		t.Fatal(err)
	}
	selected := "one"
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/proxies" {
			_ = json.NewEncoder(w).Encode(proxySnapshot{Proxies: map[string]proxyInfo{"主代理": {Type: "Selector", All: []string{"one", "two", "DIRECT"}, Now: selected}, "one": {Type: "Shadowsocks"}, "two": {Type: "Trojan"}, "DIRECT": {Type: "Direct"}}})
			return
		}
		if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/proxies/") {
			var v struct {
				Name string `json:"name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&v)
			selected = v.Name
			w.WriteHeader(204)
			return
		}
		http.NotFound(w, r)
	}))
	defer controller.Close()
	cfg := defaultConfig()
	cfg.Controller = strings.TrimPrefix(controller.URL, "http://")
	cfg.ProbeTimeoutSec = 1
	a := &App{cfg: cfg, log: log.New(io.Discard, "", 0), state: &State{}}
	handler, err := a.nodeAPIHandler()
	if err != nil {
		t.Fatal(err)
	}
	tokenBytes, err := os.ReadFile(filepath.Join(appSupportDir(), "api-token"))
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	info, err := os.Stat(filepath.Join(appSupportDir(), "api-token"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("token permissions: %v %v", info, err)
	}
	call := func(method, body, auth, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://"+cfg.HTTPListen+"/api/nodes", bytes.NewBufferString(body))
		r.URL.Scheme = ""
		r.URL.Host = ""
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("Authorization", auth)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	auth := "Bearer " + token
	if w := call("GET", "", "", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := call("GET", "", auth, "https://evil.example"); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w := call("PUT", `{"group":"主代理","mode":"manual","node":"DIRECT"}`, auth, ""); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := call("PUT", `{"group":"主代理","mode":"manual","node":"two"}`, auth, ""); w.Code != 200 || selected != "two" {
		t.Fatalf("%d %s %s", w.Code, w.Body.String(), selected)
	}
	prefs, err := readManualNodes()
	if err != nil || prefs["主代理"] != "two" {
		t.Fatalf("%v %v", prefs, err)
	}
	selected = "one"
	a.engineMu.Lock()
	err = a.restoreManualNodesLocked()
	a.engineMu.Unlock()
	if err != nil || selected != "two" {
		t.Fatalf("restore %s %v", selected, err)
	}
	if w := call("GET", "", auth, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"manual_node":"two"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if w := call("PUT", `{"group":"主代理","mode":"auto"}`, auth, ""); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	prefs, err = readManualNodes()
	if err != nil || len(prefs) != 0 {
		t.Fatal(prefs, err)
	}
}

func TestNodeSelectionAPIShowsSources(t *testing.T) {
	defs := []sourceDef{
		{ID: "big-airport", Label: "大机场", Type: "profile", Enabled: true},
		{ID: "ktm-cloud", Label: "KTM", Type: "subscription", Enabled: true},
	}
	writeSourcesEnvironment(t, defs, map[string]string{"one": "big-airport", "two": "ktm-cloud"}, "")
	selected := "one"
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/proxies" {
			_ = json.NewEncoder(w).Encode(proxySnapshot{Proxies: map[string]proxyInfo{"主代理": {Type: "Selector", All: []string{"one", "two"}, Now: selected}, "one": {Type: "Shadowsocks"}, "two": {Type: "Trojan"}}})
			return
		}
		if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/proxies/") {
			var v struct {
				Name string `json:"name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&v)
			selected = v.Name
			w.WriteHeader(204)
			return
		}
		http.NotFound(w, r)
	}))
	defer controller.Close()
	cfg := defaultConfig()
	cfg.Controller = strings.TrimPrefix(controller.URL, "http://")
	cfg.ProbeTimeoutSec = 1
	a := &App{cfg: cfg, log: log.New(io.Discard, "", 0), state: &State{}}
	handler, err := a.nodeAPIHandler()
	if err != nil {
		t.Fatal(err)
	}
	tokenBytes, err := os.ReadFile(filepath.Join(appSupportDir(), "api-token"))
	if err != nil {
		t.Fatal(err)
	}
	auth := "Bearer " + strings.TrimSpace(string(tokenBytes))
	call := func(method, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://"+cfg.HTTPListen+"/api/nodes", bytes.NewBufferString(body))
		r.URL.Scheme = ""
		r.URL.Host = ""
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("Authorization", auth)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	// Manual pinning crosses sources and the API reports the pinned node's
	// source even though it is not the highest-priority one.
	w := call("PUT", `{"group":"主代理","mode":"manual","node":"two"}`)
	if w.Code != 200 || selected != "two" {
		t.Fatalf("%d %s %s", w.Code, w.Body.String(), selected)
	}
	if !strings.Contains(w.Body.String(), `"manual_node_source":"ktm-cloud"`) || !strings.Contains(w.Body.String(), `"selected_source":"ktm-cloud"`) || !strings.Contains(w.Body.String(), `"selected_source_label":"KTM"`) {
		t.Fatalf("source display missing: %s", w.Body.String())
	}
	w = call("GET", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"manual_node_source":"ktm-cloud"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestSelectionFailuresAndInvalidPreference(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(appSupportDir(), 0700); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	selected, fail, unavailable := "one", "", false
	gets := 0
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == "GET" {
			gets++
			if fail == "readback" && gets > 1 {
				http.Error(w, "unavailable", 503)
				return
			}
			members := []string{"one", "two"}
			if unavailable {
				members = []string{"one"}
			}
			_ = json.NewEncoder(w).Encode(proxySnapshot{Proxies: map[string]proxyInfo{"主代理": {Type: "Selector", All: members, Now: selected}, "one": {Type: "Trojan"}, "two": {Type: "Trojan"}}})
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Name == "two" && fail == "reject" {
			http.Error(w, "failed", 500)
			return
		}
		if body.Name == "two" && fail == "applied_error" {
			selected = body.Name
			http.Error(w, "failed", 500)
			return
		}
		selected = body.Name
		w.WriteHeader(204)
	}))
	defer controller.Close()
	cfg := defaultConfig()
	cfg.Controller = strings.TrimPrefix(controller.URL, "http://")
	cfg.ProbeTimeoutSec = 1
	a := &App{cfg: cfg, log: log.New(io.Discard, "", 0), state: &State{}, nodeScanWake: make(chan struct{}, 1)}
	call := func(method, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/nodes", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		a.serveNodeSelection(w, r)
		return w
	}
	for _, scenario := range []string{"reject", "applied_error"} {
		mu.Lock()
		fail = scenario
		selected = "one"
		mu.Unlock()
		w := call("PUT", `{"group":"主代理","mode":"manual","node":"two"}`)
		if w.Code != 502 || !strings.Contains(w.Body.String(), `"rollback":"confirmed"`) {
			t.Fatalf("%s: %d %s", scenario, w.Code, w.Body.String())
		}
		prefs, err := readManualNodes()
		if err != nil || len(prefs) != 0 {
			t.Fatalf("%s: %v %v", scenario, prefs, err)
		}
		mu.Lock()
		actual := selected
		mu.Unlock()
		if actual != "one" {
			t.Fatal(actual)
		}
	}
	mu.Lock()
	fail = "readback"
	gets = 0
	selected = "one"
	mu.Unlock()
	w := call("PUT", `{"group":"主代理","mode":"manual","node":"two"}`)
	if w.Code != 502 || !strings.Contains(w.Body.String(), `"actual_known":false`) || !strings.Contains(w.Body.String(), `"preference_changed":false`) {
		t.Fatalf("uncertain: %d %s", w.Code, w.Body.String())
	}
	prefs, err := readManualNodes()
	if err != nil || len(prefs) != 0 {
		t.Fatalf("%v %v", prefs, err)
	}
	mu.Lock()
	fail = ""
	selected = "one"
	mu.Unlock()
	if err := saveNodeSelection(manualNodePath(), map[string]string{"主代理": "two"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	unavailable = true
	mu.Unlock()
	w = call("GET", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"warning":"manual_node_unavailable"`) || !strings.Contains(w.Body.String(), `"mode":"auto"`) || !strings.Contains(w.Body.String(), `"desired_mode":"manual"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	p, err := a.selectionProber()
	if err != nil {
		t.Fatal(err)
	}
	defer p.client.CloseIdleConnections()
	snap, err := p.snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	manual, err := p.manualSelectionLocked("主代理", snap)
	if err != nil || manual {
		t.Fatalf("manual=%v err=%v", manual, err)
	}
	// Restore leaves invalid preference visible without activating it.
	a.engineMu.Lock()
	err = a.restoreManualNodesLocked()
	a.engineMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.nodeScanWake:
	default:
	}
	w = call("PUT", `{"group":"主代理","mode":"auto"}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	select {
	case <-a.nodeScanWake:
	default:
		t.Fatal("automatic scan not signalled")
	}
}
