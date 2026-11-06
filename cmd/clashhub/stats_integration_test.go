package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
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

func TestStatsEndpointAndTraffic(t *testing.T) {
	store, err := OpenStats(filepath.Join(t.TempDir(), "stats.json"))
	if err != nil {
		t.Fatal(err)
	}
	store.top["example.com"] = time.Now().Add(-time.Minute)
	store.rankMinute = statsMinute(time.Now())
	s := &statsIntegration{store: store, logf: t.Logf}
	s.connection("example.com:443", time.Now(), nil)
	s.health("gstatic", time.Now(), nil)
	var out strings.Builder
	if _, err := (statsWriter{writer: &out, stats: s, target: "example.com:443", upload: true}).Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:33335/stats?minutes=60", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.URL.Scheme, req.URL.Host = "", ""
	w := httptest.NewRecorder()
	s.serveHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var result struct {
		Rows    []StatsRow         `json:"rows"`
		Ranking []StatsDomainRank  `json:"domain_ranking"`
		Ports   []StatsPortRow     `json:"port_rows"`
		P95     *uint64            `json:"p95_60s_ms_upper"`
		Samples uint64             `json:"p95_60s_samples"`
		Health  []StatsHealthHour  `json:"health_hours"`
		Updated time.Time          `json:"updated_at"`
		Totals  StatsTrafficTotals `json:"traffic_totals"`
		Started time.Time          `json:"process_started_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || !result.Rows[0].NodeUnknown || result.Rows[0].Upload != 3 || result.Rows[0].Successes != 1 || len(result.Health) != 1 || result.Updated.IsZero() {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(result.Ranking) != 1 || result.Ranking[0].Target != "example.com" || result.Ranking[0].Upload != 3 || result.Ranking[0].DetailStartedAt.IsZero() {
		t.Fatalf("unexpected ranking: %+v", result.Ranking)
	}
	if result.Ranking[0].Attempts != 1 || result.Ranking[0].AttemptsComplete || result.Ranking[0].AttemptsStartedAt.IsZero() {
		t.Fatalf("unexpected ranking coverage: %+v", result.Ranking[0])
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	var ranking []map[string]json.RawMessage
	if err := json.Unmarshal(raw["domain_ranking"], &ranking); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"attempts", "attempts_started_at", "attempts_complete"} {
		if _, ok := ranking[0][key]; !ok {
			t.Fatalf("missing ranking field %s", key)
		}
	}
	if len(result.Ports) != 1 || !result.Ports[0].Unknown || result.Ports[0].Upload != 3 || result.P95 == nil || result.Samples != 1 {
		t.Fatalf("unexpected port/latency stats: %+v", result)
	}
	if result.Totals.Upload != 3 || result.Totals.Download != 0 || result.Started.IsZero() {
		t.Fatalf("unexpected process totals: %+v", result)
	}
	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") },
		func(r *http.Request) { r.Host = "evil.example:33335" },
		func(r *http.Request) { r.RemoteAddr = "192.0.2.1:1234" },
		func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") },
		func(r *http.Request) { r.URL.Scheme = "http"; r.URL.Host = "127.0.0.1:33335" },
	} {
		r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:33335/stats", nil)
		r.RemoteAddr = "127.0.0.1:1234"
		r.URL.Scheme, r.URL.Host = "", ""
		mutate(r)
		w := httptest.NewRecorder()
		s.serveHTTP(w, r)
		if w.Code != http.StatusForbidden || w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("security: %d %v", w.Code, w.Header())
		}
	}
}

type partialStatsWriter struct{}

func (partialStatsWriter) Write(p []byte) (int, error) { return 2, io.ErrShortWrite }

func TestProcessTrafficIndependentOfBuckets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats")
	store, err := OpenStats(path)
	if err != nil {
		t.Fatal(err)
	}
	s := &statsIntegration{store: store, logf: t.Logf}
	// Invalid dimensions must not discard real traffic; partial writes count only accepted bytes.
	_, _ = (statsWriter{writer: partialStatsWriter{}, stats: s, target: "https://bad", upload: true}).Write([]byte("hello"))
	_, _ = (statsWriter{writer: partialStatsWriter{}, stats: s, target: "host"}).Write([]byte("hello"))
	reader := statsReader{reader: io.NopCloser(strings.NewReader("abc")), stats: s, target: "host"}
	if _, err := io.ReadAll(reader); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = (statsWriter{writer: io.Discard, stats: s, target: "host", upload: true}).Write([]byte("x"))
			}
		}()
	}
	wg.Wait()
	got, started := store.processTraffic()
	if got.Upload != 805 || got.Download != 2 || started.IsZero() {
		t.Fatalf("totals: %+v %v", got, started)
	}
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := OpenStats(path)
	if err != nil {
		t.Fatal(err)
	}
	reset, _ := reloaded.processTraffic()
	if reset != (StatsTrafficTotals{}) {
		t.Fatalf("persisted process totals: %+v", reset)
	}
	store.recordProcessTraffic(^uint64(0), ^uint64(0))
	store.recordProcessTraffic(1, 1)
	got, _ = store.processTraffic()
	if got.Upload != ^uint64(0) || got.Download != ^uint64(0) {
		t.Fatalf("overflow: %+v", got)
	}
}

func TestStatsWindowHealthAndPorts(t *testing.T) {
	now := time.Date(2026, 1, 3, 12, 30, 30, 0, time.UTC)
	store, err := OpenStats(filepath.Join(t.TempDir(), "stats"))
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return now }
	store.top["example.com"] = now.Add(-48 * time.Hour)
	store.rankMinute = statsMinute(now)
	for _, minutes := range []int{0, 5, 6, 10, 11, 60, 61, 1440, 1441, 2880} {
		at := now.Add(-time.Duration(minutes) * time.Minute)
		if err := store.RecordHealth(StatsHealth{At: at, Name: "probe", Success: true, Duration: time.Millisecond}); err != nil {
			t.Fatal(err)
		}
		if err := store.RecordPortTraffic(at, "example.com", "", 1234, 1, 2); err != nil {
			t.Fatal(err)
		}
	}
	// Future minute buckets can be present in a persisted snapshot after a clock change.
	store.health[statsHealthKey{statsMinute(now) + 1, "future"}] = statsCounts{Attempts: 1}
	s := &statsIntegration{store: store, logf: t.Logf}
	for _, tt := range []struct {
		query string
		want  int
	}{
		{"", 6}, {"5", 2}, {"10", 4}, {"60", 6}, {"1440", 8}, {"2880", 10},
	} {
		t.Run(tt.query, func(t *testing.T) {
			path := "/stats"
			if tt.query != "" {
				path += "?minutes=" + tt.query
			}
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.Host, r.RemoteAddr = "127.0.0.1:33335", "127.0.0.1:1234"
			w := httptest.NewRecorder()
			s.serveHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			var got struct {
				Rows   []StatsRow        `json:"rows"`
				Ports  []StatsPortRow    `json:"port_rows"`
				Health []StatsHealthHour `json:"health_hours"`
				Window []StatsHealthHour `json:"window_health_hours"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			var count, legacy uint64
			for _, h := range got.Window {
				count += h.Attempts
				if h.Name != "probe" {
					t.Fatalf("future health included: %+v", h)
				}
			}
			for _, h := range got.Health {
				legacy += h.Attempts
			}
			if count != uint64(tt.want) || legacy != 10 || len(got.Rows) != 1 || got.Rows[0].Upload != uint64(tt.want) || len(got.Ports) != 1 || got.Ports[0].Upload != uint64(tt.want) {
				t.Fatalf("window %q: %+v count=%d legacy=%d", tt.query, got, count, legacy)
			}
		})
	}
	for _, value := range []string{"15", "2881", "bad"} {
		r := httptest.NewRequest(http.MethodGet, "/stats?minutes="+value, nil)
		r.Host, r.RemoteAddr = "127.0.0.1:33335", "127.0.0.1:1234"
		w := httptest.NewRecorder()
		s.serveHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", value, w.Code)
		}
	}
}

