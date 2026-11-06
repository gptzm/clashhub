package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAutoProxyAllowed(t *testing.T) {
	cfg := defaultConfig()
	ok := ProxySetting{Enabled: true, Server: "127.0.0.1", Port: 33210}
	own := ProxySetting{Enabled: true, Server: "127.0.0.1", Port: 33333}
	cases := []struct {
		name    string
		s       ServiceSnapshot
		allowed bool
	}{
		{"disabled", ServiceSnapshot{Name: "Wi-Fi"}, true},
		{"fallback", ServiceSnapshot{Name: "Wi-Fi", Web: ok, Secure: ok, Socks: ok}, true},
		{"own", ServiceSnapshot{Name: "Wi-Fi", Web: own, Socks: ProxySetting{Enabled: true, Server: "127.0.0.1", Port: 33334}}, true},
		{"other", ServiceSnapshot{Name: "Wi-Fi", Web: ProxySetting{Enabled: true, Server: "127.0.0.1", Port: 7890}}, false},
		{"remote", ServiceSnapshot{Name: "Wi-Fi", Web: ProxySetting{Enabled: true, Server: "192.168.1.2", Port: 33210}}, false},
		{"pac", ServiceSnapshot{Name: "Wi-Fi", PACEnabled: true}, false},
		{"discovery", ServiceSnapshot{Name: "Wi-Fi", AutoDiscovery: true}, false},
		{"auth disabled", ServiceSnapshot{Name: "Wi-Fi", Web: ProxySetting{Auth: true}}, false},
		{"wrong protocol", ServiceSnapshot{Name: "Wi-Fi", Socks: own}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := autoProxyAllowed([]ServiceSnapshot{tc.s}, cfg)
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v err=%v", tc.allowed, err)
			}
		})
	}
}

func TestAutoSnapshotRetry(t *testing.T) {
	cfg := defaultConfig()
	calls, sleeps := 0, 0
	snap := func() ([]ServiceSnapshot, error) {
		calls++
		if calls < 3 {
			return nil, errors.New("not ready")
		}
		return []ServiceSnapshot{{Name: "Wi-Fi"}}, nil
	}
	_, err := autoSnapshotRetry(cfg, 4, time.Second, snap, func(time.Duration) { sleeps++ })
	if err != nil || calls != 3 || sleeps != 2 {
		t.Fatalf("calls=%d sleeps=%d err=%v", calls, sleeps, err)
	}
	calls = 0
	sleeps = 0
	_, err = autoSnapshotRetry(cfg, 4, time.Second, func() ([]ServiceSnapshot, error) {
		calls++
		return []ServiceSnapshot{{Name: "Wi-Fi", PACEnabled: true}}, nil
	}, func(time.Duration) { sleeps++ })
	if err == nil || calls != 1 || sleeps != 0 || !strings.Contains(err.Error(), "PAC") {
		t.Fatalf("calls=%d sleeps=%d err=%v", calls, sleeps, err)
	}
	calls = 0
	sleeps = 0
	_, err = autoSnapshotRetry(cfg, 3, time.Second, func() ([]ServiceSnapshot, error) { calls++; return nil, errors.New("unavailable") }, func(time.Duration) { sleeps++ })
	if err == nil || calls != 3 || sleeps != 2 {
		t.Fatalf("calls=%d sleeps=%d err=%v", calls, sleeps, err)
	}
}

func TestAutoProxyConfiguredFallback(t *testing.T) {
	cfg := defaultConfig()
	cfg.FallbackAddr = "192.0.2.10:4567"
	fallback := ProxySetting{Enabled: true, Server: "192.0.2.10", Port: 4567}
	if err := autoProxyAllowed([]ServiceSnapshot{{Name: "wifi", Web: fallback}}, cfg); err != nil {
		t.Fatal(err)
	}
	fallback.Port = 33210
	if autoProxyAllowed([]ServiceSnapshot{{Name: "wifi", Web: fallback}}, cfg) == nil {
		t.Fatal("accepted different port")
	}
	fallback.Server = "127.0.0.1"
	if autoProxyAllowed([]ServiceSnapshot{{Name: "wifi", Web: fallback}}, cfg) == nil {
		t.Fatal("accepted legacy fallback instead of configured address")
	}
	cfg.FallbackAddr = "[::1]:4567"
	fallback = ProxySetting{Enabled: true, Server: "::1", Port: 4567}
	if err := autoProxyAllowed([]ServiceSnapshot{{Name: "wifi", Web: fallback}}, cfg); err != nil {
		t.Fatal(err)
	}
	fallback.Auth = true
	if autoProxyAllowed([]ServiceSnapshot{{Name: "wifi", Web: fallback}}, cfg) == nil {
		t.Fatal("accepted authentication")
	}
}
