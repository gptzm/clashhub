package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSafeExitChoicesAndConflicts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	oldProbe, oldSnap, oldSetup := exitProbe, exitSnapshot, exitSetup
	defer func() { exitProbe = oldProbe; exitSnapshot = oldSnap; exitSetup = oldSetup }()
	original := ProxySetting{Enabled: true, Server: "original", Port: 8080}
	owned := ProxySetting{Enabled: true, Server: "127.0.0.1", Port: 33333}
	socks := ProxySetting{Enabled: true, Server: "127.0.0.1", Port: 33334}
	for _, tc := range []struct {
		name                             string
		originalOK, fallbackOK, conflict bool
		expected                         string
		wantErr                          bool
	}{
		{"original preferred", true, true, false, "original", false},
		{"fallback", false, true, false, "127.0.0.1", false},
		{"disable", false, false, false, "off", false},
		{"external change", true, true, true, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			cur := ServiceSnapshot{Name: "Wi-Fi", Web: owned, Secure: owned, Socks: socks}
			if tc.conflict {
				cur.Web = ProxySetting{Enabled: true, Server: "external", Port: 99}
			}
			snapshot := ServiceSnapshot{Name: "Wi-Fi", Web: original, Secure: original, Socks: original}
			exitSnapshot = func(string) (ServiceSnapshot, error) { return cur, nil }
			exitProbe = func(p ProxySetting, _ bool, _ string, _ time.Duration) error {
				if p.Server == "original" && tc.originalOK || p.Port == 7890 && tc.fallbackOK {
					return nil
				}
				return fmt.Errorf("offline")
			}
			var web string
			exitSetup = func(args ...string) (string, error) {
				p := &cur.Web
				if strings.Contains(args[0], "secure") {
					p = &cur.Secure
				}
				if strings.Contains(args[0], "socks") {
					p = &cur.Socks
				}
				if strings.HasSuffix(args[0], "state") {
					p.Enabled = false
					if p == &cur.Web {
						web = "off"
					}
				} else {
					port, _ := strconv.Atoi(args[3])
					*p = ProxySetting{Enabled: true, Server: args[2], Port: port}
					if p == &cur.Web {
						web = args[2]
					}
				}
				return "", nil
			}
			err := safeExitAll([]ServiceSnapshot{snapshot}, "127.0.0.1:33333", "127.0.0.1:33334")
			if (err != nil) != tc.wantErr || web != tc.expected {
				t.Fatalf("web=%q err=%v", web, err)
			}
		})
	}
}

func TestSafeExitConfiguredFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := defaultConfig()
	cfg.FallbackAddr = "192.0.2.10:4567"
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	oldProbe, oldSnap, oldSetup := exitProbe, exitSnapshot, exitSetup
	defer func() { exitProbe = oldProbe; exitSnapshot = oldSnap; exitSetup = oldSetup }()
	cur := ServiceSnapshot{Name: "wifi", Web: ProxySetting{Enabled: true, Server: "127.0.0.1", Port: 33333}}
	exitSnapshot = func(string) (ServiceSnapshot, error) { return cur, nil }
	exitProbe = func(p ProxySetting, _ bool, _ string, _ time.Duration) error {
		if p.Server == "192.0.2.10" && p.Port == 4567 {
			return nil
		}
		return fmt.Errorf("offline")
	}
	exitSetup = func(args ...string) (string, error) {
		if args[0] == "-setwebproxy" {
			port, _ := strconv.Atoi(args[3])
			cur.Web = ProxySetting{Enabled: true, Server: args[2], Port: port}
		}
		return "", nil
	}
	if err := safeExitAll([]ServiceSnapshot{{Name: "wifi"}}, "127.0.0.1:33333", "127.0.0.1:33334"); err != nil {
		t.Fatal(err)
	}
	if cur.Web.Server != "192.0.2.10" || cur.Web.Port != 4567 {
		t.Fatalf("wrong fallback: %+v", cur.Web)
	}
}

func TestExitProbeRejectsOpenButNonProxy(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			c.Close()
		}
	}()
	port := l.Addr().(*net.TCPAddr).Port
	for _, socks := range []bool{false, true} {
		if err := probeExitProxy(ProxySetting{Enabled: true, Server: "127.0.0.1", Port: port}, socks, "https://example.com/", time.Second); err == nil {
			t.Fatal("accepted dead proxy")
		}
	}
}

func TestExitProbeRejectsBadStatus(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(502) }))
	defer proxy.Close()
	host, ps, _ := net.SplitHostPort(strings.TrimPrefix(proxy.URL, "http://"))
	port, _ := strconv.Atoi(ps)
	if err := probeExitProxy(ProxySetting{Enabled: true, Server: host, Port: port}, false, "https://example.com", time.Second); err == nil {
		t.Fatal("accepted broken proxy")
	}
}

