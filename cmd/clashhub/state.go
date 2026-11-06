package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type ProxySetting struct {
	Enabled bool   `json:"enabled"`
	Server  string `json:"server"`
	Port    int    `json:"port"`
	Auth    bool   `json:"auth"`
}

type ServiceSnapshot struct {
	Name          string       `json:"name"`
	Web           ProxySetting `json:"web"`
	Secure        ProxySetting `json:"secure"`
	Socks         ProxySetting `json:"socks"`
	Bypass        []string     `json:"bypass"`
	PACURL        string       `json:"pac_url"`
	PACEnabled    bool         `json:"pac_enabled"`
	AutoDiscovery bool         `json:"auto_discovery"`
}

type State struct {
	Active          bool              `json:"active"`
	Ready           bool              `json:"ready"`
	PID             int               `json:"pid"`
	ProcessIdentity string            `json:"process_identity,omitempty"`
	EnginePID       int               `json:"engine_pid"`
	StartedAt       time.Time         `json:"started_at"`
	Services        []ServiceSnapshot `json:"services,omitempty"`
	AppliedHTTP     string            `json:"applied_http,omitempty"`
	AppliedSOCKS    string            `json:"applied_socks,omitempty"`

	SRWasConnected bool `json:"sr_was_connected"`
	SRStartedByUs  bool `json:"sr_started_by_us"`
	SRStoppedByUs  bool `json:"sr_stopped_by_us"`

	Mode             string    `json:"mode"`
	Upstream         string    `json:"upstream"`
	ProbeMS          int64     `json:"probe_ms"`
	AIMs             int64     `json:"ai_ms"`
	AICode           int       `json:"ai_code"`
	AIHealthy        bool      `json:"ai_healthy"`
	AIHealthKnown    bool      `json:"ai_health_known"`
	Egress           string    `json:"egress"`
	EgressOK         bool      `json:"egress_ok"`
	LastSwitch       time.Time `json:"last_switch"`
	LastReason       string    `json:"last_reason"`
	FallbackReason   string    `json:"fallback_reason"`
	FallbackFailedAt time.Time `json:"fallback_failed_at"`
	NextRetry        time.Time `json:"next_retry"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func readState() (*State, error) {
	b, err := os.ReadFile(statePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", statePath(), err)
	}
	return &st, nil
}

func writeState(st *State) error {
	st.UpdatedAt = time.Now()
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(statePath()), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(statePath()), ".state-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, statePath())
}

func processIdentity(pid int) string {
	if pid <= 0 {
		return ""
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=", "-o", "command=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func daemonMatches(st *State) bool {
	return st != nil && st.PID > 0 && st.ProcessIdentity != "" && pidAlive(st.PID) && processIdentity(st.PID) == st.ProcessIdentity
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
