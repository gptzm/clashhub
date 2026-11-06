package main

import (
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSourceConfigRollbackHidesTentativeStateFromRefresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("disabled"), 0600); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- applySourceConfigChange(path,
			func() error { return os.WriteFile(path, []byte("enabled"), 0600) },
			func() error { close(started); <-release; return errors.New("reload failed") },
			func() error { return nil }, func() error { return nil })
	}()
	<-started
	observed := make(chan string, 1)
	go func() {
		customSubscriptionCommitMu.Lock()
		defer customSubscriptionCommitMu.Unlock()
		b, _ := os.ReadFile(path)
		observed <- string(b)
	}()
	select {
	case <-observed:
		t.Fatal("refresh observed tentative configuration")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("expected apply failure")
	}
	select {
	case got := <-observed:
		if got != "disabled" {
			t.Fatalf("refresh observed %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("refresh blocked after rollback")
	}
}

func TestSourceUpdateWithoutRuntimeDoesNotPersist(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeSourcesEnvironment(t, []sourceDef{{ID: "test", Label: "Test", Type: "profile", Enabled: true}}, nil, "")
	before, err := os.ReadFile(configPath())
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("PUT", "/api/sources", strings.NewReader(`{"sources":[{"id":"test","enabled":false}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	(&statsIntegration{}).serveSources(w, req)
	if w.Code != 503 {
		t.Fatalf("expected 503, got %d", w.Code)
	}
	after, err := os.ReadFile(configPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("update persisted without runtime")
	}
}

func TestSourceFiltersPersistAndPreserveLegacyData(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeSourcesEnvironment(t, []sourceDef{{ID: "test", Label: "Test", Type: "profile", Enabled: true, DowngradePatterns: []string{"old"}}}, nil, "")
	on := true
	include, exclude := []string{" JP ", "Tokyo"}, []string{"Home"}
	if err := persistSourcesOrder(configPath(), []sourceUpdateEntry{{ID: "test", Enabled: &on, IncludeKeywords: &include, ExcludeKeywords: &exclude}}); err != nil {
		t.Fatal(err)
	}
	doc, ok := loadNodeSources()
	if !ok || len(doc.Sources) != 1 || strings.Join(doc.Sources[0].IncludeKeywords, ",") != "JP,Tokyo" || strings.Join(doc.Sources[0].ExcludeKeywords, ",") != "Home" || strings.Join(doc.Sources[0].DowngradePatterns, ",") != "old" {
		t.Fatalf("unexpected source: %+v", doc)
	}
	before, err := os.ReadFile(configPath())
	if err != nil {
		t.Fatal(err)
	}
	bad := []string{"  "}
	if err := persistSourcesOrder(configPath(), []sourceUpdateEntry{{ID: "test", Enabled: &on, IncludeKeywords: &bad}}); err == nil {
		t.Fatal("empty keyword accepted")
	}
	after, err := os.ReadFile(configPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("invalid update changed config")
	}
}
