package main

import (
	"errors"
	"testing"
)

func TestRepairProxyService(t *testing.T) {
	own := ProxySetting{Server: "127.0.0.1", Port: 33333, Enabled: true}
	socks := ProxySetting{Server: "127.0.0.1", Port: 33334, Enabled: true}
	base := ServiceSnapshot{Name: "Wi-Fi", Web: own, Secure: own, Socks: socks}
	tests := []struct {
		name       string
		mutate     func(*ServiceSnapshot)
		wantWrites int
		conflict   bool
	}{
		{"healthy", func(s *ServiceSnapshot) {}, 0, false},
		{"disabled web", func(s *ServiceSnapshot) { s.Web.Enabled = false }, 1, false},
		{"all disabled", func(s *ServiceSnapshot) { s.Web.Enabled = false; s.Secure.Enabled = false; s.Socks.Enabled = false }, 3, false},
		{"other proxy", func(s *ServiceSnapshot) { s.Web.Server = "other"; s.Socks.Enabled = false }, 0, true},
		{"empty address", func(s *ServiceSnapshot) { s.Web.Server = ""; s.Web.Enabled = false }, 0, true},
		{"pac", func(s *ServiceSnapshot) { s.PACEnabled = true; s.Web.Enabled = false }, 0, true},
		{"discovery", func(s *ServiceSnapshot) { s.AutoDiscovery = true; s.Web.Enabled = false }, 0, true},
		{"auth", func(s *ServiceSnapshot) { s.Socks.Auth = true; s.Web.Enabled = false }, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			current := base
			tt.mutate(&current)
			writes := 0
			read := func(string) (ServiceSnapshot, error) { return current, nil }
			write := func(args ...string) (string, error) {
				writes++
				switch args[0] {
				case "-setwebproxystate":
					current.Web.Enabled = true
				case "-setsecurewebproxystate":
					current.Secure.Enabled = true
				case "-setsocksfirewallproxystate":
					current.Socks.Enabled = true
				}
				return "", nil
			}
			changed, err := repairProxyService("Wi-Fi", "127.0.0.1:33333", "127.0.0.1:33334", read, write, func() bool { return false })
			if (err != nil) != tt.conflict || writes != tt.wantWrites || changed != (writes > 0) {
				t.Fatalf("changed=%v writes=%d err=%v", changed, writes, err)
			}
		})
	}
}
func TestRepairProxyServiceTakeoverAndShutdown(t *testing.T) {
	s := ServiceSnapshot{Web: ProxySetting{Server: "127.0.0.1", Port: 33333}, Secure: ProxySetting{Server: "127.0.0.1", Port: 33333}, Socks: ProxySetting{Server: "127.0.0.1", Port: 33334}}
	writes := 0
	read := func(string) (ServiceSnapshot, error) {
		if writes == 1 {
			s.Secure.Server = "external"
		}
		return s, nil
	}
	write := func(args ...string) (string, error) { writes++; s.Web.Enabled = true; return "", nil }
	changed, err := repairProxyService("Wi-Fi", "127.0.0.1:33333", "127.0.0.1:33334", read, write, func() bool { return false })
	if changed || err == nil || writes != 1 {
		t.Fatalf("changed=%v err=%v writes=%d", changed, err, writes)
	}
	writes = 0
	_, err = repairProxyService("Wi-Fi", "127.0.0.1:33333", "127.0.0.1:33334", read, write, func() bool { return true })
	if err == nil || writes != 0 {
		t.Fatalf("shutdown: %v writes %d", err, writes)
	}
	_, err = repairProxyService("Wi-Fi", "127.0.0.1:33333", "127.0.0.1:33334", func(string) (ServiceSnapshot, error) { return s, errors.New("unreadable") }, write, func() bool { return false })
	if err == nil || writes != 0 {
		t.Fatalf("unreadable: %v writes %d", err, writes)
	}
}

func TestRepairProxyServiceUnverifiedWrite(t *testing.T) {
	s := ServiceSnapshot{Web: ProxySetting{Server: "127.0.0.1", Port: 33333}, Secure: ProxySetting{Enabled: true, Server: "127.0.0.1", Port: 33333}, Socks: ProxySetting{Enabled: true, Server: "127.0.0.1", Port: 33334}}
	changed, err := repairProxyService("Wi-Fi", "127.0.0.1:33333", "127.0.0.1:33334", func(string) (ServiceSnapshot, error) { return s, nil }, func(...string) (string, error) { return "", nil }, func() bool { return false })
	if changed || err == nil {
		t.Fatalf("unverified no-op: changed=%v err=%v", changed, err)
	}
}
