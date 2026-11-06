package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalClashDiscoveryPreservesSettingsAndIgnoresInvalid(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "clash")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	valid := []byte("proxies:\n  - {name: JP, type: http, server: example.org, port: 80}\n")
	for name, body := range map[string][]byte{"valid.yaml": valid, "invalid.yaml": []byte("proxies: ["), "empty.yml": []byte("proxies: []"), "generated.txt": valid} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(dir, "valid.yaml"), filepath.Join(dir, "linked.yaml")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.json")
	original := `{"sources":[{"id":"existing","label":"Existing","type":"profile","path":"/other.yaml","enabled":false}],"source_profiles":["/other.yaml"],"untouched":true}`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	added, err := reconcileDiscoveredSources(path)
	if err != nil || len(added) != 1 || added[0].Enabled || added[0].Path != filepath.Join(dir, "valid.yaml") {
		t.Fatalf("unexpected discovery: %+v %v", added, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Sources        []SourceConfig `json:"sources"`
		SourceProfiles []string       `json:"source_profiles"`
		Untouched      bool           `json:"untouched"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.Untouched || len(doc.Sources) != 2 || doc.Sources[0].Enabled {
		t.Fatal("existing configuration changed")
	}
	if again, err := reconcileDiscoveredSources(path); err != nil || len(again) != 0 {
		t.Fatalf("duplicate discovery: %+v %v", again, err)
	}
}
