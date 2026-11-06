package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func writeConfigFile(t *testing.T, home string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, "Library", "Application Support", "ClashHub"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath(), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readConfigRaw(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(configPath())
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDefaultConfigSourcesContract(t *testing.T) {
	fakeHome(t)
	cfg := defaultConfig()
	if len(cfg.Sources) != 0 || len(cfg.SourceProfiles) != 0 || cfg.BaseRulesSource != "" {
		t.Fatalf("fresh config contains sources: %#v", cfg.Sources)
	}
	if cfg.SubscriptionRefreshHours != 6 {
		t.Fatalf("subscription_refresh_hours: %d", cfg.SubscriptionRefreshHours)
	}
}

func TestLoadConfigMigratesLegacySourceProfiles(t *testing.T) {
	home := fakeHome(t)
	big, ktm := defaultProfilePaths()
	writeConfigFile(t, home, map[string]any{"source_profiles": []string{big, ktm}})
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(cfg.Sources))
	for _, s := range cfg.Sources {
		ids = append(ids, s.ID)
	}
	if !reflect.DeepEqual(ids, []string{"fixture-a", "fixture-b"}) {
		t.Fatalf("migrated ids: %v", ids)
	}
	if cfg.BaseRulesSource != "fixture-a" {
		t.Fatalf("base: %q", cfg.BaseRulesSource)
	}
	if !reflect.DeepEqual(cfg.SourceProfiles, []string{big, ktm}) {
		t.Fatalf("derived profiles: %#v", cfg.SourceProfiles)
	}
	// The migration is persisted: sources exist alongside the derived
	// source_profiles list so older binaries can still roll back.
	raw := readConfigRaw(t)
	if _, ok := raw["sources"]; !ok {
		t.Fatal("sources not persisted")
	}
	if _, ok := raw["source_profiles"]; !ok {
		t.Fatal("derived source_profiles not persisted")
	}
	var persisted []string
	if err := json.Unmarshal(raw["source_profiles"], &persisted); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(persisted, []string{big, ktm}) {
		t.Fatalf("persisted source_profiles: %#v", persisted)
	}
	var sources []SourceConfig
	if err := json.Unmarshal(raw["sources"], &sources); err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 {
		t.Fatalf("persisted sources: %#v", sources)
	}
	// Loading the migrated file again is stable.
	again, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.Sources, cfg.Sources) {
		t.Fatalf("unstable migration: %#v", again.Sources)
	}
}

func TestLoadConfigMigratesCustomProfilesStably(t *testing.T) {
	home := fakeHome(t)
	custom := filepath.Join(home, "Library", "Application Support", "com.follow.clash", "profiles", "999888777.yaml")
	writeConfigFile(t, home, map[string]any{"source_profiles": []string{custom, custom}})
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range cfg.Sources {
		ids = append(ids, s.ID)
	}
	if !reflect.DeepEqual(ids, []string{"999888777"}) {
		t.Fatalf("ids: %v", ids)
	}
	again, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.Sources, cfg.Sources) {
		t.Fatal("migration ids unstable across loads")
	}
}

func TestLoadConfigDerivesSourceProfilesFromSources(t *testing.T) {
	home := fakeHome(t)
	big, ktm := defaultProfilePaths()
	writeConfigFile(t, home, map[string]any{
		"sources": []map[string]any{
			{"id": "ktm-cloud", "label": "KTM Cloud", "type": "profile", "path": ktm, "enabled": true},
			{"id": "big-airport", "label": "大机场 Big Airport", "type": "profile", "path": big, "enabled": false},
			{"id": "office91", "label": "玖壹 Office91", "type": "subscription", "enabled": true},
			{"id": "xlinker", "label": "XLinker · 速联", "type": "vpn", "enabled": true},
		},
		"base_rules_source": "ktm-cloud",
		// Stale value must be ignored: sources is authoritative.
		"source_profiles": []string{"/stale/path.yaml"},
	})
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.SourceProfiles, []string{ktm}) {
		t.Fatalf("derived: %#v", cfg.SourceProfiles)
	}
	// The stale file value is not rewritten back (no drift persisted).
	if persisted := readConfigRaw(t); strings.Contains(string(persisted["source_profiles"]), "stale") {
		t.Fatalf("stale source_profiles persisted: %s", persisted["source_profiles"])
	}
}

