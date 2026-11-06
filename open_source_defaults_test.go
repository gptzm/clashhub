package main

import (
	"path/filepath"
	"testing"
)

// Legacy fixtures only: current defaults deliberately do not contain profiles.
func defaultProfilePaths() (string, string) {
	return filepath.Join(userHome(), "profiles", "fixture-a.yaml"), filepath.Join(userHome(), "profiles", "fixture-b.yaml")
}

func TestOpenSourceDefaultsContainNoPersonalSources(t *testing.T) {
	c := defaultConfig()
	if len(c.Sources) != 0 || len(c.SourceProfiles) != 0 || c.BaseRulesSource != "" {
		t.Fatal("fresh install must not require personal profiles")
	}
	if c.Office91Enabled {
		t.Fatal("the 玖壹 integration must stay opt-in")
	}
	if err := validateSources(c.Sources, c.BaseRulesSource); err != nil {
		t.Fatal(err)
	}
}
