package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

var engineStartupTimeout = 25 * time.Second

// engineBinary resolves the configured engine path without assuming Homebrew's
// Apple Silicon prefix: an existing file wins, then the same name on PATH (so a
// plain "mihomo" works), then "mihomo" itself as a last resort.
func engineBinary(cfg Config) string {
	if cfg.EngineBin != "" {
		if _, err := os.Stat(cfg.EngineBin); err == nil {
			return cfg.EngineBin
		}
		if p, err := exec.LookPath(filepath.Base(cfg.EngineBin)); err == nil {
			return p
		}
		return cfg.EngineBin
	}
	if p, err := exec.LookPath("mihomo"); err == nil {
		return p
	}
	return "mihomo"
}

func engineDataDir() string {
	return filepath.Join(appSupportDir(), "mihomo")
}

func engineConfigPath() string {
	return filepath.Join(engineDataDir(), "config.yaml")
}

func engineLogPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(appSupportDir(), "mihomo.log")
	}
	return filepath.Join(home, "Library", "Logs", "mihomo.log")
}

func runGenerator() error { return runGeneratorWithSources(nil) }

func runGeneratorWithSources(paths []string) error {
	return runGeneratorWithSourcesContext(context.Background(), paths)
}

func runGeneratorWithSourcesContext(ctx context.Context, paths []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	_, err = buildEngineConfig(ctx, cfg, paths)
	return err
}

func engineSecret() string {
	b, err := os.ReadFile(engineConfigPath())
	if err != nil {
		return ""
	}
	re := regexp.MustCompile(`(?m)^secret:\s*(\S+)\s*$`)
	if m := re.FindStringSubmatch(string(b)); len(m) == 2 {
		return strings.Trim(m[1], `"'`)
	}
	return ""
}

func (a *App) engineReachable() bool {
	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequest(http.MethodGet, "http://"+a.cfg.Controller+"/version", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+engineSecret())
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (a *App) enginePIDValue() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.enginePID
}

func (a *App) setEnginePID(pid int) {
	a.mu.Lock()
	a.enginePID = pid
	a.mu.Unlock()
	a.stateMu.Lock()
	a.state.EnginePID = pid
	a.stateMu.Unlock()
}

// ownedEngines is indexed by App because the lifecycle is confined to this file.
// Each entry is accessed under that App's engineMu; the map itself needs a
// separate mutex because different App instances may operate concurrently.
var ownedEngines = struct {
	sync.Mutex
	m map[*App]*ownedEngine
}{m: make(map[*App]*ownedEngine)}

type ownedEngine struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error // published by closing done
}

func (a *App) ownedEngineLocked() *ownedEngine {
	ownedEngines.Lock()
	defer ownedEngines.Unlock()
	return ownedEngines.m[a]
}

func (a *App) setOwnedEngineLocked(p *ownedEngine) {
	ownedEngines.Lock()
	if p == nil {
		delete(ownedEngines.m, a)
	} else {
		ownedEngines.m[a] = p
	}
	ownedEngines.Unlock()
}

func (a *App) forgetExitedEngineLocked(p *ownedEngine) {
	if a.ownedEngineLocked() == p {
		a.setOwnedEngineLocked(nil)
		a.setEnginePID(0)
	}
}

// stopOwnedEngineLocked only signals the process started by this App. Wait is
// always performed exactly once by the reaper, including after a forced kill.
func (a *App) stopOwnedEngineLocked(p *ownedEngine) {
	if p == nil {
		return
	}
	select {
	case <-p.done:
	default:
		if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil && err != os.ErrProcessDone {
			a.logf("engine termination: %v", err)
		}
		select {
		case <-p.done:
		case <-time.After(3 * time.Second):
			if err := p.cmd.Process.Kill(); err != nil && err != os.ErrProcessDone {
				a.logf("engine kill: %v", err)
			}
			<-p.done
		}
	}
	a.forgetExitedEngineLocked(p)
}

