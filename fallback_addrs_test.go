package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFallbackAddressValidation(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:33210", "192.168.1.5:7890", "[::1]:1080"} {
		if err := validateFallbackAddr(addr); err != nil {
			t.Fatalf("%s rejected: %v", addr, err)
		}
	}
	for _, addr := range []string{"", "127.0.0.1", ":7890", "host:0", "host:70000", "a b:80"} {
		if err := validateFallbackAddr(addr); err == nil {
			t.Fatalf("%q accepted", addr)
		}
	}
}

func TestPersistFallbackAddressesPreservesConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	original := `{"fallback_addr":"127.0.0.1:33210","sources":[{"id":"a","type":"profile","path":"/p.yaml","enabled":true}],"other":7}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	list := []string{"127.0.0.1:33210", "127.0.0.1:7890"}
	if err := persistFallbackAddresses(path, list); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["other"].(float64) != 7 {
		t.Fatal("unrelated key lost")
	}
	if _, ok := doc["sources"]; !ok {
		t.Fatal("sources lost")
	}
	addrs, _ := doc["fallback_addrs"].([]any)
	if len(addrs) != 2 || addrs[1] != "127.0.0.1:7890" {
		t.Fatalf("fallback_addrs = %+v", doc["fallback_addrs"])
	}
	if doc["fallback_addr"] != "127.0.0.1:33210" {
		t.Fatalf("first address not mirrored: %v", doc["fallback_addr"])
	}
}

func TestNormalizeFallbackAddrs(t *testing.T) {
	cfg := Config{FallbackAddr: "a:1", FallbackAddrs: []string{"a:1", "b:2", "b:2", "c:3"}}
	normalizeFallbackAddrs(&cfg)
	if cfg.FallbackAddr != "a:1" || len(cfg.FallbackAddrs) != 3 || cfg.FallbackAddrs[2] != "c:3" {
		t.Fatalf("normalized %+v", cfg.FallbackAddrs)
	}
}
