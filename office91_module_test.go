package main

import (
	"testing"
)

func TestOffice91ModuleRequiresExplicitEnable(t *testing.T) {
	cfg := Config{Sources: []SourceConfig{{ID: sourceIDOffice91, Type: SourceTypeSubscription, Enabled: true}}}
	if office91SourceEnabledInConfig(cfg) {
		t.Fatal("missing module opt-in enabled office91")
	}
	cfg.Office91Enabled = true
	if !office91SourceEnabledInConfig(cfg) {
		t.Fatal("explicit module opt-in did not enable office91")
	}
	cfg.Sources[0].Enabled = false
	if office91SourceEnabledInConfig(cfg) {
		t.Fatal("disabled source enabled office91")
	}
}

func TestLoadConfigRemovesOnlyXLinkerSource(t *testing.T) {
	home := fakeHome(t)
	profile, _ := defaultProfilePaths()
	writeConfigFile(t, home, map[string]any{"sources": []map[string]any{
		{"id": "profile", "label": "Profile", "type": "profile", "path": profile, "enabled": true},
		{"id": "xlinker", "label": "XLinker", "type": "vpn", "enabled": true},
		{"id": "other-vpn", "label": "Other", "type": "vpn", "enabled": true},
	}, "base_rules_source": "profile"})
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Sources) != 2 || cfg.Sources[0].ID != "profile" || cfg.Sources[1].ID != "other-vpn" {
		t.Fatalf("sources: %+v", cfg.Sources)
	}
	if cfg.Office91Enabled {
		t.Fatal("missing module flag unexpectedly enabled")
	}
}