// restoreManualNodesVerifiedLocked confirms each requested selector change with
// a fresh controller snapshot. Unavailable preferences are retained unchanged.
func (a *App) restoreManualNodesVerifiedLocked() error {
	prefs, err := readManualNodes()
	if err != nil || len(prefs) == 0 {
		return err
	}
	p, err := a.selectionProber()
	if err != nil {
		return err
	}
	defer p.client.CloseIdleConnections()
	ctx := context.Background()
	snap, err := p.snapshot(ctx)
	if err != nil {
		return err
	}
	for _, group := range []string{"主代理", "AI主代理"} {
		node := prefs[group]
		if node == "" {
			continue
		}
		nodes, ok := selectableNodes(snap, group)
		if !ok || !containsNode(nodes, node) {
			a.logf("manual node unavailable for %s; preference retained", group)
			continue
		}
		if snap.Proxies[group].Now == node {
			continue
		}
		if err := p.selectNode(ctx, group, node); err != nil {
			return fmt.Errorf("restore selector %s: %w", group, err)
		}
		snap, err = p.snapshot(ctx)
		if err != nil {
			return fmt.Errorf("read back selector %s: %w", group, err)
		}
		status, ok := groupStatus(snap, map[string]string{group: node}, group)
		if !ok || status.Mode != "manual" {
			return fmt.Errorf("read back selector %s: manual node not active", group)
		}
	}
	return nil
}

func (a *App) startEngine() error {
	a.engineMu.Lock()
	defer a.engineMu.Unlock()
	return a.startEngineLocked()
}

func (a *App) startEngineLocked() error {
	a.advanceEngineGenerationLocked()
	a.invalidateAIHealth()
	_ = a.persist()
	if a.shuttingDown.Load() {
		return fmt.Errorf("engine start refused during shutdown")
	}
	if p := a.ownedEngineLocked(); p != nil {
		select {
		case <-p.done:
			a.forgetExitedEngineLocked(p)
		default:
			return fmt.Errorf("engine process already running")
		}
	}
	if portOpen(a.cfg.PrimaryAddr, 600*time.Millisecond) {
		return fmt.Errorf("port %s is already in use; process identity cannot be verified", a.cfg.PrimaryAddr)
	}
	if _, err := os.Stat(engineConfigPath()); err != nil {
		return fmt.Errorf("engine config missing: %w", err)
	}
	if err := os.MkdirAll(engineDataDir(), 0o755); err != nil {
		return err
	}
	lf, err := os.OpenFile(engineLogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	cmd := exec.Command(engineBinary(a.cfg), "-d", engineDataDir(), "-f", engineConfigPath())
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = lf.Close()
		return fmt.Errorf("start mihomo: %w", err)
	}
	pid := cmd.Process.Pid
	p := &ownedEngine{cmd: cmd, done: make(chan struct{})}
	a.setOwnedEngineLocked(p)
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	_ = lf.Close()
	a.setEnginePID(pid)
	a.logf("mihomo engine starting pid=%d", pid)

	deadline := time.Now().Add(engineStartupTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-p.done:
			a.forgetExitedEngineLocked(p)
			return fmt.Errorf("mihomo exited during startup (%v); see %s", p.err, engineLogPath())
		default:
		}
		if a.shuttingDown.Load() {
			a.stopOwnedEngineLocked(p)
			return fmt.Errorf("engine startup interrupted by shutdown")
		}
		if portOpen(a.cfg.PrimaryAddr, 500*time.Millisecond) && a.engineReachable() {
			select {
			case <-p.done:
				a.forgetExitedEngineLocked(p)
				return fmt.Errorf("mihomo exited during startup (%v); see %s", p.err, engineLogPath())
			default:
			}
			a.logf("mihomo engine ready pid=%d", pid)
			if err := a.restoreRoutingModeLocked(); err != nil {
				a.stopOwnedEngineLocked(p)
				return fmt.Errorf("restore routing mode: %w", err)
			}
			if err := a.restoreManualNodesVerifiedLocked(); err != nil {
				a.logf("restore manual node selection failed: %v", err)
			}
			a.persist()
			return nil
		}
		select {
		case <-p.done:
			a.forgetExitedEngineLocked(p)
			return fmt.Errorf("mihomo exited during startup (%v); see %s", p.err, engineLogPath())
		case <-time.After(400 * time.Millisecond):
		}
	}
	a.logf("mihomo startup timed out; stopping owned process pid=%d", pid)
	a.stopOwnedEngineLocked(p)
	return fmt.Errorf("mihomo did not become ready; see %s", engineLogPath())
}

func (a *App) stopEnginePID(pid int) {
	a.engineMu.Lock()
	defer a.engineMu.Unlock()
	p := a.ownedEngineLocked()
	if p != nil && (pid == 0 || pid == p.cmd.Process.Pid) {
		a.stopOwnedEngineLocked(p)
	} else if pid != 0 {
		a.logf("refusing to signal engine pid=%d: not an owned process", pid)
	}
}

