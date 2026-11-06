package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
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

func TestNodePrioritySelection(t *testing.T) {
	for _, bound := range []bool{false, true} {
		idx := sourceIndex{}
		if bound {
			idx = sourceIndex{bound: true, defs: []sourceDef{{ID: "first", Enabled: true}, {ID: "second", Enabled: true}}, nodeSource: map[string]string{"a": "first", "b": "first", "c": "second"}}
		}
		healthy := map[string]bool{"a": true, "b": true, "c": true}
		failed := map[string]bool{}
		in := selectNodeChoiceInput{Index: idx, Candidates: []string{"a", "b", "c"}, Active: "a", Previous: "a", Priorities: map[string]string{"a": "low", "b": "medium", "c": "high"}, Healthy: func(n string) bool { return healthy[n] }, Failed: func(n string) bool { return failed[n] }, Delay: func(n string) int64 {
			if n == "c" {
				return 5
			}
			if n == "b" {
				return 100
			}
			return 10
		}}
		want := "c"
		if got := selectNodeChoice(in); got != want {
			t.Fatalf("bound=%v priority choice %q want %q", bound, got, want)
		}
		if bound {
			// Equal priority falls back to existing source order, even if slower.
			in.Priorities["b"] = "high"
			if got := selectNodeChoice(in); got != "b" {
				t.Fatalf("same-tier source order: %q", got)
			}
			in.Priorities["b"] = "medium"
			// A disabled source cannot win even with the highest priority.
			in.Index.defs[1].Enabled = false
			if got := selectNodeChoice(in); got != "b" {
				t.Fatalf("disabled high source: %q", got)
			}
			in.Index.defs[1].Enabled = true
		}
		// Within a tier, retain the healthy sticky node when under 500ms.
		in.Priorities["a"] = "high"
		if got := selectNodeChoice(in); got != "a" {
			t.Fatalf("same-tier stickiness: %q", got)
		}
		in.Priorities["a"] = "low"
		healthy["b"] = false
		healthy["c"] = false
		if got := selectNodeChoice(in); got != "a" {
			t.Fatalf("health must override priority: %q", got)
		}
		healthy["a"] = false
		if got := selectNodeChoice(in); got != "" {
			t.Fatalf("uncertain health must hold: %q", got)
		}
		for _, n := range in.Candidates {
			failed[n] = true
		}
		if got := selectNodeChoice(in); got != "" {
			t.Fatalf("target failures should hold current selection: %q", got)
		}
	}
}

func TestUncertainHigherSourceDoesNotBlockHealthyLowerSource(t *testing.T) {
	idx := sourceIndex{bound: true, defs: []sourceDef{{ID: "first", Enabled: true}, {ID: "second", Enabled: true}}, nodeSource: map[string]string{"pending": "first", "ready": "second"}}
	for _, active := range []string{"pending", "FALLBACK-LOCAL"} {
		in := selectNodeChoiceInput{Index: idx, Candidates: []string{"pending", "ready"}, Active: active,
			Healthy: func(n string) bool { return n == "ready" }, Failed: func(string) bool { return false }, Delay: func(string) int64 { return 20 }}
		if got := selectNodeChoice(in); got != "ready" {
			t.Fatalf("active=%s got %q", active, got)
		}
		in.Healthy = func(string) bool { return false }
		if got := selectNodeChoice(in); got != "" {
			t.Fatalf("uncertain nodes should hold: %q", got)
		}
	}
}

func TestDisabledExcludedWithoutHealthyCandidates(t *testing.T) {
	for _, bound := range []bool{false, true} {
		idx := sourceIndex{}
		if bound {
			idx = sourceIndex{bound: true, defs: []sourceDef{{ID: "s", Enabled: true}}, nodeSource: map[string]string{"disabled": "s", "failed": "s"}}
		}
		in := selectNodeChoiceInput{Index: idx, Candidates: []string{"disabled", "failed"}, Active: "disabled", Previous: "disabled", Priorities: map[string]string{"disabled": "disabled:high"}, Healthy: func(string) bool { return false }, Failed: func(n string) bool { return n == "failed" }, Delay: func(string) int64 { return 1 }}
		if got := selectNodeChoice(in); got != "" {
			t.Fatalf("bound=%v: %q", bound, got)
		}
		in.Candidates = []string{"disabled"}
		if got := selectNodeChoice(in); got != "" {
			t.Fatalf("bound=%v: %q", bound, got)
		}
		in.Healthy = func(string) bool { return true }
		if got := selectNodeChoice(in); got != "" {
			t.Fatalf("sticky disabled bound=%v: %q", bound, got)
		}
	}
}

func TestDisableActiveUsesRejectOrFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name                                string
		reject, rejectWrite, nested, manual bool
		want                                int
	}{
		{"reject active", true, true, false, false, 409},
		{"nested route", true, true, true, false, 409},
		{"legacy selector", false, true, false, false, 409},
		{"failed reject write", true, false, false, false, 409},
		{"manual pin", true, true, false, true, 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			if tc.manual {
				if err := saveNodeSelection(manualNodePath(), map[string]string{"主代理": "a"}); err != nil {
					t.Fatal(err)
				}
			}
			var mu sync.Mutex
			now := "a"
			if tc.nested {
				now = "nested"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Method == http.MethodPut {
					if !tc.rejectWrite {
						http.Error(w, "failed", 500)
						return
					}
					var req map[string]string
					_ = json.NewDecoder(r.Body).Decode(&req)
					if req["name"] != "REJECT" {
						t.Errorf("unverified route %q", req["name"])
					}
					now = req["name"]
					w.WriteHeader(204)
					return
				}
				members := []string{"a", "nested"}
				if tc.reject {
					members = append(members, "REJECT")
				}
				_ = json.NewEncoder(w).Encode(proxySnapshot{Proxies: map[string]proxyInfo{
					"主代理":    {Type: "Selector", All: members, Now: now},
					"AI主代理":  {Type: "Selector", All: []string{"b"}, Now: "b"},
					"nested": {Type: "URLTest", All: []string{"a", "b"}, Now: "a"},
					"a":      {Type: "Trojan"}, "b": {Type: "Trojan"}, "REJECT": {Type: "Reject"},
				}})
			}))
			defer server.Close()
			cfg := defaultConfig()
			cfg.Controller = strings.TrimPrefix(server.URL, "http://")
			cfg.ProbeTimeoutSec = 1
			a := &App{cfg: cfg, log: log.New(io.Discard, "", 0), state: &State{}, nodeScanWake: make(chan struct{}, 1)}
			r := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"node":"a","enabled":false}`))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			a.serveNodePriorities(w, r)
			if w.Code != tc.want {
				t.Fatalf("status %d want %d: %s", w.Code, tc.want, w.Body.String())
			}
			prefs, err := readNodePriorities()
			if err != nil {
				t.Fatal(err)
			}
			if nodeExcluded(prefs, "a") != (tc.want == 200) {
				t.Fatalf("unexpected preferences %v", prefs)
			}
			mu.Lock()
			got := now
			mu.Unlock()
			if got != map[bool]string{true: "nested", false: "a"}[tc.nested] {
				t.Fatalf("failed update mutated selector: %s", got)
			}
		})
	}
}

func TestDisablePersistenceFailureDoesNotMutateSelectors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(nodePrioritiesPath(), 0700); err != nil {
		t.Fatal(err)
	}
	var writes atomic.Int32
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
			w.WriteHeader(500)
			return
		}
		_ = json.NewEncoder(w).Encode(proxySnapshot{Proxies: map[string]proxyInfo{
			"主代理":   {Type: "Selector", All: []string{"a", "b"}, Now: "b"},
			"AI主代理": {Type: "Selector", All: []string{"a", "b"}, Now: "b"},
			"a":     {Type: "Trojan"}, "b": {Type: "Trojan"},
		}})
	}))
	defer controller.Close()
	cfg := defaultConfig()
	cfg.Controller = strings.TrimPrefix(controller.URL, "http://")
	a := &App{cfg: cfg}
	r := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"node":"a","enabled":false}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.serveNodePriorities(w, r)
	if w.Code != 500 || writes.Load() != 0 {
		t.Fatalf("status=%d selector writes=%d", w.Code, writes.Load())
	}
}

func TestUnrelatedDisabledNodeDoesNotBlockPrimaryRequests(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := saveNodeSelection(nodePrioritiesPath(), map[string]string{"a": "disabled"}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) }))
	defer primary.Close()
	cfg := defaultConfig()
	cfg.Controller = "127.0.0.1:1" // no controller or unrelated AI selector required
	cfg.PrimaryAddr = strings.TrimPrefix(primary.URL, "http://")
	a := &App{cfg: cfg, transports: map[string]*http.Transport{}}
	resp, err := a.routingRoundTrip(httptest.NewRequest(http.MethodGet, "http://example.test/", nil), cfg.PrimaryAddr, 0)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if calls.Load() != 1 {
		t.Fatalf("primary calls %d", calls.Load())
	}
}

func TestNodePriorityAPI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(appSupportDir(), 0700); err != nil {
		t.Fatal(err)
	}
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("priority update changed manual selector: %s", r.Method)
		}
		_ = json.NewEncoder(w).Encode(proxySnapshot{Proxies: map[string]proxyInfo{"主代理": {Type: "Selector", All: []string{"a", "b", "DIRECT"}, Now: "b"}, "AI主代理": {Type: "Selector", All: []string{"a", "b"}, Now: "b"}, "a": {Type: "Trojan"}, "b": {Type: "Trojan"}, "DIRECT": {Type: "Direct"}}})
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
	token, err := os.ReadFile(filepath.Join(appSupportDir(), "api-token"))
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, body, auth, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://"+cfg.HTTPListen+"/api/node-priorities", bytes.NewBufferString(body))
		r.URL.Scheme = ""
		r.URL.Host = ""
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("Authorization", auth)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	auth := "Bearer " + strings.TrimSpace(string(token))
	if w := call("GET", "", "", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := call("GET", "", auth, "https://example.com"); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w := call("GET", "", auth, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"priority":"medium"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, body := range []string{`{"node":"DIRECT","priority":"high"}`, `{"node":"a","priority":"invalid"}`, `{"node":"a","priority":"high","extra":1}`} {
		if w := call("PUT", body, auth, ""); w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := call("PUT", `{"node":"a","priority":"high"}`, auth, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	prefs, err := readNodePriorities()
	if err != nil || prefs["a"] != "high" {
		t.Fatal(prefs, err)
	}
	st, err := os.Stat(nodePrioritiesPath())
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal(st, err)
	}
	if w := call("PUT", `{"node":"a","priority":"medium"}`, auth, ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	prefs, err = readNodePriorities()
	if err != nil || prefs["a"] != "medium" {
		t.Fatal(prefs, err)
	}
	if w := call("PUT", `{"node":"a","enabled":false}`, auth, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"override":"medium"`) || !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call("PUT", `{"node":"a","enabled":true}`, auth, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	prefs, err = readNodePriorities()
	if err != nil || prefs["a"] != "medium" {
		t.Fatal(prefs, err)
	}
	if w := call("PUT", `{"node":"a","priority":"default"}`, auth, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	prefs, err = readNodePriorities()
	if err != nil || len(prefs) != 0 {
		t.Fatal(prefs, err)
	}
	if err := os.WriteFile(nodePrioritiesPath(), []byte(`{"a":"invalid"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if w := call("GET", "", auth, ""); w.Code != 500 {
		t.Fatal(w.Code)
	}
}

// Stored values must accept a bare "disabled" and all five tiers; a regression
// here makes the whole priorities API fail closed.
func TestValidStoredNodePriorities(t *testing.T) {
	for _, v := range []string{"disabled", "disabled:highest", "disabled:high", "disabled:medium", "disabled:low", "disabled:lowest",
		"highest", "high", "medium", "low", "lowest"} {
		if !validNodePriority(v) {
			t.Fatalf("valid value rejected: %q", v)
		}
	}
	for _, v := range []string{"", "disabled:", "urgent", "medium:x"} {
		if validNodePriority(v) {
			t.Fatalf("invalid value accepted: %q", v)
		}
	}
	if nodePriorityRank("highest") <= nodePriorityRank("high") || nodePriorityRank("low") <= nodePriorityRank("lowest") {
		t.Fatal("tier order is not monotonic")
	}
	if downgradePriority("highest") != "high" || downgradePriority("lowest") != "lowest" {
		t.Fatal("downgrade does not walk the tiers")
	}
}

func TestBoostNearExpiry(t *testing.T) {
	now := time.Now()
	day := 24 * time.Hour
	cases := []struct {
		remaining time.Duration
		in        string
		want      string
	}{
		{5 * day, "medium", "medium"},
		{2 * day, "medium", "high"},
		{2 * day, "highest", "highest"},
		{12 * time.Hour, "low", "high"},
		{12 * time.Hour, "highest", "highest"},
		{-time.Hour, "medium", "medium"},
	}
	for _, c := range cases {
		got := boostNearExpiry(c.in, now.Add(c.remaining).Unix(), now)
		if got != c.want {
			t.Fatalf("remaining %v from %s: got %s want %s", c.remaining, c.in, got, c.want)
		}
	}
	if boostNearExpiry("medium", 0, now) != "medium" {
		t.Fatal("missing expiry must not boost")
	}
}