func TestStatsMethod(t *testing.T) {
	store, err := OpenStats(filepath.Join(t.TempDir(), "stats"))
	if err != nil {
		t.Fatal(err)
	}
	s := &statsIntegration{store: store, logf: t.Logf}
	req := httptest.NewRequest(http.MethodPost, "/stats", nil)
	req.Host = "127.0.0.1:33335"
	req.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	s.serveHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET" {
		t.Fatal(w.Code, w.Header())
	}
}

func TestStatsLifecycleLockAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats")
	if os.Getenv("CLASHHUB_LOCK_CHILD") != "" {
		f, err := lockStatsLifecycle(os.Getenv("CLASHHUB_LOCK_CHILD"))
		if err == nil {
			f.Close()
			os.Exit(2)
		}
		return
	}
	f, err := lockStatsLifecycle(path)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestStatsLifecycleLockAcrossProcesses$")
	cmd.Env = append(os.Environ(), "CLASHHUB_LOCK_CHILD="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child lock: %v %s", err, out)
	}
	f.Close()
	next, err := lockStatsLifecycle(path)
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
}

func TestProxyShutdownWaitsForFinalTraffic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats")
	store, err := OpenStats(path)
	if err != nil {
		t.Fatal(err)
	}
	a := &App{stats: &statsIntegration{store: store, logf: t.Logf}}
	if !a.beginProxyWork() {
		t.Fatal("admission")
	}
	client, clientPeer := net.Pipe()
	upstream, upstreamPeer := net.Pipe()
	defer clientPeer.Close()
	defer upstreamPeer.Close()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer a.proxyWork.Done()
		a.relay(client, upstream, "127.0.0.1:443", "example.com")
	}()
	got := make(chan struct{})
	go func() { b := make([]byte, 3); _, _ = io.ReadFull(upstreamPeer, b); close(got) }()
	if _, err := clientPeer.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	<-got
	a.stopProxyWork()
	<-finished
	if a.beginProxyWork() {
		t.Fatal("admission after shutdown")
	}
	if err := store.closeAndFlush(); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordTraffic(time.Time{}, "example.com", "", 9, 0); err == nil {
		t.Fatal("write after final flush")
	}
	reloaded, err := OpenStats(path)
	if err != nil {
		t.Fatal(err)
	}
	ports, err := reloaded.QueryPortRows(5)
	if err != nil || len(ports) != 1 || ports[0].Upload != 3 {
		t.Fatalf("tail traffic %+v %v", ports, err)
	}
}

