package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Reconcile must only append missing FlClash profiles and keep every existing
// entry untouched. It is skipped when this machine has no FlClash database.
func TestReconcileDiscoveredSourcesPreservesConfig(t *testing.T) {
	if _, err := os.Stat(flclashDatabasePath()); err != nil {
		t.Skip("no FlClash database")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	original := `{"sources":[{"id":"keep","label":"Keep","type":"profile","path":"/x/1.yaml","enabled":false,"default_priority":"low"}],"other":"v"}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	added, err := reconcileDiscoveredSources(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) == 0 {
		t.Skip("no new profiles to discover")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Sources []map[string]any `json:"sources"`
		Other   string           `json:"other"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Other != "v" {
		t.Fatal("unrelated config keys were dropped")
	}
	if len(doc.Sources) != len(added)+1 {
		t.Fatalf("sources %d, added %d", len(doc.Sources), len(added))
	}
	first := doc.Sources[0]
	if first["id"] != "keep" || first["enabled"] != false || first["default_priority"] != "low" {
		t.Fatalf("existing entry changed: %+v", first)
	}
	for _, e := range doc.Sources[1:] {
		if e["enabled"] != true {
			t.Fatalf("discovered entry not enabled: %+v", e)
		}
		if id, _ := e["id"].(string); len(id) < len("flclash-") || id[:len("flclash-")] != "flclash-" {
			t.Fatalf("unexpected id %v", e["id"])
		}
	}
	// A second pass must be a no-op.
	if again, err := reconcileDiscoveredSources(path); err != nil || len(again) != 0 {
		t.Fatalf("second pass added %d (%v)", len(again), err)
	}
}
