package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type Mode string

const (
	ModePrimary  Mode = "primary"
	ModeFallback Mode = "fallback"
	ModeProbing  Mode = "probing"
	ModeOff      Mode = "off"
)

type App struct {
	autoStart bool
	cfg       Config
	log       *log.Logger

	logFile   *os.File
	state     *State
	stateMu   sync.Mutex
	persistMu sync.Mutex

	mu                   sync.Mutex
	mode                 Mode
	probeEnd             time.Time
	primaryFails         int
	probeMS              int64
	aiMS                 int64
	aiCode               int
	aiHealthy            bool
	egress               string
	egressOK             bool
	lastEgress           time.Time
	egressRunning        bool
	recoveryDelay        time.Duration
	nextRecovery         time.Time
	lastSwitch           time.Time
	lastReason           string
	fallbackReason       string
	fallbackFailedAt     time.Time
	lastProbeOK          time.Time
	lastFallbackWatchdog time.Time
	fallbackEpoch        uint64

	transitionMu sync.Mutex
	// routingGate serializes mode commits with fallback attempts and probe commits.
	routingGate              sync.RWMutex
	routingGeneration        uint64
	routingAttempts          map[*routingAttempt]struct{}
	engineMu                 sync.Mutex
	sourceRegenMu            sync.Mutex
	engineGeneration         uint64
	selectionGeneration      uint64
	primaryAlternativeActive bool // guarded by engineMu; scanner must not undo an in-flight validation
	aiSelectionGeneration    uint64
	aiGeneration             uint64
	enginePID                int
	nodeScanWake             chan struct{}
	// Request failures only wake the existing node-health loop; they never
	// change selectors from the request path. Keep a generation-bound cooldown
	// so a broken node cannot create a scan per request.
	nodeScanFailureMu    sync.Mutex
	nodeScanFailureAt    time.Time
	nodeScanFailureToken probeToken

	fallbackMu             sync.RWMutex
	fallbackAddrs          []string
	fallbackActive         string
	fallbackTrafficMu      sync.Mutex
	fallbackTrafficBytes   uint64
	fallbackTrafficPending bool
	fallbackTrafficWorker  bool
	fallbackTrafficLast    time.Time

	applyMu sync.Mutex

	httpSrv *http.Server
	socksLn net.Listener
	stats   *statsIntegration
	capture captureState

	blockedMu      sync.RWMutex
	blockedDomains map[string]struct{}

	transportMu sync.Mutex
	transports  map[string]*http.Transport

	proxyWorkMu  sync.Mutex
	proxyWork    sync.WaitGroup
	proxyClosing bool
	proxySockets map[net.Conn]struct{}
	activeConns  int64

	// tunnelConns registers established relay tunnels by upstream label so a
	// failure-driven node switch can fast-fail the tunnels that are stranded
	// on the old node instead of leaving them black-holed.
	tunnelMu    sync.Mutex
	tunnelConns map[net.Conn]string

	// strategyMu guards the runtime node-selection strategy. It boots from
	// cfg and can be changed live via /api/strategy without a restart.
	strategyMu        sync.RWMutex
	primaryStrategy   string
	aiStrategy        string
	latencyGapMSValue int
	balanceGapMSValue int
	shuttingDown      atomic.Bool
	stopCh            chan struct{}
	stopOnce          sync.Once
	cleanupOnce       sync.Once
}

func newApp(cfg Config) (*App, error) {
	if err := os.MkdirAll(appSupportDir(), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(logPath()), 0o755); err != nil {
		return nil, err
	}
	rotateLog()
	f, err := os.OpenFile(logPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	st, err := readState()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if st == nil {
		st = &State{Active: true, StartedAt: time.Now()}
	}
	blocked, err := readBlockedDomains(blockedDomainPath())
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("load blocked domains: %w", err)
	}
	a := &App{
		blockedDomains: blocked,
		cfg:            cfg,
		log:            log.New(f, "", log.LstdFlags),
		logFile:        f,
		state:          st,
		mode:           ModeProbing,
		transports:     map[string]*http.Transport{},
		stopCh:         make(chan struct{}),
		nodeScanWake:   make(chan struct{}, 1),
	}
	a.probeEnd = time.Now().Add(30 * time.Second)
	a.setFallbackAddrs(a.cfg.FallbackAddrs)
	setAIRouteDomains(a.cfg.aiDomains())
	latencyGap := cfg.LatencyGapMS
	if latencyGap <= 0 {
		latencyGap = latencyGapMSDefault
	}
	balanceGap := cfg.BalanceGapMS
	if balanceGap <= 0 {
		balanceGap = balanceGapMSDefault
	}
	a.setStrategyValues(normalizeStrategy(cfg.PrimaryStrategy), normalizeStrategy(cfg.AIStrategy), latencyGap, balanceGap)
	go a.routeTrafficLoop()
	return a, nil
}

func rotateLog() {
	st, err := os.Stat(logPath())
	if err != nil || st.Size() < 5<<20 {
		return
	}
	_ = os.Remove(logPath() + ".1")
	_ = os.Rename(logPath(), logPath()+".1")
}

func (a *App) logf(format string, args ...any) {
	a.log.Printf(format, args...)
}

func (a *App) notify(msg string) {
	if !a.cfg.Notify {
		return
	}
	escaped := strings.ReplaceAll(msg, `"`, `\"`)
	cmd := exec.Command("osascript", "-e", fmt.Sprintf(`display notification "%s" with title "clashhub"`, escaped))
	go func() { _ = cmd.Run() }()
}

func (a *App) snapshotMode() (Mode, time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mode, a.probeEnd
}

// snapshotRoutingState captures the routing preference and all generations in
// the same lock order used by probes and selector commits. A caller can carry
// this token through an in-flight request and reject failure signals from an
// obsolete engine/selector generation.
func (a *App) snapshotRoutingState() (string, probeToken, error) {
	a.engineMu.Lock()
	defer a.engineMu.Unlock()
	a.routingGate.RLock()
	defer a.routingGate.RUnlock()
	mode, err := readRoutingMode()
	return mode, probeToken{a.routingGeneration, a.engineGeneration, a.selectionGeneration, a.aiSelectionGeneration}, err
}

const failureScanCooldown = 15 * time.Second

// scheduleAIFailureScan is deliberately a non-blocking health signal. It is
// safe to call for every failed setup/transport attempt: only AI destinations
// qualify, failures from an old generation are discarded, and a generation
// receives at most one wake during the cooldown window. The node-health loop
// performs the actual scan and retains its existing manual-node safeguards.
func (a *App) scheduleAIFailureScan(target string, requestToken probeToken) {
	if a.shuttingDown.Load() || a.nodeScanWake == nil || !isAIDomainCfg(a.cfg, hostOnly(target)) {
		return
	}
	a.scheduleFailureScan(requestToken)
}