func TestSafeExitPACConflict(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p, s, n := exitProbe, exitSnapshot, exitSetup
	defer func() { exitProbe = p; exitSnapshot = s; exitSetup = n }()
	exitProbe = func(ProxySetting, bool, string, time.Duration) error { return nil }
	exitSnapshot = func(string) (ServiceSnapshot, error) { return ServiceSnapshot{Name: "x", PACURL: "changed"}, nil }
	exitSetup = func(...string) (string, error) { t.Fatal("unexpected write"); return "", nil }
	if err := safeExitAll([]ServiceSnapshot{{Name: "x"}}, "127.0.0.1:33333", "127.0.0.1:33334"); err == nil {
		t.Fatal("expected conflict")
	}
}

func TestExitJournalCrashAndConflict(t *testing.T) {
	for _, stage := range []string{"before-write", "after-write", "external-after-write", "failed-intent"} {
		t.Run(stage, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			p, s, n, w := exitProbe, exitSnapshot, exitSetup, persistExitJournal
			defer func() { exitProbe = p; exitSnapshot = s; exitSetup = n; persistExitJournal = w }()
			owned := ProxySetting{Enabled: true, Server: "127.0.0.1", Port: 33333}
			old := ServiceSnapshot{Name: "wifi"}
			cur := ServiceSnapshot{Name: "wifi", Web: owned}
			exitProbe = func(p ProxySetting, _ bool, _ string, _ time.Duration) error {
				if p.Port == 7890 {
					return nil
				}
				return fmt.Errorf("offline")
			}
			exitSnapshot = func(string) (ServiceSnapshot, error) { return cur, nil }
			writes := 0
			exitSetup = func(args ...string) (string, error) {
				if args[0] != "-setwebproxy" {
					t.Fatalf("unexpected mutation %v", args)
				}
				writes++
				if stage == "before-write" && writes == 1 {
					return "", fmt.Errorf("injected before write")
				}
				cur.Web = ProxySetting{Enabled: true, Server: "127.0.0.1", Port: 7890}
				if (stage == "after-write" || stage == "external-after-write") && writes == 1 {
					return "", fmt.Errorf("injected after write")
				}
				return "", nil
			}
			if stage == "failed-intent" {
				persistExitJournal = func(*exitJournal) error { return fmt.Errorf("disk failure") }
			}
			if err := safeExitAll([]ServiceSnapshot{old}, "127.0.0.1:33333", "127.0.0.1:33334"); err == nil {
				t.Fatal("expected injected failure")
			}
			if stage == "failed-intent" && writes != 0 {
				t.Fatal("write occurred without durable intent")
			}
			persistExitJournal = w
			if stage == "external-after-write" {
				cur.Web = ProxySetting{Enabled: true, Server: "external", Port: 456}
			}
			err := safeExitAll([]ServiceSnapshot{old}, "127.0.0.1:33333", "127.0.0.1:33334")
			if stage == "external-after-write" {
				if err == nil || cur.Web.Server != "external" {
					t.Fatalf("conflict overwritten: %v %+v", err, cur.Web)
				}
			} else if err != nil || cur.Web.Port != 7890 {
				t.Fatalf("recovery failed: %v %+v", err, cur.Web)
			}
		})
	}
}

func TestExitLegacyPACRestore(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p, s, n := exitProbe, exitSnapshot, exitSetup
	defer func() { exitProbe = p; exitSnapshot = s; exitSetup = n }()
	old := ServiceSnapshot{Name: "wifi", PACURL: "https://example.test/pac", PACEnabled: true, AutoDiscovery: true}
	cur := old
	cur.PACEnabled = false
	cur.AutoDiscovery = false
	exitProbe = func(ProxySetting, bool, string, time.Duration) error { return fmt.Errorf("offline") }
	exitSnapshot = func(string) (ServiceSnapshot, error) { return cur, nil }
	exitSetup = func(args ...string) (string, error) {
		switch args[0] {
		case "-setautoproxystate":
			cur.PACEnabled = true
		case "-setproxyautodiscovery":
			cur.AutoDiscovery = true
		default:
			t.Fatalf("unexpected %v", args)
		}
		return "", nil
	}
	if err := safeExitAll([]ServiceSnapshot{old}, "127.0.0.1:33333", "127.0.0.1:33334"); err != nil || !cur.PACEnabled || !cur.AutoDiscovery {
		t.Fatalf("PAC restore: %v %+v", err, cur)
	}
}

func TestExitCorruptJournalFailsClosed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	old := []ServiceSnapshot{{Name: "wifi"}}
	id, err := exitSessionID(old, "127.0.0.1:33333", "127.0.0.1:33334")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(appSupportDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(exitJournalPath(id), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = safeExitAll(old, "127.0.0.1:33333", "127.0.0.1:33334"); err == nil {
		t.Fatal("accepted corrupt journal")
	}
}

func TestApplyRejectsEnabledPACOrDiscovery(t *testing.T) {
	for _, s := range []ServiceSnapshot{{Name: "wifi", PACEnabled: true}, {Name: "wifi", AutoDiscovery: true}} {
		if err := applyOurProxy([]ServiceSnapshot{s}, "127.0.0.1:33333", "127.0.0.1:33334"); err == nil {
			t.Fatalf("accepted unsafe snapshot %+v", s)
		}
	}
}