func (a *App) reloadEngine() error {
	a.engineMu.Lock()
	defer a.engineMu.Unlock()
	a.advanceEngineGenerationLocked()
	a.invalidateAIHealth()
	_ = a.persist()

	if a.shuttingDown.Load() {
		return fmt.Errorf("engine reload refused during shutdown")
	}
	if a.engineReachable() {
		body, err := json.Marshal(map[string]string{"path": engineConfigPath()})
		if err != nil {
			return err
		}
		req, err := http.NewRequest(http.MethodPut, "http://"+a.cfg.Controller+"/configs?force=true", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+engineSecret())
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
		if err != nil {
			return fmt.Errorf("reload controller: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			return fmt.Errorf("reload controller returned %s", resp.Status)
		}
		if err := a.restoreRoutingModeLocked(); err != nil {
			return fmt.Errorf("restore routing mode: %w", err)
		}
		if err := a.restoreManualNodesVerifiedLocked(); err != nil {
			return fmt.Errorf("restore manual node selection: %w", err)
		}
		// A hot reload can strand tunnels bound to removed or replaced proxies.
		// Close them so clients reconnect against the fresh config.
		a.purgeEngineTunnels("engine config reloaded")
		return nil
	}
	if portOpen(a.cfg.PrimaryAddr, 500*time.Millisecond) {
		return fmt.Errorf("engine controller unavailable; refusing to replace running process")
	}
	if p := a.ownedEngineLocked(); p != nil {
		select {
		case <-p.done:
			a.forgetExitedEngineLocked(p)
		default:
			return fmt.Errorf("engine process still alive; refusing to replace it")
		}
	} else if a.enginePIDValue() != 0 {
		// A persisted PID does not establish ownership or liveness.
		a.setEnginePID(0)
	}
	return a.startEngineLocked()
}

func sourceHashes(paths []string) (map[string][32]byte, error) {
	out := make(map[string][32]byte, len(paths))
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read source %s: %w", p, err)
		}
		out[p] = sha256.Sum256(b)
	}
	return out, nil
}

func hashesEqual(a, b map[string][32]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// Runtime generation/reload uses the same outer gate as offline import and
// rollback. Never acquire this gate while holding engineMu.
func (a *App) watcherEngineUpdate(update func() error) error {
	return a.watcherEngineUpdateContext(func(context.Context) error { return update() })
}

func (a *App) watcherEngineUpdateContext(update func(context.Context) error) error {
	a.sourceRegenMu.Lock()
	defer a.sourceRegenMu.Unlock()
	if a.shuttingDown.Load() {
		return fmt.Errorf("engine watcher stopped during shutdown")
	}
	select {
	case <-a.stopCh:
		return fmt.Errorf("engine watcher stopped")
	default:
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-a.stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	return withSourceProcessLock(ctx, func() error { return update(ctx) })
}

func (a *App) engineWatcher() {
	aliveTicker := time.NewTicker(5 * time.Second)
	sourceTicker := time.NewTicker(60 * time.Second)
	defer aliveTicker.Stop()
	defer sourceTicker.Stop()

	last, _ := sourceHashes(a.cfg.SourceProfiles)
	for {
		select {
		case <-a.stopCh:
			return
		case <-aliveTicker.C:
			if !portOpen(a.cfg.PrimaryAddr, 500*time.Millisecond) {
				a.logf("engine port %s is down; restarting", a.cfg.PrimaryAddr)
				if err := a.watcherEngineUpdate(a.reloadEngine); err != nil {
					a.logf("engine restart failed: %v", err)
				}
			}
		case <-sourceTicker.C:
			cur, err := sourceHashes(a.cfg.SourceProfiles)
			if err != nil {
				a.logf("source profiles unavailable: %v", err)
				continue
			}
			if hashesEqual(cur, last) {
				continue
			}
			a.logf("source profiles changed; regenerating engine config")
			if err := a.watcherEngineUpdateContext(func(ctx context.Context) error {
				if err := runGeneratorWithSourcesContext(ctx, a.cfg.SourceProfiles); err != nil {
					return fmt.Errorf("regenerate: %w", err)
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				return a.reloadEngine()
			}); err != nil {
				a.logf("engine source update failed: %v", err)
				continue
			}
			last = cur
			a.stateMu.Lock()
			a.state.LastReason = "source config updated"
			a.stateMu.Unlock()
			a.persist()
		}
	}
}

// netDialOK is a small helper used by diagnostics.
func netDialOK(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 600*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}