// A routed primary probe can fail even when a previous per-node probe was
// green. Wake the existing scanner rather than waiting for its next tick; only
// the scanner may change selectors, and it retains its manual-mode safeguards.
func (a *App) scheduleFailureScan(requestToken probeToken) {
	if a.shuttingDown.Load() || a.nodeScanWake == nil {
		return
	}
	current, allowed := a.snapshotProbeToken()
	if !allowed || current != requestToken {
		return
	}
	now := time.Now()
	a.nodeScanFailureMu.Lock()
	if current == a.nodeScanFailureToken && !a.nodeScanFailureAt.IsZero() && now.Sub(a.nodeScanFailureAt) < failureScanCooldown {
		a.nodeScanFailureMu.Unlock()
		return
	}
	a.nodeScanFailureToken = current
	a.nodeScanFailureAt = now
	a.nodeScanFailureMu.Unlock()

	// Revalidate after taking the throttle lock. A selector reload or routing
	// mode change racing with the request must not enqueue an old-generation
	// recovery wake. The health cycle also snapshots the current generation
	// before it can publish any selector change.
	latest, allowed := a.snapshotProbeToken()
	if !allowed || latest != requestToken || a.shuttingDown.Load() {
		return
	}
	select {
	case a.nodeScanWake <- struct{}{}:
	default:
	}
}

func (a *App) upstreamForMode() string {
	switch a.mode {
	case ModePrimary:
		return a.cfg.PrimaryAddr
	case ModeFallback:
		return a.fallbackAddr()
	case ModeOff:
		return ""
	default:
		return "probing"
	}
}

func (a *App) setMode(m Mode, reason string) {
	now := time.Now()
	a.mu.Lock()
	if a.shuttingDown.Load() {
		a.mu.Unlock()
		return
	}
	changed := a.mode != m
	if changed {
		a.fallbackEpoch++
		a.invalidateAIHealthLocked()
	}
	a.mode = m
	a.lastReason = reason
	if changed {
		a.lastSwitch = now
		if m == ModeFallback {
			a.fallbackReason = reason
			a.fallbackFailedAt = now
			a.recoveryDelay = time.Duration(a.cfg.RecoveryInitSec) * time.Second
			a.nextRecovery = now.Add(a.recoveryDelay)
			a.primaryFails = 0
		} else {
			a.fallbackReason = ""
			a.fallbackFailedAt = time.Time{}
			a.nextRecovery = time.Time{}
		}
	}
	upstream := a.upstreamForMode()
	a.mu.Unlock()

	if changed {
		a.logf("mode -> %s (%s)", m, reason)
	}

	a.stateMu.Lock()
	a.state.Mode = string(m)
	a.state.Upstream = upstream
	a.state.LastReason = reason
	if changed {
		a.state.LastSwitch = now
	}
	a.stateMu.Unlock()

	a.persist()
	if changed {
		switch m {
		case ModePrimary:
			if reason == "manual selector confirmed; actual path unverified" || reason == "manual selection protected; actual path unverified" {
				a.notify("已手动切回主路，连通性尚未验证")
			} else {
				a.notify("日本节点引擎可用，已切回主路")
			}
		case ModeFallback:
			a.notify("日本节点引擎不通，已切到 33210")
		}
	}
}

func (a *App) persist() error {
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	a.stateMu.Lock()
	st := *a.state
	a.stateMu.Unlock()

	a.mu.Lock()
	st.Mode = string(a.mode)
	st.Upstream = a.upstreamForMode()
	st.ProbeMS = a.probeMS
	// A persisted boolean cannot verify out-of-band selector changes or expiry.
	// The node diagnostics endpoint provides per-node fresh dual-site results.
	st.AIMs = 0
	st.AICode = 0
	st.AIHealthy = false
	st.AIHealthKnown = false
	st.Egress = a.egress
	st.EgressOK = a.egressOK
	st.LastSwitch = a.lastSwitch
	st.LastReason = a.lastReason
	st.FallbackReason = a.fallbackReason
	st.FallbackFailedAt = a.fallbackFailedAt
	st.NextRetry = a.nextRecovery
	st.EnginePID = a.enginePID
	a.mu.Unlock()

	if err := writeState(&st); err != nil {
		a.logf("persist state: %v", err)
		return err
	}
	return nil
}

func (a *App) persistLoop() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-a.stopCh:
			return
		case <-t.C:
			a.persist()
		}
	}
}

func (a *App) startServers() error {
	if err := a.startHTTPProxy(); err != nil {
		return err
	}
	if err := a.startSOCKSProxy(); err != nil {
		return err
	}
	return nil
}

func (a *App) startupSequence() {
	generation, allowed := a.routingProbeToken()
	if !allowed {
		return
	}
	a.routingGate.RLock()
	if !a.routingProbeValidLocked(generation) {
		a.routingGate.RUnlock()
		return
	}
	a.setMode(ModeProbing, "starting")
	a.mu.Lock()
	a.probeEnd = time.Now().Add(20 * time.Second)
	a.mu.Unlock()
	a.routingGate.RUnlock()

	deadline := time.Now().Add(20 * time.Second)
	lastFailure := "engine not ready"
	for time.Now().Before(deadline) {
		if a.shuttingDown.Load() {
			return
		}
		if ok, reason := a.probePrimaryFull(); ok {
			a.routingGate.RLock()
			if a.shuttingDown.Load() || !a.routingProbeValidLocked(generation) {
				a.routingGate.RUnlock()
				return
			}
			a.logf("startup: engine ready (%s)", reason)
			a.setMode(ModePrimary, "startup: "+reason)
			a.routingGate.RUnlock()
			return
		} else {
			lastFailure = reason
			a.logf("startup: engine not ready: %s", reason)
		}
		select {
		case <-a.stopCh:
			return
		case <-time.After(1 * time.Second):
		}
	}
	a.enterFallbackIfMode("startup: "+lastFailure, ModeProbing)
}

func (a *App) applySystemProxy() error {
	a.applyMu.Lock()
	defer a.applyMu.Unlock()
	if a.shuttingDown.Load() {
		return fmt.Errorf("proxy application interrupted by shutdown")
	}
	a.stateMu.Lock()
	services := append([]ServiceSnapshot(nil), a.state.Services...)
	a.stateMu.Unlock()
	if len(services) == 0 {
		var err error
		services, err = snapshotAllServices()
		if err != nil {
			return err
		}
		a.stateMu.Lock()
		a.state.Services = services
		a.stateMu.Unlock()
	}
	if a.autoStart {
		if err := autoProxyAllowed(services, a.cfg); err != nil {
			return fmt.Errorf("automatic startup snapshot rejected: %w", err)
		}
	}
	a.stateMu.Lock()
	a.state.AppliedHTTP, a.state.AppliedSOCKS = a.cfg.HTTPListen, a.cfg.SOCKSListen
	a.stateMu.Unlock()
	if err := a.persist(); err != nil {
		return err
	}
	if err := applyOurProxy(services, a.cfg.HTTPListen, a.cfg.SOCKSListen); err != nil {
		return err
	}
	a.logf("system proxy -> %s (http) / %s (socks)", a.cfg.HTTPListen, a.cfg.SOCKSListen)
	return nil
}

