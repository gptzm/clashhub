package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigPolicyAndExtensionsSurviveMigration(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(appSupportDir(), 0700); err != nil {
		t.Fatal(err)
	}
	raw := `{"sources":[{"id":"custom","label":"Custom","type":"profile","enabled":true,"path":"/tmp/profile.yaml","default_priority":"low","downgrade_patterns":[],"future_policy":{"mode":"keep"}}],"future_config":{"enabled":true}}`
	if err := os.WriteFile(configPath(), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sources[0].DefaultPriority != "low" || cfg.Sources[0].DowngradePatterns == nil {
		t.Fatal("policy lost while loading")
	}
	cfg.Sources[0].Label = "Edited"
	if err = saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(appSupportDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err = json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	var extension map[string]bool
	if json.Unmarshal(got["future_config"], &extension) != nil || !extension["enabled"] {
		t.Fatal("top-level extension lost")
	}

	var rows []map[string]json.RawMessage
	json.Unmarshal(got["sources"], &rows)
	if string(rows[0]["default_priority"]) != `"low"` || string(rows[0]["downgrade_patterns"]) != "[]" || rows[0]["future_policy"] == nil || string(rows[0]["label"]) != `"Edited"` {
		t.Fatalf("source fields not preserved: %s", got["sources"])
	}
	reloaded, err := loadConfig()
	if err != nil || reloaded.Sources[0].DowngradePatterns == nil {
		t.Fatalf("reload: %v", err)
	}
}

func TestSourcePolicyDefaultAndExplicitEmptyRemainDistinct(t *testing.T) {
	for _, raw := range []string{`{"id":"s"}`, `{"id":"s","downgrade_patterns":null}`, `{"id":"s","downgrade_patterns":[]}`, `{"id":"s","downgrade_patterns":["test"]}`} {
		var source SourceConfig
		if err := json.Unmarshal([]byte(raw), &source); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(source)
		if err != nil {
			t.Fatal(err)
		}
		var round SourceConfig
		json.Unmarshal(data, &round)
		if (source.DowngradePatterns == nil) != (round.DowngradePatterns == nil) || len(source.DowngradePatterns) != len(round.DowngradePatterns) {
			t.Fatalf("changed policy: %s", raw)
		}
	}
}
