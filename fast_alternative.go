package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// selectFreshAlternative makes one bounded, temporary choice after the full
// primary path failed. The node is only a candidate: callers must validate the
// actual primary path (including Japan egress) before declaring recovery.
// No preference is changed and an unknown controller/pin fails closed.
func (a *App) selectFreshAlternative(ctx context.Context, token probeToken, excluded map[string]bool) (probeToken, string) {
	if a.stats == nil || a.stats.diagnostics == nil || a.shuttingDown.Load() {
		return token, ""
	}
	if !a.lockEngineContext(ctx) {
		return token, ""
	}
	defer a.engineMu.Unlock()
	a.routingGate.RLock()
	if !a.probeTokenValidLocked(token) || a.shuttingDown.Load() || ctx.Err() != nil {
		a.routingGate.RUnlock()
		return token, ""
	}
	a.routingGate.RUnlock()
	prefs, err := readManualNodes()
	if err != nil || prefs["主代理"] != "" {
		return token, ""
	}
	p, err := a.selectionProber()
	if err != nil {
		return token, ""
	}
	defer p.client.CloseIdleConnections()
	snapshot, err := p.snapshot(ctx)
	if err != nil {
		return token, ""
	}
	group := snapshot.Proxies["主代理"]
	active, resolved := resolveActiveNode(snapshot, "主代理")
	// The current route may resolve through an automatic subgroup. A new
	// selection is accepted only after PUT and explicit selector readback.
	if !resolved || !strings.EqualFold(group.Type, "Selector") || active == "REJECT" {
		return token, ""
	}
	excluded[active] = true
	members, ok := selectableNodes(snapshot, "主代理")
	if !ok {
		return token, ""
	}
	jp, ok := snapshot.Proxies["JP-ALL"]
	if !ok {
		return token, ""
	}
	allowed := map[string]bool{}
	for _, n := range jp.All {
		allowed[n] = true
	}
	var candidates []string
	for _, n := range members {
		if !excluded[n] && allowed[n] {
			candidates = append(candidates, n)
		}
	}
	if len(candidates) == 0 {
		return token, ""
	}
	sort.Strings(candidates)
	priorities, err := readNodePriorities()
	if err != nil {
		return token, ""
	}
	idx := loadSourceIndex()
	d := a.stats.diagnostics
	d.mu.Lock()
	results := make(map[string]nodeDiagnosticResult, len(candidates))
	for _, n := range candidates {
		results[n] = d.results[n]["gstatic"]
	}
	d.mu.Unlock()
	now := time.Now()
	healthy := func(n string) bool {
		r := results[n]
		return r.Status == "ok" && r.DelayMS > 0 && !r.CheckedAt.IsZero() && !r.CheckedAt.After(now) && r.ExpiresAt.After(now)
	}
	choice := selectNodeChoice(selectNodeChoiceInput{Priorities: priorities, Index: idx, Candidates: candidates, Healthy: healthy, Delay: func(n string) int64 { return results[n].DelayMS }, Failed: func(string) bool { return false }, Strategy: a.strategyForKind("general"), LatencyGapMS: a.latencyGapMS(), BalanceGapMS: a.balanceGapMS()})
	if choice == "" || choice == "FALLBACK-33210" || !healthy(choice) {
		return token, ""
	}
	// Check both route generation and manual preference immediately before PUT.
	a.routingGate.RLock()
	valid := a.probeTokenValidLocked(token) && !a.shuttingDown.Load() && ctx.Err() == nil
	a.routingGate.RUnlock()
	prefs, err = readManualNodes()
	if !valid || err != nil || prefs["主代理"] != "" {
		return token, ""
	}
	a.selectionGeneration++
	newToken := token
	newToken.selection = a.selectionGeneration
	if err := p.selectNode(ctx, "主代理", choice); err != nil {
		return newToken, ""
	}
	verified, err := p.snapshot(ctx)
	if err != nil || verified.Proxies["主代理"].Now != choice {
		return newToken, ""
	}
	return newToken, choice
}

func (a *App) alternativeStillSelected(ctx context.Context, token probeToken, choice string) bool {
	if choice == "" {
		return false
	}
	if !a.lockEngineContext(ctx) {
		return false
	}
	defer a.engineMu.Unlock()
	a.routingGate.RLock()
	valid := a.probeTokenValidLocked(token) && !a.shuttingDown.Load() && ctx.Err() == nil
	a.routingGate.RUnlock()
	if !valid {
		return false
	}
	prefs, err := readManualNodes()
	if err != nil || prefs["主代理"] != "" {
		return false
	}
	p, err := a.selectionProber()
	if err != nil {
		return false
	}
	defer p.client.CloseIdleConnections()
	snapshot, err := p.snapshot(ctx)
	if err != nil || snapshot.Proxies["主代理"].Now != choice {
		return false
	}
	previous := map[string]string{}
	data, err := os.ReadFile(p.selectionPath)
	if err == nil {
		if json.Unmarshal(data, &previous) != nil || previous == nil {
			return false
		}
	} else if !os.IsNotExist(err) {
		return false
	}
	previous["主代理"] = choice
	return saveNodeSelection(p.selectionPath, previous) == nil
}