func (a *App) healthLoop() {
	probeTicker := time.NewTicker(time.Duration(a.cfg.ProbeIntervalSec) * time.Second)
	tick := time.NewTicker(1 * time.Second)
	defer probeTicker.Stop()
	defer tick.Stop()

	for {
		select {
		case <-a.stopCh:
			return
		case <-probeTicker.C:
			if a.directRouting() {
				continue
			}
			mode, _ := a.snapshotMode()
			if mode == ModePrimary {
				a.healthProbe()
			}
		case <-tick.C:
			if a.directRouting() {
				continue
			}
			mode, probeEnd := a.snapshotMode()
			if mode == ModeProbing && time.Now().After(probeEnd) {
				go a.enterFallbackIfMode("probe timeout", ModeProbing)
			}
			// Fallback watchdog: while routed to the external fallback, a dead
			// fallback blackholes traffic until the next scheduled recovery.
			// Detect that within seconds and force an immediate attempt.
			if mode == ModeFallback && time.Now().After(a.lastFallbackWatchdog.Add(10*time.Second)) {
				a.mu.Lock()
				a.lastFallbackWatchdog = time.Now()
				a.mu.Unlock()
				go func() {
					if !a.fallbackUsable() {
						a.logf("fallback watchdog: external fallback is not carrying traffic; forcing recovery")
						a.pullRecoveryEarlier()
						a.mu.Lock()
						a.nextRecovery = time.Now()
						a.mu.Unlock()
						go a.attemptRecovery()
					}
				}()
			}
			if mode == ModeFallback {
				a.mu.Lock()
				next := a.nextRecovery
				a.mu.Unlock()
				if !next.IsZero() && time.Now().After(next) {
					go a.attemptRecovery()
				}
			}
		}
	}
}

// probePrimaryFull answers whether the routed path can carry real traffic.
// Several independent ordinary targets are tried in turn: Japanese nodes differ
// in which sites they can reach, so one blocked site (for example gstatic on a
// node that only allows Google) must not declare the whole route down and push
// every request onto the external fallback.
func (a *App) probePrimaryFull() (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return a.probePrimaryFullContext(ctx)
}

// A second, independent HTTP endpoint is only a reachability confirmation; it
// does not stand in for AI checks or for the configured Japan egress policy.
func (a *App) probePrimaryTarget(target string, expected int, label string) (bool, string) {
	timeout := time.Duration(a.cfg.ProbeTimeoutSec) * time.Second
	return a.probePrimaryTargetTimeout(target, expected, label, timeout)
}

func (a *App) probePrimaryTargetTimeout(target string, expected int, label string, timeout time.Duration) (bool, string) {
	token, allowed := a.snapshotProbeToken()
	if !allowed {
		return false, "routing mode changed"
	}

	start := time.Now()
	_, gMs, gErr := probeStatus(a.cfg.PrimaryAddr, target, expected, timeout)
	a.stats.health(label, start, gErr)
	a.engineMu.Lock()
	a.routingGate.RLock()
	if !a.probeTokenValidLocked(token) {
		a.routingGate.RUnlock()
		a.engineMu.Unlock()
		return false, "routing mode changed"
	}
	if label == "gstatic" {
		a.mu.Lock()
		a.probeMS = gMs
		a.mu.Unlock()
	}
	a.routingGate.RUnlock()
	a.engineMu.Unlock()
	// AI health is published by the node scanner only after both OpenAI and
	// ChatGPT probes and selector verification. A routed request may use the
	// external fallback and cannot establish the selected node's health.

	if gErr != nil {
		return false, target + ": " + gErr.Error()
	}

	return true, fmt.Sprintf("%s %dms", label, gMs)
}

func (a *App) routingProbeToken() (uint64, bool) {
	a.routingGate.RLock()
	defer a.routingGate.RUnlock()
	mode, err := readRoutingMode()
	return a.routingGeneration, err == nil && mode != "direct"
}

func (a *App) routingProbeValidLocked(generation uint64) bool {
	mode, err := readRoutingMode()
	return err == nil && mode != "direct" && generation == a.routingGeneration
}

// probeToken snapshots both generations in the same lock order as route changes.
type probeToken struct {
	routing     uint64
	engine      uint64
	selection   uint64
	aiSelection uint64
}

func (a *App) snapshotProbeToken() (probeToken, bool) {
	a.engineMu.Lock()
	defer a.engineMu.Unlock()
	a.routingGate.RLock()
	defer a.routingGate.RUnlock()
	return probeToken{a.routingGeneration, a.engineGeneration, a.selectionGeneration, a.aiSelectionGeneration}, a.routingProbeValidLocked(a.routingGeneration)
}

// The caller holds engineMu and routingGate.
func (a *App) probeTokenValidLocked(t probeToken) bool {
	return t.selection == a.selectionGeneration && t.engine == a.engineGeneration && a.routingProbeValidLocked(t.routing)
}

// manualFallbackProtectionLocked resolves only confirmed, available manual pins.
// engineMu must be held. An unreadable preference/controller fails closed: a
// transient error must not silently divert a user's pinned traffic to fallback.
func (a *App) manualFallbackProtectionLocked(ctx context.Context, target string) bool {
	prefs, err := readManualNodes()
	if err != nil {
		return true
	}
	mode, err := readRoutingMode()
	if err != nil {
		return true
	}
	if mode == "direct" {
		return false
	}
	groups := []string{"主代理"}
	if mode == "rule" {
		// Neither the global outer mode nor a request hostname proves which
		// mihomo rule/selector will be used. Protect either active pin rather
		// than bypassing it based on an incomplete AI-domain heuristic.
		groups = append(groups, "AI主代理")
	}
	needed := false
	for _, group := range groups {
		needed = needed || prefs[group] != ""
	}
	if !needed {
		return false
	}
	p, err := a.selectionProber()
	if err != nil {
		return true
	}
	defer p.client.CloseIdleConnections()
	actualMode, err := controllerRoutingMode(ctx, p)
	if err != nil || actualMode != mode {
		return true
	}
	snap, err := p.snapshot(ctx)
	if err != nil {
		return true
	}
	if mode == "global" && snap.Proxies["GLOBAL"].Now != "主代理" {
		return false
	}
	for _, group := range groups {
		nodes, ok := selectableNodes(snap, group)
		if ok && prefs[group] != "" && containsNode(nodes, prefs[group]) && snap.Proxies[group].Now == prefs[group] {
			return true
		}
	}
	return false
}

// allowFallbackRetry checks the request generation again after the controller
// read. A concurrent manual PUT advances it before mutating any selector.
func (a *App) allowFallbackRetry(ctx context.Context, target string, token probeToken) bool {
	a.engineMu.Lock()
	defer a.engineMu.Unlock()
	a.routingGate.RLock()
	// Request routing rules can choose the AI selector even for a hostname we
	// cannot classify. Health probes intentionally use the normal-only token.
	valid := a.probeTokenValidLocked(token) && token.aiSelection == a.aiSelectionGeneration
	a.routingGate.RUnlock()
	return valid && !a.manualFallbackProtectionLocked(ctx, target)
}