func TestSourcesAPIAndStatsRows(t *testing.T) {
	defs := []sourceDef{
		{ID: "big-airport", Label: "Big Airport", Type: "profile", Enabled: true},
		{ID: "office91", Label: "Office 91", Type: "vpn", Enabled: true},
		{ID: "ktm-cloud", Label: "KTM", Type: "subscription", Enabled: false},
	}
	writeSourcesEnvironment(t, defs, map[string]string{"b1": "big-airport", "o1": "office91"}, "")
	store, err := OpenStats(filepath.Join(t.TempDir(), "stats.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Bytes clashhub relayed directly to the fallback port.
	if err := store.RecordPortTraffic(time.Now(), "example.com:443", "", 7890, 11, 13); err != nil {
		t.Fatal(err)
	}
	traffic, err := openNodeTraffic(filepath.Join(t.TempDir(), "traffic.json"), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	// Both synthetic samples must be in the past, even at a minute boundary.
	now := time.Now().Add(-10 * time.Second)
	names := map[string]string{
		"b1":             nodeHealthName("b1"),
		"o1":             nodeHealthName("o1"),
		"FALLBACK-LOCAL": nodeHealthName("FALLBACK-LOCAL"),
		"x1":             nodeHealthName("x1"),
	}
	conns := []mihomoConnectionSample{
		{ID: "1", Start: now.Add(-time.Minute), Chains: []string{"b1"}, Upload: 10, Download: 20},
		{ID: "2", Start: now.Add(-time.Minute), Chains: []string{"o1"}, Upload: 100, Download: 200},
		{ID: "3", Start: now.Add(-time.Minute), Chains: []string{"FALLBACK-LOCAL"}, Upload: 1000, Download: 2000},
		{ID: "4", Start: now.Add(-time.Minute), Chains: []string{"x1"}, Upload: 7, Download: 8},
	}
	if err := traffic.apply("one", now, mihomoConnectionsSnapshot{UploadTotal: 1117, DownloadTotal: 2228, Connections: conns}, names); err != nil {
		t.Fatal(err)
	}
	conns[0].Upload, conns[0].Download = 30, 60
	conns[1].Upload, conns[1].Download = 250, 500
	conns[2].Upload, conns[2].Download = 1500, 3000
	conns[3].Upload, conns[3].Download = 10, 14
	if err := traffic.apply("one", now.Add(5*time.Second), mihomoConnectionsSnapshot{UploadTotal: 1790, DownloadTotal: 3574, Connections: conns}, names); err != nil {
		t.Fatal(err)
	}
	s := &statsIntegration{store: store, nodeTraffic: traffic, logf: t.Logf}
	s.sourceApply = func(_ context.Context, save func() error) error { return save() }
	token := strings.Repeat("a", 64)
	if err := os.WriteFile(filepath.Join(appSupportDir(), "api-token"), []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	request := func(method, path, auth, body, contentType string, mutate func(*http.Request)) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:33335"+path, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:1234"
		r.URL.Scheme = ""
		r.URL.Host = ""
		if auth != "" {
			r.Header.Set("Authorization", "Bearer "+auth)
		}
		if contentType != "" {
			r.Header.Set("Content-Type", contentType)
		}
		if mutate != nil {
			mutate(r)
		}
		w := httptest.NewRecorder()
		s.serveHTTP(w, r)
		return w
	}
	// Same loopback/token/Host/Origin protections as the other node APIs.
	if w := request("GET", "/api/sources", "", "", "", nil); w.Code != 401 {
		t.Fatal(w.Code)
	}
	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") },
		func(r *http.Request) { r.Host = "evil.example:33335" },
		func(r *http.Request) { r.RemoteAddr = "192.0.2.1:1234" },
		func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") },
		func(r *http.Request) { r.URL.Scheme = "http"; r.URL.Host = "127.0.0.1:33335" },
	} {
		if w := request("GET", "/api/sources", token, "", "", mutate); w.Code != 403 {
			t.Fatalf("security: %d", w.Code)
		}
	}
	if w := request("POST", "/api/sources", token, "", "", nil); w.Code != 405 || w.Header().Get("Allow") != "GET, PUT" {
		t.Fatal(w.Code, w.Header().Get("Allow"))
	}
	for _, v := range []string{"1", "2881", "x"} {
		if w := request("GET", "/api/sources?minutes="+v, token, "", "", nil); w.Code != 400 {
			t.Fatal(v, w.Code)
		}
	}
	w := request("GET", "/api/sources?minutes=60", token, "", "", nil)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, w.Body.String())
	}
	var report sourcesReport
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Sources) != 3 || report.Sources[0].ID != "big-airport" || report.Sources[1].ID != "office91" || report.Sources[2].ID != "ktm-cloud" {
		t.Fatalf("manifest: %+v", report.Sources)
	}
	if report.Sources[0].NodeCount != 1 || report.Sources[0].Status != "ok" || report.Sources[0].Type != "profile" || !report.Sources[0].Enabled {
		t.Fatalf("big-airport row: %+v", report.Sources[0])
	}
	if report.Sources[1].NodeCount != 1 || report.Sources[1].Status != "ok" {
		t.Fatalf("office91 row: %+v", report.Sources[1])
	}
	if report.Sources[2].Enabled || report.Sources[2].Status != "disabled" || report.Sources[2].NodeCount != 0 {
		t.Fatalf("ktm-cloud row: %+v", report.Sources[2])
	}
	rows := report.Traffic.Rows
	if len(rows) != 4 || rows[0].ID != "big-airport" || rows[1].ID != "office91" || rows[2].ID != "ktm-cloud" || rows[3].ID != "office91-port" {
		t.Fatalf("traffic rows: %+v", rows)
	}
	if rows[0].Upload != 20 || rows[0].Download != 40 || rows[0].Label != "Big Airport" {
		t.Fatalf("big-airport traffic: %+v", rows[0])
	}
	// The office91 node row carries node bytes only; the 7890 port bytes
	// (direct relay plus the engine's fallback node) stay on their own row.
	if rows[1].Upload != 150 || rows[1].Download != 300 {
		t.Fatalf("office91 node traffic: %+v", rows[1])
	}
	if rows[2].Upload != 0 || rows[2].Download != 0 {
		t.Fatalf("disabled source traffic: %+v", rows[2])
	}
	if rows[3].Upload != 511 || rows[3].Download != 1013 || rows[3].Label != "Office 91（端口兜底）" {
		t.Fatalf("office91-port row: %+v", rows[3])
	}
	if report.Traffic.UnknownUpload != 3 || report.Traffic.UnknownDownload != 6 || report.Traffic.Note != "observed-partial" {
		t.Fatalf("unknown traffic: %+v", report.Traffic)
	}
	if report.BaseRulesSource != "big-airport" || !report.PriorityActive || report.PriorityStatus != "ok" {
		t.Fatalf("report meta: %+v", report)
	}
	// /stats carries the same rows plus the unknown remainder.
	req := httptest.NewRequest(http.MethodGet, "/stats?minutes=60", nil)
	req.Host, req.RemoteAddr = "127.0.0.1:33335", "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	s.serveHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var stats struct {
		SourceRows []sourceTrafficRow `json:"source_rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if len(stats.SourceRows) != 5 {
		t.Fatalf("source_rows: %+v", stats.SourceRows)
	}
	want := []sourceTrafficRow{rows[0], rows[1], rows[2], rows[3], {ID: "unknown", Label: "unknown", Upload: 3, Download: 6}}
	for i := range want {
		if stats.SourceRows[i] != want[i] {
			t.Fatalf("source_rows[%d] = %+v, want %+v", i, stats.SourceRows[i], want[i])
		}
	}
	// PUT persists priority order and enabled flags, preserving every other
	// config.json field; unknown, missing or duplicated ids are rejected.
	put := func(body, contentType string) *httptest.ResponseRecorder {
		return request("PUT", "/api/sources", token, body, contentType, nil)
	}
	w = put(`{"sources":[{"id":"ktm-cloud","enabled":true},{"id":"office91","enabled":true},{"id":"big-airport","enabled":false}]}`, "application/json")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var updated sourcesReport
	if err := json.Unmarshal(w.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if len(updated.Sources) != 3 || updated.Sources[0].ID != "ktm-cloud" || updated.Sources[2].ID != "big-airport" || updated.Sources[2].Enabled || updated.Sources[2].Status != "disabled" || !updated.PriorityActive {
		t.Fatalf("updated report: %+v", updated.Sources)
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
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Sources) != 3 || doc.Sources[0].ID != "ktm-cloud" || !doc.Sources[0].Enabled || doc.Sources[2].ID != "big-airport" || doc.Sources[2].Enabled {
		t.Fatalf("config not rewritten: %+v", doc.Sources)
	}
	if doc.BaseRules != "big-airport" || doc.VPN != "Shadowrocket" || len(doc.Profiles) != 2 {
		t.Fatalf("unrelated config fields lost: %+v", doc)
	}
	for _, body := range []string{
		`{"sources":[{"id":"nope","enabled":true},{"id":"office91","enabled":true},{"id":"big-airport","enabled":false}]}`,
		`{"sources":[{"id":"ktm-cloud","enabled":true}]}`,
		`{"sources":[{"id":"ktm-cloud","enabled":true},{"id":"ktm-cloud","enabled":true},{"id":"office91","enabled":true}]}`,
		`{"sources":[{"id":"ktm-cloud"},{"id":"office91","enabled":true},{"id":"big-airport","enabled":false}]}`,
		`{"sources":[]}`,
		`{"order":[{"id":"ktm-cloud","enabled":true}]}`,
		``,
	} {
		if w := put(body, "application/json"); w.Code != 400 {
			t.Fatalf("accepted %s: %d %s", body, w.Code, w.Body.String())
		}
	}
	if w := put(`{"sources":[{"id":"ktm-cloud","enabled":true}]}`, "text/plain"); w.Code != 415 {
		t.Fatal(w.Code)
	}
	after, _ := os.ReadFile(configPath())
	if string(after) != string(b) {
		t.Fatal("rejected update modified config")
	}
	// Stale labels: attribution collapses to unknown, priority is inactive.
	writeSourcesEnvironment(t, defs, map[string]string{"b1": "big-airport"}, "0123456789abcdef")
	// The fresh environment has a new HOME; reinstall the API token there.
	if err := os.WriteFile(filepath.Join(appSupportDir(), "api-token"), []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	w = request("GET", "/api/sources?minutes=60", token, "", "", nil)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	var stale sourcesReport
	if err := json.Unmarshal(w.Body.Bytes(), &stale); err != nil {
		t.Fatal(err)
	}
	if stale.PriorityActive || stale.PriorityStatus != "labels-stale" || stale.Sources[0].Status != "unknown" || stale.Sources[0].NodeCount != 0 {
		t.Fatalf("stale report: %+v", stale)
	}
	if stale.Traffic.Rows[0].Upload != 0 || stale.Traffic.UnknownUpload != 173 || stale.Traffic.UnknownDownload != 346 {
		t.Fatalf("stale traffic: %+v", stale.Traffic)
	}
	if stale.Traffic.Rows[3].ID != "office91-port" || stale.Traffic.Rows[3].Upload != 511 {
		t.Fatalf("stale port row: %+v", stale.Traffic.Rows)
	}
}