func (a *App) tryFreshAlternative(token probeToken) (probeToken, string) {
	if a.stats == nil || a.stats.diagnostics == nil {
		return token, ""
	}
	if a.log != nil {
		a.logf("primary recovery: checking fresh alternatives")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if !a.lockEngineContext(ctx) {
		return token, ""
	}
	a.primaryAlternativeActive = true
	a.engineMu.Unlock()
	defer func() { a.engineMu.Lock(); a.primaryAlternativeActive = false; a.engineMu.Unlock() }()
	excluded := map[string]bool{}
	next := token
	attempts := 0
	for ctx.Err() == nil && !a.shuttingDown.Load() && attempts < 2 {
		var choice string
		next, choice = a.selectFreshAlternative(ctx, next, excluded)
		if choice == "" {
			return next, ""
		}
		attempts++
		excluded[choice] = true
		// This must validate the real route, not merely the controller's node probe.
		candidateCtx, candidateCancel := context.WithTimeout(ctx, 9*time.Second)
		ok, _ := a.probePrimaryFullContext(candidateCtx)
		candidateCancel()
		if ok && a.alternativeStillSelected(ctx, next, choice) {
			if a.log != nil {
				a.logf("primary recovery: alternate path confirmed")
			}
			// Existing tunnels were dialed while the failed node was active and
			// stay bound to it. Fast-fail them so clients reconnect on the
			// confirmed alternative instead of hanging on a black hole.
			a.purgeEngineTunnels("primary repinned to " + choice)
			return next, choice
		}
	}
	if a.log != nil {
		a.logf("primary recovery: no confirmed alternative within budget")
	}
	return next, ""
}

func (a *App) lockEngineContext(ctx context.Context) bool {
	for !a.engineMu.TryLock() {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(10 * time.Millisecond):
		}
	}
	return true
}

// probePrimaryFullContext uses a single cancelable budget; egress is checked
// once after ordinary connectivity succeeds. Callers verify the selector token.
func (a *App) probePrimaryFullContext(parent context.Context) (bool, string) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	go func() {
		select {
		case <-a.stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	if !a.lockEngineContext(ctx) {
		return false, "probe cancelled"
	}
	a.routingGate.RLock()
	token := probeToken{a.routingGeneration, a.engineGeneration, a.selectionGeneration, a.aiSelectionGeneration}
	allowed := a.routingProbeValidLocked(token.routing)
	a.routingGate.RUnlock()
	a.engineMu.Unlock()
	if !allowed || ctx.Err() != nil {
		return false, "routing mode changed"
	}
	timeout := time.Duration(a.cfg.ProbeTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	connected := false
	label := "gstatic"
	var latency int64
	lastReason := "no probe target"
	for i, target := range ordinaryProbeTargets(a.cfg.ProbeURL) {
		budget := timeout
		label = "gstatic"
		if i > 0 {
			label = "independent"
			budget /= 2
			if budget < 2*time.Second {
				budget = 2 * time.Second
			}
		}
		started := time.Now()
		_, ms, err := probeStatusContext(ctx, a.cfg.PrimaryAddr, target, probeExpectedCode(target), budget)
		if !a.lockEngineContext(ctx) {
			return false, "probe cancelled"
		}
		a.routingGate.RLock()
		valid := a.probeTokenValidLocked(token) && ctx.Err() == nil && !a.shuttingDown.Load()
		if valid {
			a.stats.health(label, started, err)
			if label == "gstatic" {
				a.mu.Lock()
				a.probeMS = ms
				a.mu.Unlock()
			}
		}
		a.routingGate.RUnlock()
		a.engineMu.Unlock()
		if !valid {
			return false, "routing mode changed"
		}
		if err == nil {
			connected = true
			latency = ms
			break
		}
		lastReason = target + ": " + err.Error()
		if ctx.Err() != nil {
			return false, "probe cancelled"
		}
	}
	if !connected {
		return false, lastReason
	}

	if ctx.Err() != nil {
		return false, "probe cancelled"
	}
	return true, fmt.Sprintf("%s %dms", label, latency)
}

func egressTimeBudget(ctx context.Context, now time.Time) time.Duration {
	budget := 4 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		remaining := deadline.Sub(now) - 500*time.Millisecond
		if remaining < budget {
			return remaining
		}
	}
	return budget
}