func (a *App) healthProbe() {
	token, allowed := a.snapshotProbeToken()
	if !allowed {
		return
	}
	ok, reason := a.probePrimaryFull()
	if !ok {
		// Begin selecting freshly proven alternatives while the next probe runs.
		// The generation token prevents a stale probe from waking a later route.
		a.scheduleFailureScan(token)
	}
	a.transitionMu.Lock()
	defer a.transitionMu.Unlock()
	a.engineMu.Lock()
	a.routingGate.RLock()
	if !a.probeTokenValidLocked(token) {
		a.routingGate.RUnlock()
		a.engineMu.Unlock()
		return
	}
	a.mu.Lock()
	if a.mode != ModePrimary {
		a.mu.Unlock()
		a.routingGate.RUnlock()
		a.engineMu.Unlock()
		return
	}
	a.mu.Unlock()
	if !ok {
		a.mu.Lock()
		a.primaryFails++
		fails := a.primaryFails
		a.mu.Unlock()
		a.logf("engine probe failed (%d): %s", fails, reason)
		a.routingGate.RUnlock()
		a.engineMu.Unlock()
		if fails >= 2 {
			// The fast path only considers previously confirmed, fresh nodes. It
			// must prove the actual routed primary before staying on it.
			var alternative string
			token, alternative = a.tryFreshAlternative(token)
			recovered := alternative != ""
			fallbackOK := false
			if !recovered {
				fallbackOK = a.fallbackUsable()
			}
			a.engineMu.Lock()
			defer a.engineMu.Unlock()
			a.routingGate.RLock()
			defer a.routingGate.RUnlock()
			if !a.probeTokenValidLocked(token) || a.shuttingDown.Load() {
				return
			}
			if recovered {
				a.mu.Lock()
				a.primaryFails = 0
				a.lastProbeOK = time.Now()
				a.mu.Unlock()
				a.persist()
				return
			}
			mode, _ := a.snapshotMode()
			if mode == ModePrimary && !a.shuttingDown.Load() {
				// Never hand traffic to a fallback that is not itself usable:
				// that converts a partial outage into a total one. Stay on the
				// engine (its selector keeps trying other nodes) unless the
				// fallback answers a real request right now.
				if fallbackOK && !a.manualFallbackProtectionLocked(context.Background(), "") {
					a.setMode(ModeFallback, reason)
				} else {
					why := "fallback unavailable"
					if fallbackOK {
						why = "manual selection protected"
					}
					a.mu.Lock()
					a.fallbackReason = why + "; staying on engine: " + reason
					a.fallbackFailedAt = time.Now()
					a.mu.Unlock()
					a.logf("%s; staying on engine despite probe failure: %s", why, reason)
					a.persist()
				}
			}
		}
		return
	}
	a.mu.Lock()
	a.primaryFails = 0
	a.lastProbeOK = time.Now()
	a.mu.Unlock()
	a.routingGate.RUnlock()
	a.engineMu.Unlock()
	a.persist()
}

// fallbackUsable answers whether the fallback upstream can carry a real
// request right now. Port-open checks are not enough: a dead upstream may
// still accept the TCP connection and then fail the request.
const independentProbeURL = "https://www.cloudflare.com/cdn-cgi/trace"

func (a *App) fallbackUsable() bool {
	return a.fallbackUsableTarget(independentProbeURL)
}

// The second target proves reachability only to that target; it does not
// assert gstatic, AI, or geographic health. RequireJapan continues to gate
// primary validation separately and is never relaxed by fallback evidence.
func (a *App) fallbackUsableTarget(target string) bool {
	timeout := time.Duration(a.cfg.ProbeTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	for _, addr := range a.fallbackList() {
		if a.fallbackReachable(addr, target, timeout) {
			a.fallbackMu.Lock()
			a.fallbackActive = addr
			a.fallbackMu.Unlock()
			return true
		}
	}
	return false
}

func (a *App) fallbackReachable(addr, target string, timeout time.Duration) bool {
	if _, _, err := probeStatus(addr, a.cfg.ProbeURL, probeExpectedCode(a.cfg.ProbeURL), timeout); err == nil {
		return true
	}
	if target == a.cfg.ProbeURL {
		return false
	}
	_, _, err := probeStatus(addr, target, 200, timeout)
	return err == nil
}

// confirmEngineFailure checks a distinct non-AI endpoint through the same
// route. Success is real endpoint evidence, not a claim that gstatic works.
func (a *App) confirmEngineFailure() (bool, string) {
	return a.confirmEngineFailureTarget(independentProbeURL)
}

func (a *App) confirmEngineFailureTarget(target string) (bool, string) {
	select {
	case <-a.stopCh:
		return false, ""
	case <-time.After(1200 * time.Millisecond):
	}
	if a.cfg.ProbeURL == target {
		return false, "no independent confirmation endpoint"
	}
	return a.probePrimaryTarget(target, 200, "cloudflare")
}

func (a *App) egressCheck() {
	generation, allowed := a.routingProbeToken()
	if !allowed {
		return
	}
	a.mu.Lock()
	if a.egressRunning {
		a.mu.Unlock()
		return
	}
	a.egressRunning = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.egressRunning = false
		a.mu.Unlock()
	}()

	ip, loc, err := egressThrough(a.cfg.PrimaryAddr, 8*time.Second)
	if err != nil {
		a.logf("egress check failed: %v", err)
		return
	}
	a.routingGate.RLock()
	defer a.routingGate.RUnlock()
	if !a.routingProbeValidLocked(generation) {
		return
	}
	val := strings.TrimSpace(strings.Join([]string{loc, ip}, " "))
	a.mu.Lock()
	a.egress = val
	a.lastEgress = time.Now()
	a.mu.Unlock()
	if loc != "" && !strings.EqualFold(loc, "JP") {
		a.logf("warning: Shadowrocket egress is %s, not JP (%s)", loc, ip)
	} else {
		a.logf("egress via Shadowrocket: %s", val)
	}
	a.persist()
}

func (a *App) enterFallback(reason string) {
	a.enterFallbackIfMode(reason, "")
}

func (a *App) enterFallbackIfMode(reason string, expected Mode) {
	a.transitionMu.Lock()
	defer a.transitionMu.Unlock()

	token, allowed := a.snapshotProbeToken()
	if !allowed {
		return
	}
	mode, _ := a.snapshotMode()
	if mode == ModeFallback || mode == ModeOff || (expected != "" && mode != expected) || a.shuttingDown.Load() {
		return
	}
	// A fresh candidate must prove the actual path before startup diverts to backup.
	recovered := false
	if mode == ModeProbing {
		var candidate string
		token, candidate = a.tryFreshAlternative(token)
		recovered = candidate != ""
	}
	// Do not strand requests on a dead fallback after startup probe expiry.
	// Probe outside routingGate so direct-mode changes remain responsive.
	usable := true
	if mode == ModeProbing && !recovered {
		usable = a.fallbackUsable()
	}
	a.engineMu.Lock()
	defer a.engineMu.Unlock()
	a.routingGate.RLock()
	defer a.routingGate.RUnlock()
	if !a.probeTokenValidLocked(token) {
		return
	}
	if mode, err := readRoutingMode(); err != nil || mode == "direct" {
		return
	}
	mode, _ = a.snapshotMode()
	if mode == ModeFallback || mode == ModeOff || (expected != "" && mode != expected) || a.shuttingDown.Load() {
		return
	}
	if recovered {
		a.setMode(ModePrimary, "primary alternative verified")
		return
	}
	if a.manualFallbackProtectionLocked(context.Background(), "") {
		a.setMode(ModePrimary, "manual selection protected; actual path unverified")
		return
	}
	if !usable {
		a.setMode(ModePrimary, "fallback unavailable; staying on engine: "+reason)
		return
	}
	a.logf("entering fallback: %s", reason)
	a.setMode(ModeFallback, reason)
}

const fallbackTrafficThreshold uint64 = 1 << 30
const fallbackTrafficMinInterval = time.Minute

