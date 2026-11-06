package main

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The helper is a separate child process, never the test runner itself.
func TestEngineChild(t *testing.T) {
	if os.Getenv("CLASHHUB_ENGINE_TEST_CHILD") != "1" {
		return
	}
	if os.Getenv("CLASHHUB_ENGINE_TEST_EXIT") == "1" {
		os.Exit(2)
	}
	for {
		time.Sleep(time.Second)
	}
}

func testEngineApp(t *testing.T) *App {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(engineDataDir(), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(engineLogPath()), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(engineConfigPath(), []byte("secret: test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	a := &App{cfg: Config{PrimaryAddr: addr, Controller: addr, EngineBin: os.Args[0]}, state: &State{}, log: log.New(io.Discard, "", 0)}
	return a
}

func TestEngineExitIsReapedAndCanRestart(t *testing.T) {
	a := testEngineApp(t)
	// The test binary rejects engine flags and exits immediately.
	for i := 0; i < 2; i++ {
		if err := a.startEngine(); err == nil {
			t.Fatal("expected startup failure")
		}
		if a.enginePIDValue() != 0 || a.ownedEngineLocked() != nil {
			t.Fatal("exited engine retained")
		}
	}
}

func TestEngineTimeoutStopsOwnedChild(t *testing.T) {
	a := testEngineApp(t)
	t.Setenv("CLASHHUB_ENGINE_TEST_CHILD", "1")
	old := engineStartupTimeout
	engineStartupTimeout = 200 * time.Millisecond
	t.Cleanup(func() { engineStartupTimeout = old })
	wrapper := filepath.Join(t.TempDir(), "engine")
	script := "#!/bin/sh\nexec " + strconv.Quote(os.Args[0]) + " -test.run=^TestEngineChild$\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	a.cfg.EngineBin = wrapper
	if err := a.startEngine(); err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("expected timeout, got %v", err)
	}
	if a.enginePIDValue() != 0 || a.ownedEngineLocked() != nil {
		t.Fatal("timed-out engine retained")
	}
}

func TestStopOnlyOwnedChild(t *testing.T) {
	a := testEngineApp(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestEngineChild$")
	cmd.Env = append(os.Environ(), "CLASHHUB_ENGINE_TEST_CHILD=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &ownedEngine{cmd: cmd, done: make(chan struct{})}
	a.setOwnedEngineLocked(p)
	a.setEnginePID(cmd.Process.Pid)
	go func() { p.err = cmd.Wait(); close(p.done) }()
	t.Cleanup(func() { a.stopEnginePID(cmd.Process.Pid) })
	a.stopEnginePID(-1)
	select {
	case <-p.done:
		t.Fatal("wrong PID stopped child")
	default:
	}
	a.stopEnginePID(cmd.Process.Pid)
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("child not reaped")
	}
	if a.enginePIDValue() != 0 || a.ownedEngineLocked() != nil {
		t.Fatal("stopped engine retained")
	}
}

func TestRestoreManualNodesReadback(t *testing.T) {
	for _, tc := range []struct {
		name     string
		behavior string
		wantErr  bool
	}{
		{"confirmed", "apply", false},
		{"ignored put", "ignore", true},
		{"readback failure", "error", true},
		{"missing selector", "missing", true},
		{"node no longer selectable", "unavailable", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testEngineApp(t)
			if err := saveNodeSelection(manualNodePath(), map[string]string{"主代理": "node1"}); err != nil {
				t.Fatal(err)
			}
			selected := "node2"
			gets, puts := 0, 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/proxies":
					gets++
					if gets > 1 && tc.behavior == "error" {
						http.Error(w, "failed", 500)
						return
					}
					proxies := map[string]proxyInfo{"主代理": {Type: "Selector", All: []string{"node1", "node2"}, Now: selected}, "node1": {Type: "Shadowsocks"}, "node2": {Type: "Shadowsocks"}}
					if gets > 1 && tc.behavior == "missing" {
						delete(proxies, "主代理")
					}
					if gets > 1 && tc.behavior == "unavailable" {
						delete(proxies, "node1")
					}
					_ = json.NewEncoder(w).Encode(proxySnapshot{Proxies: proxies})
				case r.Method == http.MethodPut && r.URL.Path == "/proxies/主代理":
					puts++
					if tc.behavior != "ignore" {
						selected = "node1"
					}
					w.WriteHeader(http.StatusNoContent)
				default:
					http.NotFound(w, r)
				}
			}))
			defer s.Close()
			a.cfg.Controller = strings.TrimPrefix(s.URL, "http://")
			a.cfg.ProbeURL = "https://example.com/"
			err := a.restoreManualNodesVerifiedLocked()
			if (err != nil) != tc.wantErr {
				t.Fatalf("unexpected error: %v", err)
			}
			if gets != 2 || puts != 1 {
				t.Fatalf("got %d snapshots and %d PUTs", gets, puts)
			}
			prefs, err := readManualNodes()
			if err != nil || prefs["主代理"] != "node1" {
				t.Fatalf("preference changed: %v, %v", prefs, err)
			}
		})
	}
}

func TestSourceHashesDetectSameTimestampAndMissing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(p, []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	a, err := sourceHashes([]string{p})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("two"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	b, err := sourceHashes([]string{p})
	if err != nil {
		t.Fatal(err)
	}
	if hashesEqual(a, b) {
		t.Fatal("content change was missed")
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceHashes([]string{p}); err == nil {
		t.Fatal("missing source was accepted")
	}
}

func TestGenScriptPathEnvOverride(t *testing.T) {
	want := filepath.Join(t.TempDir(), "gen_config.py")
	t.Setenv("CLASHHUB_GEN_CONFIG", want)
	if got := genScriptPath(); got != want {
		t.Fatalf("override ignored: %q", got)
	}
}

func TestEngineBinaryResolution(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if got := engineBinary(Config{EngineBin: exe}); got != exe {
		t.Fatalf("existing path not preferred: %q", got)
	}
	dir := t.TempDir()
	tool := filepath.Join(dir, "mihomo")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if got := engineBinary(Config{EngineBin: "/nonexistent/mihomo"}); got != tool {
		t.Fatalf("PATH fallback failed: %q", got)
	}
}

func TestGenScriptPathStaysInHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// The working directory contains a real gen_config.py; it must not be chosen.
	want := filepath.Join(home, "clashhub", "gen_config.py")
	if got := genScriptPath(); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