func TestLoadConfigRejectsInvalidSources(t *testing.T) {
	cases := []struct {
		name    string
		sources []map[string]any
		base    string
	}{
		{"duplicate ids", []map[string]any{
			{"id": "a", "label": "A", "type": "profile", "path": "/tmp/a.yaml", "enabled": true},
			{"id": "a", "label": "B", "type": "profile", "path": "/tmp/b.yaml", "enabled": true},
		}, ""},
		{"case-insensitive duplicate", []map[string]any{
			{"id": "a", "label": "A", "type": "profile", "path": "/tmp/a.yaml", "enabled": true},
			{"id": "A", "label": "B", "type": "profile", "path": "/tmp/b.yaml", "enabled": true},
		}, ""},
		{"unknown type", []map[string]any{
			{"id": "a", "label": "A", "type": "carrier-pigeon", "enabled": true},
		}, ""},
		{"profile without path", []map[string]any{
			{"id": "a", "label": "A", "type": "profile", "enabled": true},
		}, ""},
		{"empty id", []map[string]any{
			{"id": "", "label": "A", "type": "vpn", "enabled": true},
		}, ""},
		{"invalid id chars", []map[string]any{
			{"id": "Big Airport", "label": "A", "type": "vpn", "enabled": true},
		}, ""},
		{"base unknown", []map[string]any{
			{"id": "a", "label": "A", "type": "profile", "path": "/tmp/a.yaml", "enabled": true},
		}, "nope"},
		{"base is vpn", []map[string]any{
			{"id": "a", "label": "A", "type": "profile", "path": "/tmp/a.yaml", "enabled": true},
			{"id": "v", "label": "V", "type": "vpn", "enabled": true},
		}, "v"},
		{"base disabled", []map[string]any{
			{"id": "a", "label": "A", "type": "profile", "path": "/tmp/a.yaml", "enabled": true},
			{"id": "b", "label": "B", "type": "profile", "path": "/tmp/b.yaml", "enabled": false},
		}, "b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := fakeHome(t)
			doc := map[string]any{}
			if tc.sources != nil {
				doc["sources"] = tc.sources
			} else {
				doc["sources"] = []map[string]any{}
			}
			if tc.base != "" {
				doc["base_rules_source"] = tc.base
			}
			writeConfigFile(t, home, doc)
			if _, err := loadConfig(); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestLoadConfigBaseRulesDefault(t *testing.T) {
	home := fakeHome(t)
	writeConfigFile(t, home, map[string]any{
		"sources": []map[string]any{
			{"id": "ktm-cloud", "label": "KTM Cloud", "type": "profile", "path": "/tmp/ktm.yaml", "enabled": true},
		},
	})
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseRulesSource != "ktm-cloud" {
		t.Fatalf("base default: %q", cfg.BaseRulesSource)
	}
}

func TestLoadConfigNormalizesRefreshHours(t *testing.T) {
	for _, hours := range []int{0, -5} {
		home := fakeHome(t)
		writeConfigFile(t, home, map[string]any{
			"sources":                    []map[string]any{{"id": "ktm-cloud", "label": "KTM Cloud", "type": "profile", "path": "/tmp/ktm.yaml", "enabled": true}},
			"subscription_refresh_hours": hours,
		})
		cfg, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.SubscriptionRefreshHours != 6 {
			t.Fatalf("hours %d -> %d", hours, cfg.SubscriptionRefreshHours)
		}
	}
}

func TestSaveConfigKeepsDerivedSourceProfiles(t *testing.T) {
	fakeHome(t)
	big, ktm := defaultProfilePaths()
	cfg := defaultConfig()
	cfg.Sources = []SourceConfig{{ID: "a", Label: "A", Type: SourceTypeProfile, Path: big, Enabled: true}, {ID: "b", Label: "B", Type: SourceTypeProfile, Path: ktm, Enabled: true}}
	cfg.SourceProfiles = []string{"/must/be/rederived.yaml"} // must be recomputed
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	raw := readConfigRaw(t)
	var persisted []string
	if err := json.Unmarshal(raw["source_profiles"], &persisted); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(persisted, []string{big, ktm}) {
		t.Fatalf("persisted: %#v", persisted)
	}
	loaded, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.Sources, cfg.Sources) || !reflect.DeepEqual(loaded.SourceProfiles, persisted) {
		t.Fatal("roundtrip mismatch")
	}
}

func TestLoadConfigFreshInstallWritesFullConfig(t *testing.T) {
	fakeHome(t)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	raw := readConfigRaw(t)
	for _, key := range []string{"sources", "base_rules_source", "subscription_refresh_hours", "source_profiles"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("missing key %q", key)
		}
	}
	if len(cfg.Sources) != 0 {
		t.Fatalf("sources: %#v", cfg.Sources)
	}
}