// Account only independently attributable fallback payload. The counter is
// process-local: a restart discards the remainder rather than replaying data.
func (a *App) recordFallbackTraffic(n uint64) {
	if n == 0 || a.shuttingDown.Load() {
		return
	}
	a.fallbackTrafficMu.Lock()
	total := statsSaturatingAdd(a.fallbackTrafficBytes, n)
	if total >= fallbackTrafficThreshold {
		a.fallbackTrafficPending = true
		total %= fallbackTrafficThreshold
	}
	a.fallbackTrafficBytes = total
	if a.fallbackTrafficPending && !a.fallbackTrafficWorker {
		a.fallbackTrafficWorker = true
		go a.runFallbackTrafficWake()
	}
	a.fallbackTrafficMu.Unlock()
}

func (a *App) runFallbackTrafficWake() {
	for {
		a.fallbackTrafficMu.Lock()
		if !a.fallbackTrafficPending || a.shuttingDown.Load() {
			a.fallbackTrafficWorker = false
			a.fallbackTrafficMu.Unlock()
			return
		}
		delay := time.Until(a.fallbackTrafficLast.Add(fallbackTrafficMinInterval))
		if delay <= 0 {
			a.fallbackTrafficPending = false
			a.fallbackTrafficLast = time.Now()
			a.fallbackTrafficMu.Unlock()
			select {
			case a.nodeScanWake <- struct{}{}:
			default:
			}
			// The regular recovery path retains its cooldown and two-success gate.
			a.routingGate.RLock()
			allowed := a.routingProbeValidLocked(a.routingGeneration)
			a.mu.Lock()
			if allowed && a.mode == ModeFallback {
				a.nextRecovery = time.Now()
			}
			a.mu.Unlock()
			a.routingGate.RUnlock()
			if allowed {
				go a.attemptRecovery()
			}
			continue
		}
		a.fallbackTrafficMu.Unlock()
		t := time.NewTimer(delay)
		select {
		case <-a.stopCh:
			if !t.Stop() {
				<-t.C
			}
			a.fallbackTrafficMu.Lock()
			a.fallbackTrafficWorker = false
			a.fallbackTrafficMu.Unlock()
			return
		case <-t.C:
		}
	}
}

// recoverySuccessThreshold requires consecutive successful primary probes.
const recoverySuccessThreshold = 2

func (a *App) attemptRecovery() {
	// Avoid queuing duplicate probes while a recovery attempt is running.
	if !a.transitionMu.TryLock() {
		return
	}
	defer a.transitionMu.Unlock()
	token, allowed := a.snapshotProbeToken()
	if !allowed {
		return
	}

	a.routingGate.RLock()
	if !a.routingProbeValidLocked(token.routing) {
		a.routingGate.RUnlock()
		return
	}
	a.mu.Lock()
	now := time.Now()
	cooldownEnd := a.lastSwitch.Add(time.Duration(a.cfg.RecoveryInitSec) * time.Second)
	if a.mode != ModeFallback || a.shuttingDown.Load() || a.nextRecovery.IsZero() || now.Before(a.nextRecovery) {
		a.mu.Unlock()
		a.routingGate.RUnlock()
		return
	}
	if now.Before(cooldownEnd) {
		a.nextRecovery = cooldownEnd
		a.mu.Unlock()
		a.persist()
		a.routingGate.RUnlock()
		return
	}
	a.mu.Unlock()
	a.routingGate.RUnlock()

	// Keep routing through fallback while testing primary. Only consecutive
	// successful probes qualify for a switch back.
	a.logf("probing engine again")
	// Probe the engine's existing selection without changing its selectors.
	var reason string
	ok := false
	for successes := 0; successes < recoverySuccessThreshold; successes++ {
		if a.shuttingDown.Load() {
			return
		}
		ok, reason = a.probePrimaryFull()
		if !ok {
			// One flaky probe (a node mid-failover, a transient TLS EOF) must
			// not discard the whole recovery chain; confirm once before
			// treating the engine as still down.
			if ok2, reason2 := a.confirmEngineFailure(); ok2 {
				ok, reason = true, reason2
			} else {
				break
			}
		}
		if successes+1 < recoverySuccessThreshold {
			select {
			case <-a.stopCh:
				return
			case <-time.After(1500 * time.Millisecond):
			}
		}
	}
	if !ok {
		a.scheduleFailureScan(token)
		// While keeping traffic on the verified backup, try one fresh eligible
		// alternative and independently validate the routed primary twice.
		var alternative string
		token, alternative = a.tryFreshAlternative(token)
		if alternative != "" {
			select {
			case <-a.stopCh:
				return
			case <-time.After(1500 * time.Millisecond):
			}
			ok, reason = a.probePrimaryFull()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			ok = ok && a.alternativeStillSelected(ctx, token, alternative)
			cancel()
		}
	}
	a.engineMu.Lock()
	defer a.engineMu.Unlock()
	a.routingGate.RLock()
	defer a.routingGate.RUnlock()
	if a.shuttingDown.Load() || !a.probeTokenValidLocked(token) {
		return
	}
	if ok {
		a.mu.Lock()
		a.recoveryDelay = time.Duration(a.cfg.RecoveryInitSec) * time.Second
		a.nextRecovery = time.Time{}
		a.primaryFails = 0
		a.lastProbeOK = time.Now()
		a.mu.Unlock()
		a.setMode(ModePrimary, "recovered: "+reason)
		a.logf("recovered: back on engine")
		return
	}

	a.mu.Lock()
	maxDelay := time.Duration(a.cfg.RecoveryMaxSec) * time.Second
	if a.recoveryDelay >= maxDelay || a.recoveryDelay > maxDelay/2 {
		a.recoveryDelay = maxDelay
	} else {
		a.recoveryDelay *= 2
	}
	if a.recoveryDelay < time.Duration(a.cfg.RecoveryInitSec)*time.Second {
		a.recoveryDelay = time.Duration(a.cfg.RecoveryInitSec) * time.Second
	}
	next := a.recoveryDelay
	a.fallbackReason = "recovery failed: " + reason
	a.fallbackFailedAt = time.Now()
	a.nextRecovery = a.fallbackFailedAt.Add(next)
	a.mu.Unlock()
	a.setMode(ModeFallback, "recovery failed: "+reason)
	a.logf("engine still down: %s; next retry in %s", reason, next)
}

func (a *App) forceRetry() {
	go func() {
		mode, _ := a.snapshotMode()
		if mode == ModeFallback {
			a.routingGate.RLock()
			if !a.routingProbeValidLocked(a.routingGeneration) {
				a.routingGate.RUnlock()
				return
			}
			a.mu.Lock()
			if a.mode != ModeFallback {
				a.mu.Unlock()
				a.routingGate.RUnlock()
				return
			}
			a.nextRecovery = time.Now()
			a.mu.Unlock()
			a.routingGate.RUnlock()
			a.attemptRecovery()
			return
		}
		if mode == ModePrimary {
			a.healthProbe()
		}
	}()
}

func (a *App) invalidateAIHealthLocked() {
	a.aiGeneration++
	a.aiHealthy = false
	a.aiMS = 0
	a.aiCode = 0
}

func (a *App) invalidateAIHealth() {
	a.mu.Lock()
	a.invalidateAIHealthLocked()
	a.mu.Unlock()
}

