package main

import "testing"

func TestRoutingConfigValidation(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:33210", "[::1]:8080", "proxy.local:1234"} {
		cfg := defaultConfig()
		cfg.FallbackAddr = addr
		if err := validateRoutingConfig(cfg); err != nil {
			t.Fatal(err)
		}
	}
	for _, addr := range []string{"host:0", "host:65536", ":8080", "::1:8080", "host:bad"} {
		cfg := defaultConfig()
		cfg.FallbackAddr = addr
		if validateRoutingConfig(cfg) == nil {
			t.Fatalf("accepted %q", addr)
		}
	}
	cfg := defaultConfig()
	cfg.AggregateKeywords = []string{" "}
	if validateRoutingConfig(cfg) == nil {
		t.Fatal("accepted blank keyword")
	}
}
