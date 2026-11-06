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
)

func deleteTestIntegration(t *testing.T) (*statsIntegration, *int) {
	t.Helper()
	var applies int
	s := &statsIntegration{sourceApply: func(_ context.Context, save func() error) error {
		applies++
		return save()
	}}
	return s, &applies
}

func saveCustomSourceForTest(t *testing.T, s *statsIntegration, label string) string {
	t.Helper()
	urlValue := "https://provider.example/sub?token=private"
	body, _ := json.Marshal(subscriptionSourceRequest{Action: "save", Label: label, URL: &urlValue})
	req := httptest.NewRequest(http.MethodPost, "/api/sources/subscriptions", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.serveSubscriptionSources(w, req)
	if w.Code/100 != 2 {
		t.Fatalf("save failed: %d %s", w.Code, w.Body.String())
	}
	var saved struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil || saved.ID == "" {
		t.Fatalf("save response: %s", w.Body.String())
	}
	return saved.ID
}

func TestDeleteCustomSubscriptionRemovesConfigAndCaches(t *testing.T) {
	home := fakeHome(t)
	writeConfigFile(t, home, defaultConfig())
	s, applies := deleteTestIntegration(t)
	id := saveCustomSourceForTest(t, s, "Removable")
	nodeCache := filepath.Join(appSupportDir(), "sources", id+".yaml")
	if err := os.MkdirAll(filepath.Dir(nodeCache), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nodeCache, []byte("proxies: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/sources/subscriptions?id="+id, nil)
	w := httptest.NewRecorder()
	s.serveSubscriptionSources(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("delete failed: %d %s", w.Code, w.Body.String())
	}
	if *applies != 1 {
		t.Fatalf("engine apply not triggered exactly once: %d", *applies)
	}
	if _, ok, err := readCustomSubscriptionSource(configPath(), id); err != nil || ok {
		t.Fatalf("source still present: ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(subscriptionURLCachePath(id)); !os.IsNotExist(err) {
		t.Fatalf("URL cache survived: %v", err)
	}
	if _, err := os.Stat(nodeCache); !os.IsNotExist(err) {
		t.Fatalf("node cache survived: %v", err)
	}
}

func TestDeleteSubscriptionRejectsProtectedAndMissingSources(t *testing.T) {
	home := fakeHome(t)
	cfg := defaultConfig()
	cfg.Sources = []SourceConfig{{ID: "big-airport", Label: "Base", Type: SourceTypeProfile, Enabled: true}, {ID: sourceIDOffice91, Label: "Office91", Type: SourceTypeSubscription, Enabled: true}}
	writeConfigFile(t, home, cfg)
	s, _ := deleteTestIntegration(t)
	for _, tc := range []struct {
		id   string
		code int
	}{
		{"big-airport", http.StatusBadRequest},
		{sourceIDOffice91, http.StatusBadRequest},
		{customSubscriptionSourcePrefix + "missing", http.StatusNotFound},
		{"not-a-source", http.StatusBadRequest},
	} {
		req := httptest.NewRequest(http.MethodDelete, "/api/sources/subscriptions?id="+tc.id, nil)
		w := httptest.NewRecorder()
		s.serveSubscriptionSources(w, req)
		if w.Code != tc.code {
			t.Fatalf("%s: got %d want %d", tc.id, w.Code, tc.code)
		}
	}
}

func TestDeleteSubscriptionRequiresApply(t *testing.T) {
	home := fakeHome(t)
	writeConfigFile(t, home, defaultConfig())
	s := &statsIntegration{}
	id := saveCustomSourceForTest(t, s, "Keep")
	req := httptest.NewRequest(http.MethodDelete, "/api/sources/subscriptions?id="+id, nil)
	w := httptest.NewRecorder()
	s.serveSubscriptionSources(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d", w.Code)
	}
	if _, ok, _ := readCustomSubscriptionSource(configPath(), id); !ok {
		t.Fatal("source was removed without applying the engine change")
	}
}

func TestOffice91ModuleToggleAppliesEngineChange(t *testing.T) {
	home := fakeHome(t)
	cfg := defaultConfig()
	cfg.Sources = []SourceConfig{{ID: sourceIDOffice91, Label: "Office91", Type: SourceTypeSubscription, Enabled: true}}
	writeConfigFile(t, home, cfg)
	s, applies := deleteTestIntegration(t)
	for _, enabled := range []bool{true, false} {
		body, _ := json.Marshal(map[string]bool{"enabled": enabled})
		req := httptest.NewRequest(http.MethodPost, "/api/office91-module", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.serveOffice91Module(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("toggle %v: %d %s", enabled, w.Code, w.Body.String())
		}
		loaded, err := loadConfig()
		if err != nil || loaded.Office91Enabled != enabled {
			t.Fatalf("flag %v not persisted: %v %v", enabled, loaded.Office91Enabled, err)
		}
	}
	if *applies != 2 {
		t.Fatalf("engine applies = %d", *applies)
	}
}

func TestSourceUpdateReportsPercentage(t *testing.T) {
	home := fakeHome(t)
	id := customSubscriptionSourcePrefix + "abcdef"
	cfg := defaultConfig()
	cfg.Sources = []SourceConfig{
		{ID: id, Label: "Update", Type: SourceTypeSubscription, Enabled: true},
		{ID: "big-airport", Label: "Base", Type: SourceTypeProfile, Enabled: true},
	}
	writeConfigFile(t, home, cfg)
	subscriptionStatusMu.Lock()
	refreshRequests[id] = make(chan struct{}, 1)
	refreshRequests["big-airport"] = make(chan struct{}, 1)
	subscriptionStatusMu.Unlock()
	defer func() {
		subscriptionStatusMu.Lock()
		delete(refreshRequests, id)
		delete(refreshRequests, "big-airport")
		subscriptionStatusMu.Unlock()
	}()
	s := &statsIntegration{}
	req := httptest.NewRequest(http.MethodPost, "/api/sources/update", nil)
	w := httptest.NewRecorder()
	s.serveSourcesUpdate(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("post: %d %s", w.Code, w.Body.String())
	}
	var started struct {
		Total int `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &started); err != nil || started.Total != 2 {
		t.Fatalf("batch: %s", w.Body.String())
	}
	read := func() map[string]any {
		req := httptest.NewRequest(http.MethodGet, "/api/sources/update", nil)
		w := httptest.NewRecorder()
		s.serveSourcesUpdate(w, req)
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("get: %s", w.Body.String())
		}
		return out
	}
	first := read()
	if first["running"] != true || first["percent"].(float64) != 0 {
		t.Fatalf("initial progress: %v", first)
	}
	recordSubscriptionStatus(SubscriptionStatus{SourceID: id, Status: "ok"})
	half := read()
	if half["percent"].(float64) != 50 {
		t.Fatalf("half progress: %v", half)
	}
	recordSubscriptionStatus(SubscriptionStatus{SourceID: "big-airport", Status: "ok"})
	done := read()
	if done["running"] != false || done["percent"].(float64) != 100 {
		t.Fatalf("done progress: %v", done)
	}
}