func (a *App) aiHealthyValue() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.aiHealthy
}

// isAIDomainCfg checks the host against the configured AI providers' domains.
func isAIDomainCfg(cfg Config, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, d := range cfg.aiDomains() {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

func hostOnly(target string) string {
	if h, _, err := net.SplitHostPort(target); err == nil {
		return h
	}
	return target
}

func (a *App) fallbackDial(ctx context.Context, target string, generation uint64) (net.Conn, string, error) {
	var firstErr error
	tried := map[string]bool{}
	for _, addr := range a.fallbackList() {
		if tried[addr] || ctx.Err() != nil {
			continue
		}
		tried[addr] = true
		c, label, err := a.routingProxyDial(ctx, addr, target, generation, 8*time.Second)
		if err == nil {
			a.fallbackMu.Lock()
			a.fallbackActive = addr
			a.fallbackMu.Unlock()
			return c, label, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr == nil {
		firstErr = context.Canceled
	}
	return nil, a.fallbackAddr(), firstErr
}

// pullRecoveryEarlier bounds how long a dead fallback delays the next recovery
// probe; the regular backoff still applies afterwards.
func (a *App) pullRecoveryEarlier() {
	a.routingGate.RLock()
	defer a.routingGate.RUnlock()
	if !a.routingProbeValidLocked(a.routingGeneration) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.mode != ModeFallback || a.nextRecovery.IsZero() {
		return
	}
	if t := time.Now().Add(10 * time.Second); t.Before(a.nextRecovery) {
		a.nextRecovery = t
	}
}

func (a *App) dialForTarget(ctx context.Context, target string) (net.Conn, string, error) {
	for {
		routingMode, requestToken, routingErr := a.snapshotRoutingState()
		generation := requestToken.routing
		if routingErr != nil {
			return nil, "", fmt.Errorf("routing preference unavailable: %w", routingErr)
		}
		if routingMode == "direct" {
			c, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", target)
			return c, "direct", err
		}
		mode, probeEnd := a.snapshotMode()
		if mode == ModeProbing {
			if time.Now().Before(probeEnd) {
				select {
				case <-ctx.Done():
					return nil, "", ctx.Err()
				case <-time.After(150 * time.Millisecond):
				}
				continue
			}
			return nil, "", fmt.Errorf("routing not ready")
		}

		if mode == ModeFallback {
			c, label, err := a.fallbackDial(ctx, target, generation)
			if err == nil {
				return c, label, nil
			}
			if ctx.Err() != nil {
				return nil, "", ctx.Err()
			}
			a.scheduleFailureScan(requestToken)
			c, label, err = a.recoveryPrimaryDial(ctx, target, generation)
			if err != nil && ctx.Err() == nil {
				a.scheduleFailureScan(requestToken)
			}
			return c, label, err
		}

		// A protected manual selector must not be bypassed on retry.
		primaryTimeout := time.Duration(a.cfg.PrimaryTimeoutMS) * time.Millisecond
		c, _, err := a.routingProxyDial(ctx, a.cfg.PrimaryAddr, target, generation, primaryTimeout)
		if err == nil {
			return c, a.cfg.PrimaryAddr, nil
		}
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		a.scheduleFailureScan(requestToken)
		if !a.allowFallbackRetry(ctx, target, requestToken) {
			return nil, a.cfg.PrimaryAddr, fmt.Errorf("primary failed; fallback suppressed for manual selection or changed route: %w", err)
		}
		a.logf("engine CONNECT %s failed: %v; retrying via 33210", target, err)
		c2, label, err2 := a.fallbackDial(ctx, target, generation)
		if err2 == nil {
			return c2, label, nil
		}
		if ctx.Err() == nil {
			a.scheduleFailureScan(requestToken)
		}
		return nil, a.cfg.FallbackAddr, fmt.Errorf("7890: %v; 33210: %v", err, err2)
	}
}

func (a *App) dialStats(proxy, target string, timeout time.Duration) (net.Conn, error) {
	start := time.Now()
	c, err := dialViaHTTPProxy(proxy, target, timeout)
	a.stats.connection(target, start, err)
	return c, err
}

func (a *App) transportFor(upstream string) *http.Transport {
	a.transportMu.Lock()
	defer a.transportMu.Unlock()
	if t, ok := a.transports[upstream]; ok {
		return t
	}
	var proxy func(*http.Request) (*url.URL, error)
	if upstream != "direct" {
		proxy = http.ProxyURL(&url.URL{Scheme: "http", Host: upstream})
	}
	t := &http.Transport{
		Proxy: proxy,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			start := time.Now()
			conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, addr)
			if a.stats != nil {
				a.stats.store.RecordRecentConnection(time.Since(start))
			}
			return conn, err
		},
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
	}
	a.transports[upstream] = t
	return t
}

func (a *App) fallbackTrafficCallback(upstream string) func(uint64) {
	if upstream != "direct" && upstream != a.cfg.PrimaryAddr && containsString(a.fallbackList(), upstream) {
		return a.recordFallbackTraffic
	}
	return nil
}

// beginProxyWork serializes admission with shutdown; Wait never races Add.
func (a *App) beginProxyWork() bool {
	a.proxyWorkMu.Lock()
	defer a.proxyWorkMu.Unlock()
	if a.proxyClosing {
		return false
	}
	a.proxyWork.Add(1)
	return true
}
func (a *App) trackProxySockets(conns ...net.Conn) bool {
	a.proxyWorkMu.Lock()
	defer a.proxyWorkMu.Unlock()
	if a.proxyClosing {
		for _, c := range conns {
			_ = c.Close()
		}
		return false
	}
	if a.proxySockets == nil {
		a.proxySockets = make(map[net.Conn]struct{})
	}
	for _, c := range conns {
		a.proxySockets[c] = struct{}{}
	}
	return true
}
func (a *App) untrackProxySockets(conns ...net.Conn) {
	a.proxyWorkMu.Lock()
	defer a.proxyWorkMu.Unlock()
	for _, c := range conns {
		delete(a.proxySockets, c)
	}
}
func (a *App) stopProxyWork() {
	a.proxyWorkMu.Lock()
	a.proxyClosing = true
	for c := range a.proxySockets {
		_ = c.Close()
	}
	a.proxyWorkMu.Unlock()
	a.proxyWork.Wait()
}

func (a *App) trackTunnel(label string, conns ...net.Conn) {
	a.tunnelMu.Lock()
	defer a.tunnelMu.Unlock()
	if a.tunnelConns == nil {
		a.tunnelConns = make(map[net.Conn]string)
	}
	for _, c := range conns {
		a.tunnelConns[c] = label
	}
}

func (a *App) untrackTunnel(conns ...net.Conn) {
	a.tunnelMu.Lock()
	defer a.tunnelMu.Unlock()
	for _, c := range conns {
		delete(a.tunnelConns, c)
	}
}

// purgeTunnelsByUpstream fast-fails established tunnels dialed through the
// given upstream. A node switch strands tunnels that mihomo already bound to
// the old node; closing them turns a silent black hole into an immediate
// client-visible error so clients reconnect through the new node.
func (a *App) purgeTunnelsByUpstream(label, reason string) int {
	a.tunnelMu.Lock()
	var victims []net.Conn
	for c, l := range a.tunnelConns {
		if l == label {
			victims = append(victims, c)
			delete(a.tunnelConns, c)
		}
	}
	a.tunnelMu.Unlock()
	if len(victims) == 0 {
		return 0
	}
	for _, c := range victims {
		_ = c.Close()
	}
	a.logf("fast-fail: closed %d tunnel(s) via %s (%s)", len(victims), label, reason)
	return len(victims)
}

// purgeEngineTunnels kills tunnels riding clashhub's own mihomo engine.
func (a *App) purgeEngineTunnels(reason string) {
	a.purgeTunnelsByUpstream(a.cfg.PrimaryAddr, reason)
}

// strategyForKind maps a sweep route kind to its configured selection
// strategy. A nil App (isolated probers in tests) keeps legacy behavior.
func (a *App) strategyForKind(kind string) string {
	if a == nil {
		return ""
	}
	a.strategyMu.RLock()
	defer a.strategyMu.RUnlock()
	if kind == "ai" {
		return a.aiStrategy
	}
	return a.primaryStrategy
}

// latencyGapMS returns the configured latency-mode gap in milliseconds.
func (a *App) latencyGapMS() int {
	if a == nil {
		return latencyGapMSDefault
	}
	a.strategyMu.RLock()
	defer a.strategyMu.RUnlock()
	if a.latencyGapMSValue <= 0 {
		return latencyGapMSDefault
	}
	return a.latencyGapMSValue
}

// balanceGapMS returns the configured balanced-mode gap in milliseconds.
func (a *App) balanceGapMS() int {
	if a == nil {
		return balanceGapMSDefault
	}
	a.strategyMu.RLock()
	defer a.strategyMu.RUnlock()
	if a.balanceGapMSValue <= 0 {
		return balanceGapMSDefault
	}
	return a.balanceGapMSValue
}

// strategySnapshot returns the current strategy values for display.
func (a *App) strategySnapshot() (primary, ai string, latencyGap, balanceGap int) {
	a.strategyMu.RLock()
	defer a.strategyMu.RUnlock()
	return a.primaryStrategy, a.aiStrategy, a.latencyGapMSValue, a.balanceGapMSValue
}

// setStrategyValues installs new live strategy values. Empty strategies fall
// back to stability so a bad API payload can never disable selection.
func (a *App) setStrategyValues(primary, ai string, latencyGap, balanceGap int) {
	if normalized := normalizeStrategy(primary); normalized != "" {
		primary = normalized
	} else {
		primary = "stability"
	}
	if normalized := normalizeStrategy(ai); normalized != "" {
		ai = normalized
	} else {
		ai = "latency"
	}
	if latencyGap <= 0 {
		latencyGap = latencyGapMSDefault
	}
	if balanceGap <= 0 {
		balanceGap = balanceGapMSDefault
	}
	a.strategyMu.Lock()
	defer a.strategyMu.Unlock()
	a.primaryStrategy = primary
	a.aiStrategy = ai
	a.latencyGapMSValue = latencyGap
	a.balanceGapMSValue = balanceGap
}

// watchTunnelStall runs in a goroutine for the lifetime of one relay tunnel.
// Two limits apply: before the upstream ever delivered a byte, silence beyond
// the first-byte limit is a handshake black hole; afterwards, total silence in
// both directions beyond the stall limit is a stranded tunnel. Both close the
// pair so the client sees an error instead of hanging forever.
func (a *App) watchTunnelStall(client, upstream net.Conn, label, target string, started time.Time, lastAnyDir, upstreamBytes *atomic.Int64, done <-chan struct{}) {
	firstByte := time.Duration(a.cfg.TunnelFirstByteTimeoutSec) * time.Second
	stall := time.Duration(a.cfg.TunnelStallTimeoutSec) * time.Second
	if firstByte <= 0 {
		firstByte = tunnelFirstByteTimeoutSecDefault * time.Second
	}
	if stall <= 0 {
		stall = tunnelStallTimeoutSecDefault * time.Second
	}
	tick := firstByte / 4
	if tick > 5*time.Second {
		tick = 5 * time.Second
	}
	if tick < 250*time.Millisecond {
		tick = 250 * time.Millisecond
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		last := time.Unix(0, lastAnyDir.Load())
		silent := time.Since(last)
		if upstreamBytes.Load() == 0 {
			if silent >= firstByte {
				a.logf("tunnel %s via %s: no upstream byte within %s; closing (started %s ago)",
					target, label, firstByte, time.Since(started).Round(time.Second))
				_ = client.Close()
				_ = upstream.Close()
				return
			}
			continue
		}
		if silent >= stall {
			a.logf("tunnel %s via %s: silent for %s; closing stalled tunnel",
				target, label, silent.Round(time.Second))
			_ = client.Close()
			_ = upstream.Close()
			return
		}
	}
}

// progressReader refreshes activity timestamps as bytes flow through a relay
// copy direction. fromUpstream is set once data arrives from the upstream side.
type progressReader struct {
	reader       io.Reader
	at           *atomic.Int64
	fromUpstream *atomic.Int64
	upstream     bool
}

func (r *progressReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.at.Store(time.Now().UnixNano())
		if r.upstream && r.fromUpstream.Load() == 0 {
			r.fromUpstream.Store(1)
		}
	}
	return n, err
}

func (a *App) relay(client, upstream net.Conn, label, target string) {
	if !a.trackProxySockets(client, upstream) {
		return
	}
	defer a.untrackProxySockets(client, upstream)
	a.trackTunnel(label, client, upstream)
	defer a.untrackTunnel(client, upstream)
	atomic.AddInt64(&a.activeConns, 1)
	defer atomic.AddInt64(&a.activeConns, -1)
	defer client.Close()
	defer upstream.Close()

	gen, port, capture := a.capture.match(target)
	started := time.Now()
	var uploaded, downloaded int64
	var failed atomic.Bool
	// Progress timestamps feed the stall watchdog below. lastAnyDir covers
	// both directions; upstreamBytes marks the tunnel as having received data
	// from upstream, which switches the watchdog from the short first-byte
	// limit to the long established-stream limit.
	var lastAnyDir, upstreamBytes atomic.Int64
	lastAnyDir.Store(time.Now().UnixNano())
	done := make(chan struct{}, 2)
	copyDirection := func(dst, src net.Conn, upload bool) {
		srcReader := &progressReader{reader: src, at: &lastAnyDir, fromUpstream: &upstreamBytes, upstream: !upload}
		n, err := io.Copy(statsWriter{writer: dst, stats: a.stats, target: target, port: selectedPort(label), upload: upload, onBytes: a.fallbackTrafficCallback(label)}, srcReader)
		if upload {
			uploaded = n
		} else {
			downloaded = n
		}
		if err != nil {
			failed.Store(true)
		}
		if err != nil {
			// An error cannot be treated as a clean EOF: wake the other copy.
			_ = client.Close()
			_ = upstream.Close()
		} else if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			if err := cw.CloseWrite(); err != nil {
				_ = client.Close()
				_ = upstream.Close()
			}
		} else {
			// Without a half-close there is no way to signal EOF safely.
			_ = client.Close()
			_ = upstream.Close()
		}
		done <- struct{}{}
	}
	go copyDirection(upstream, client, true)
	go copyDirection(client, upstream, false)
	wdDone := make(chan struct{})
	defer close(wdDone)
	go a.watchTunnelStall(client, upstream, label, target, started, &lastAnyDir, &upstreamBytes, wdDone)
	<-done
	<-done
	if capture {
		result := "completed"
		if failed.Load() {
			result = "error"
		}
		a.capture.record(gen, captureEvent{Time: started, Kind: "connection", Port: port, Result: result, DurationMS: time.Since(started).Milliseconds(), Upload: uploaded, Download: downloaded})
	}
}

func (a *App) requestStop() {
	a.shuttingDown.Store(true)
	a.stopOnce.Do(func() { close(a.stopCh) })
}

func (a *App) cleanup() {
	a.requestStop()
	a.cleanupOnce.Do(func() {
		if err := a.capture.shutdown(); err != nil {
			a.logf("capture cleanup failed: %v", err)
		}
		a.applyMu.Lock()
		defer a.applyMu.Unlock()

		if a.httpSrv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			_ = a.httpSrv.Shutdown(ctx)
			cancel()
		}
		if a.socksLn != nil {
			_ = a.socksLn.Close()
		}
		if a.httpSrv != nil {
			_ = a.httpSrv.Close()
		}
		a.stopProxyWork()
		a.transportMu.Lock()
		for _, t := range a.transports {
			t.CloseIdleConnections()
		}
		a.transportMu.Unlock()

		pid := a.enginePIDValue()
		a.stopEnginePID(pid)
		a.setEnginePID(0)

		a.stateMu.Lock()
		services := append([]ServiceSnapshot(nil), a.state.Services...)
		httpListen, socksListen := a.state.AppliedHTTP, a.state.AppliedSOCKS
		a.stateMu.Unlock()

		restored := len(services) == 0
		if len(services) > 0 && httpListen != "" && socksListen != "" {
			if err := restoreAllProxy(services, httpListen, socksListen); err != nil {
				a.logf("restore system proxy: %v", err)
			} else {
				restored = true
				a.logf("system proxy restored")
			}
		}

		a.mu.Lock()
		a.invalidateAIHealthLocked()
		a.mode = ModeOff
		a.nextRecovery = time.Time{}
		a.fallbackReason = ""
		a.fallbackFailedAt = time.Time{}
		a.mu.Unlock()
		a.stateMu.Lock()
		a.state.Active = false
		a.state.Ready = false
		a.state.PID = 0
		if restored {
			a.state.Services = nil
			a.state.AppliedHTTP = ""
			a.state.AppliedSOCKS = ""
		}
		a.state.Mode = "off"
		a.state.Upstream = ""
		a.stateMu.Unlock()
		a.persist()
		a.stats.close()
		a.logf("clashhub off")
		if a.logFile != nil {
			_ = a.logFile.Close()
		}
	})
}

func (a *App) watchSignals() (<-chan os.Signal, func()) {
	termCh := make(chan os.Signal, 1)
	controlCh := make(chan os.Signal, 2)
	signal.Notify(termCh, syscall.SIGTERM, syscall.SIGINT)
	signal.Notify(controlCh, syscall.SIGUSR1, syscall.SIGUSR2)
	signalDone := make(chan struct{})
	go func() {
		select {
		case <-termCh:
			a.requestStop()
		case <-signalDone:
		}
	}()
	return controlCh, func() {
		signal.Stop(termCh)
		signal.Stop(controlCh)
		close(signalDone)
	}
}

func (a *App) run() error {
	if err := os.MkdirAll(appSupportDir(), 0o755); err != nil {
		return err
	}
	lf, err := os.OpenFile(lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another clashhub daemon is already running")
	}

	// Register termination before any startup work can launch an engine or
	// change proxy settings. The worker can interrupt long startup stages.
	controlCh, stopSignals := a.watchSignals()
	defer stopSignals()
	defer a.cleanup()

	a.stateMu.Lock()
	a.state.Active = true
	a.state.PID = os.Getpid()
	a.state.ProcessIdentity = processIdentity(os.Getpid())
	if a.state.StartedAt.IsZero() {
		a.state.StartedAt = time.Now()
	}
	a.stateMu.Unlock()
	a.persist()

	if a.shuttingDown.Load() {
		return nil
	}
	a.stats, err = startStatsIntegration(a.logf)
	if err != nil {
		return err
	}

	a.stats.diagnostics.bind(a)
	if a.shuttingDown.Load() {
		return nil
	}
	if err := a.startServers(); err != nil {
		return err
	}
	if a.shuttingDown.Load() {
		return nil
	}
	if err := a.watcherEngineUpdate(a.startEngine); err != nil {
		if a.shuttingDown.Load() {
			return nil
		}
		return fmt.Errorf("start engine: %w", err)
	}
	a.startupSequence()
	if a.autoStart {
		mode, _ := a.snapshotMode()
		if mode == ModeFallback {
			available := false
			for i := 0; i < 12 && !a.shuttingDown.Load(); i++ {
				for _, addr := range a.fallbackList() {
					if portOpen(addr, 500*time.Millisecond) {
						a.fallbackMu.Lock()
						a.fallbackActive = addr
						a.fallbackMu.Unlock()
						available = true
						break
					}
				}
				if available {
					break
				}
				select {
				case <-a.stopCh:
					return nil
				case <-time.After(5 * time.Second):
				}
			}
			if !available {
				return fmt.Errorf("automatic startup: fallback %s unavailable after bounded wait", a.cfg.FallbackAddr)
			}
		}
	}
	if a.shuttingDown.Load() {
		return nil
	}
	if err := a.applySystemProxy(); err != nil {
		if a.shuttingDown.Load() {
			return nil
		}
		return fmt.Errorf("apply system proxy: %w", err)
	}
	if a.shuttingDown.Load() {
		return nil
	}
	a.stateMu.Lock()
	a.state.Ready = true
	a.stateMu.Unlock()
	a.persist()

	go a.healthLoop()
	nodeCtx, cancelNodeHealth := context.WithCancel(context.Background())
	nodeHealthDone := make(chan struct{})
	go func() {
		defer close(nodeHealthDone)
		a.nodeHealthLoop(nodeCtx)
	}()
	go a.activeNodeProbeLoop(nodeCtx)
	defer func() {
		cancelNodeHealth()
		<-nodeHealthDone
	}()
	go a.persistLoop()
	go a.proxyRepairLoop()
	go a.engineWatcher()
	subCtx, cancelSubs := context.WithCancel(context.Background())
	subsDone := make(chan struct{})
	go func() {
		defer close(subsDone)
		a.runSourceSubscriptions(subCtx)
	}()
	defer func() {
		cancelSubs()
		<-subsDone
	}()

	for {
		select {
		case <-a.stopCh:
			return nil
		case s := <-controlCh:
			if s == syscall.SIGUSR1 {
				a.logf("SIGUSR1: forcing retry")
				a.forceRetry()
				continue
			}
			if s == syscall.SIGUSR2 {
				a.logf("SIGUSR2: reloading engine")
				go func() {
					if err := a.watcherEngineUpdate(a.reloadEngine); err != nil {
						a.logf("engine reload: %v", err)
					}
				}()
				continue
			}
			a.logf("received %v, shutting down", s)
			return nil
		}
	}
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
