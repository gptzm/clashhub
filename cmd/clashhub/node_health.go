package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const nodeProbeInterval = time.Minute
const nodeProbeConcurrency = 3

// Workers are shared across sources; the per-source limiter enforces the cap.
const nodeProbeWorkers = 12

// Allow the preferred active route a brief head start before using an already
// proven alternative; an incomplete probe is never classified as a failure.
const activeProbeGrace = 500 * time.Millisecond

// The controller's expected parameter enforces these exact HTTP statuses.
// 401 on the unauthenticated API endpoint proves reachability, not authentication.
const openAIProbeExpected = 401
const chatGPTProbeExpected = 200

// The landing page answers a Cloudflare challenge (403) for a plain client,
// so reachability is measured on a static path that consistently answers 200.
const chatGPTProbeURL = "https://chatgpt.com/robots.txt"

var nodeResultMaxAge = 2 * nodeProbeInterval

// Allow the controller to return the result after its own probe deadline.
const nodeControllerTimeoutMargin = time.Second

// nodeProber only talks to a literal loopback controller, without a proxy or redirects.
// Names are hashed before persistence because subscription labels may contain secrets.
type nodeProber struct {
	base, secret, probeURL, aiURL string
	// aiTargets lists the configured AI providers for probers without an App.
	aiTargets         []string
	timeout           time.Duration
	client            *http.Client
	stats             *statsIntegration
	selectionPath     string
	app               *App
	selectionMu       sync.Mutex
	uncertainStreakMu sync.Mutex
	uncertainStreak   map[string]int
}

func newNodeProber(controller, secret, probeURL string, timeout time.Duration, stats *statsIntegration) (*nodeProber, error) {
	host, _, err := net.SplitHostPort(controller)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("node probe controller must be a literal loopback address")
	}
	u, err := url.Parse(probeURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, fmt.Errorf("invalid node probe URL")
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &nodeProber{selectionPath: filepath.Join(appSupportDir(), "node-selection.json"), base: "http://" + controller, secret: secret, probeURL: probeURL, timeout: timeout, stats: stats, uncertainStreak: map[string]int{},
		client: &http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (p *nodeProber) get(ctx context.Context, path string, query url.Values, out any, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	u := p.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("controller request invalid")
	}
	req.Header.Set("Authorization", "Bearer "+p.secret)
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("controller request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// mihomo reports a failed node test as HTTP 503 with a fixed message.
		// Keep a short body marker so that failure can be told apart from a
		// controller that is itself unavailable.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("controller returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
		return fmt.Errorf("invalid controller response")
	}
	return nil
}

func (p *nodeProber) nodes(ctx context.Context) ([]string, error) {
	var response struct {
		Proxies map[string]struct {
			Type string   `json:"type"`
			All  []string `json:"all"`
		} `json:"proxies"`
	}
	if err := p.get(ctx, "/proxies", nil, &response, p.timeout); err != nil {
		return nil, err
	}
	// Only the Japanese aggregate identifies selectable candidates. BACKUP-AUTO
	// holds every node of every country; probing it made each sweep last minutes
	// and pushed fresh results past their validity window.
	seen := make(map[string]bool)
	for _, group := range []string{"JP-ALL"} {
		g, ok := response.Proxies[group]
		if !ok || !strings.EqualFold(g.Type, "URLTest") {
			continue
		}
		for _, name := range g.All {
			n, ok := response.Proxies[name]
			if !ok || len(n.All) != 0 || n.Type == "" || name == "FALLBACK-LOCAL" {
				continue
			}
			switch strings.ToLower(n.Type) {
			case "selector", "urltest", "fallback", "loadbalance", "relay", "smart", "direct", "reject", "rejectdrop", "pass", "compatible", "dns":
				continue
			}
			seen[name] = true
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func nodeHealthName(name string) string {
	sum := sha256.Sum256([]byte(name))
	return "node-" + hex.EncodeToString(sum[:])
}

type probeResult struct {
	delay     int64
	failed    bool
	uncertain bool
	at        time.Time
	duration  time.Duration
}

// uncertainDeathThreshold: a node whose probe stays uncertain for this many
// consecutive sweeps is treated as dead for selection; a single uncertain
// result alone never moves the selector.
const uncertainDeathThreshold = 2

// recordUncertain maintains the per kind|node streak of consecutive uncertain
// probes. Successes and explicit failures reset or saturate it.
func (p *nodeProber) recordUncertain(kind, node string, r probeResult) {
	p.uncertainStreakMu.Lock()
	defer p.uncertainStreakMu.Unlock()
	if p.uncertainStreak == nil {
		p.uncertainStreak = map[string]int{}
	}
	key := kind + "|" + node
	switch {
	case r.at.IsZero():
		// No evidence this sweep; leave the streak unchanged.
	case r.failed && !r.uncertain:
		delete(p.uncertainStreak, key)
	case r.uncertain:
		p.uncertainStreak[key]++
	default:
		delete(p.uncertainStreak, key)
	}
}

func (p *nodeProber) deadForSelection(kind, node string, r probeResult) bool {
	return !r.at.IsZero() && time.Since(r.at) <= nodeResultMaxAge && r.failed && !r.uncertain
}

func (p *nodeProber) probe(ctx context.Context, name, kind, target string, expected int) probeResult {
	return p.probeGeneration(ctx, name, kind, target, expected, 0, false, false)
}

func (p *nodeProber) probeGeneration(ctx context.Context, name, kind, target string, expected int, generation uint64, bound, engineLocked bool) (out probeResult) {
	if bound && p.app != nil {
		valid := func() bool {
			if !engineLocked {
				p.app.engineMu.Lock()
				defer p.app.engineMu.Unlock()
			}
			return p.app.engineGeneration == generation && ctx.Err() == nil
		}
		if !valid() {
			return probeResult{uncertain: true}
		}
		// Never block reload on network IO. Old responses carry no node evidence.
		defer func() {
			// Limiter release runs first, avoiding a slot/engineMu inversion.
			if !valid() {
				out = probeResult{uncertain: true}
			}
		}()
	}

	release, ok := p.acquireNodeProbe(ctx, name, kind)
	if !ok {
		return probeResult{uncertain: true}
	}
	defer release()

	start := time.Now()
	var result struct {
		Delay *int64 `json:"delay"`
	}
	// Ordinary probes fall back to independent targets: a node that cannot reach
	// one site may still carry every other request. AI probes keep their own
	// dual-target contract and are never widened here.
	targets := []string{target}
	expecteds := []int{expected}
	if kind == "gstatic" {
		targets = ordinaryProbeTargets(target)
		expecteds = nil
		for _, t := range targets {
			expecteds = append(expecteds, nodeProbeExpectedCode(t))
		}
	}
	var err error
	for i, t := range targets {
		attemptTimeout := p.timeout
		if i > 0 {
			attemptTimeout = p.timeout / 2
			if attemptTimeout < 2*time.Second {
				attemptTimeout = 2 * time.Second
			}
		}
		result.Delay = nil
		err = p.get(ctx, "/proxies/"+url.PathEscape(name)+"/delay", url.Values{"url": {t}, "timeout": {fmt.Sprint(attemptTimeout.Milliseconds())}, "expected": {fmt.Sprint(expecteds[i])}}, &result, attemptTimeout+nodeControllerTimeoutMargin)
		if err == nil && result.Delay != nil && *result.Delay > 0 && *result.Delay <= attemptTimeout.Milliseconds() {
			err = nil
			break
		}
		if err == nil {
			err = fmt.Errorf("invalid or timed out delay")
		}
		// Shutdown cancellation is not a node failure.
		if ctx.Err() != nil {
			return probeResult{uncertain: true}
		}
		// mihomo answers 504 when this node could not fetch the target, and 503
		// when the controller itself is overloaded. Only 504 is node evidence.
		if !targetLevelProbeFailure(err) {
			break
		}
	}
	// Shutdown cancellation is not a node failure.
	if ctx.Err() != nil {
		return probeResult{uncertain: true}
	}
	duration := time.Since(start)
	if err == nil {
		duration = time.Duration(*result.Delay) * time.Millisecond
	}
	if err != nil {
		// Controller errors are ambiguous. A decoded invalid delay or a 504 is a
		// target-specific failure, not evidence that every route is down.
		failed := targetLevelProbeFailure(err)
		return probeResult{failed: failed, uncertain: !failed, at: time.Now(), duration: duration}
	}
	return probeResult{delay: *result.Delay, at: time.Now(), duration: duration}
}

// targetLevelProbeFailure separates "this node could not fetch the target"
// from "the controller did not answer". mihomo returns 504 for the former.
func targetLevelProbeFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "invalid or timed out delay") ||
		strings.Contains(msg, "controller returned HTTP 504") ||
		strings.Contains(msg, "An error occurred in the delay test")
}

// recordProbeHistoryLocked requires engineMu for an app-backed prober.
func (p *nodeProber) recordProbeHistoryLocked(generation uint64, name, kind string, v probeResult) {
	if (p.app != nil && p.app.engineGeneration != generation) || p.stats == nil || p.stats.store == nil || v.at.IsZero() || v.uncertain {
		return
	}
	if err := p.stats.store.RecordHealth(StatsHealth{Name: nodeHealthName(name) + "-" + kind, Success: !v.failed, Duration: v.duration}); err != nil && p.stats.logf != nil {
		p.stats.logf("record node health stats failed")
	}
}

func (p *nodeProber) publishProbe(generation uint64, name, kind string, v probeResult) {
	if p.app != nil {
		p.app.engineMu.Lock()
		defer p.app.engineMu.Unlock()
	}
	p.publishProbeLocked(generation, name, kind, v)
}
func (p *nodeProber) publishProbeLocked(generation uint64, name, kind string, v probeResult) {
	p.recordProbeHistoryLocked(generation, name, kind, v)
	p.publishNodeProbeLocked(generation, name, kind, v)
}

// Selector writes never close connections; existing connections retain their route.
func (p *nodeProber) selectNode(ctx context.Context, group, name string) error {
	body, _ := json.Marshal(map[string]string{"name": name})
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, p.base+"/proxies/"+url.PathEscape(group), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("invalid selector request")
	}
	req.Header.Set("Authorization", "Bearer "+p.secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("selector controller request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("selector returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// commitAutomaticSelection performs the last routing-mode/generation check
// immediately before an automatic selector write.  The routing read lock is
// held across the write so a concurrent direct-mode commit cannot race the
// recheck and leave a direct route with a stale scanner decision.  Isolated
// probers used by tests and tools have no App and retain their previous
// behavior.
func (p *nodeProber) commitAutomaticSelection(ctx context.Context, routingGeneration uint64, routingAllowed bool, group, name string, before func()) (bool, error) {
	if p.app == nil {
		if before != nil {
			before()
		}
		return true, p.selectNode(ctx, group, name)
	}
	p.app.routingGate.RLock()
	defer p.app.routingGate.RUnlock()
	if !routingAllowed || p.app.shuttingDown.Load() || !p.app.routingProbeValidLocked(routingGeneration) {
		return false, nil
	}
	if before != nil {
		before()
	}
	return true, p.selectNode(ctx, group, name)
}

type proxyInfo struct {
	Type string   `json:"type"`
	All  []string `json:"all"`
	Now  string   `json:"now"`
}

type proxySnapshot struct {
	Proxies map[string]proxyInfo `json:"proxies"`
}

func (p *nodeProber) snapshot(ctx context.Context) (proxySnapshot, error) {
	var response proxySnapshot
	err := p.get(ctx, "/proxies", nil, &response, p.timeout)
	return response, err
}

func (p *nodeProber) cycle(ctx context.Context) error {
	// Capture a generation under the same lock used by reload, without holding it
	// during a potentially lengthy scan.
	var generation, aiGeneration uint64
	var recoveryRoutingGeneration uint64
	var recoveryRoutingAllowed bool
	if p.app != nil {
		recoveryRoutingGeneration, recoveryRoutingAllowed = p.app.routingProbeToken()
	}
	if p.app != nil {
		p.app.engineMu.Lock()
		generation = p.app.engineGeneration
		p.app.invalidateAIHealth()
		p.app.mu.Lock()
		aiGeneration = p.app.aiGeneration
		p.app.mu.Unlock()
	}
	names, err := p.nodes(ctx)
	var response proxySnapshot
	if err == nil {
		response, err = p.snapshot(ctx)
	}
	if p.app != nil {
		p.app.engineMu.Unlock()
	}
	if err != nil {
		return err
	}
	// Without a bound source index, keep selecting from the whole pool.
	idx := loadSourceIndex()
	if p.app != nil && p.app.log != nil && !idx.selectionActive() {
		p.app.logf("node scan: source mapping unavailable or stale; using whole-pool selection")
	}
	jp := map[string]bool{}
	if g, ok := response.Proxies["JP-ALL"]; ok && strings.EqualFold(g.Type, "URLTest") {
		for _, n := range g.All {
			jp[n] = true
		}
	}
	type route struct {
		group, kind, target string
		expected            int
	}
	routes := []route{{"主代理", "gstatic", p.probeURL, nodeProbeExpectedCode(p.probeURL)}}
	aiSet := aiProbeSetFor(p.aiTargets)
	if p.app != nil {
		aiSet = p.app.cfg.aiProbeSet()
	}
	if len(aiSet) > 0 {
		routes = append(routes, route{"AI主代理", "ai", "", 0})
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(routes))
	for _, r := range routes {
		r := r
		wg.Add(1)
		go func() {
			defer wg.Done()
			results := map[string]probeResult{}
			// Per selected AI provider: a target is satisfied when any of its
			// attempts succeeds; the last attempt's result is published.
			aiResults := map[string]map[string]probeResult{}
			type completedProbe struct {
				name   string
				result probeResult
				ai     map[string]probeResult
			}
			completed := make(chan completedProbe, len(names))
			// Queue the initial active node first within its source. All probes still
			// pass through the same process-wide source and node limiters.
			routeNames := append([]string(nil), names...)
			if r.kind == "ai" && containsNode(response.Proxies[r.group].All, "FALLBACK-LOCAL") {
				routeNames = append(routeNames, "FALLBACK-LOCAL")
			}
			if active, ok := resolveActiveNode(response, r.group); ok {
				for i, name := range routeNames {
					if name == active {
						copy(routeNames[1:i+1], routeNames[:i])
						routeNames[0] = active
						break
					}
				}
			}
			go func() {
				defer close(completed)
				forEachProbeNode(ctx, routeNames, idx, func(n string) {
					result := probeResult{}
					if r.target != "" {
						result = p.probeGeneration(ctx, n, r.kind, r.target, r.expected, generation, true, false)
						p.publishProbe(generation, n, r.kind, result)
					}
					var ai map[string]probeResult
					if r.kind == "ai" {
						ai = map[string]probeResult{}
						for target, checks := range aiSet {
							var last probeResult
							worst := int64(0)
							allOK, sawFail := true, false
							for _, check := range checks { // every check must pass
								checkOK := false
								for i, attempt := range check { // codes are alternatives
									last = p.probeGeneration(ctx, n, target, attempt.URL, attempt.Expected, generation, true, false)
									p.publishProbe(generation, n, target, last)
									if !last.failed && !last.uncertain && last.delay > 0 {
										checkOK = true
										break
									}
									if i == len(check)-1 {
										break
									}
								}
								if last.delay > worst {
									worst = last.delay // tie-break on the slowest required check
								}
								if last.failed {
									sawFail = true
								}
								// Always run every required check so evidence shows
								// which site failed; health requires all of them.
								allOK = allOK && checkOK
							}
							merged := last
							merged.delay = worst
							merged.failed = sawFail && !allOK     // conclusive failure fails the target
							merged.uncertain = !allOK && !sawFail // inconclusive stays inconclusive
							ai[target] = merged
						}
						// The per-node "result" slot carries the primary check's
						// outcome; the empty-target route probe itself is skipped.
						result = ai[firstAITarget(aiSet)]
					}
					select {
					case completed <- completedProbe{n, result, ai}:
					case <-ctx.Done():
					}
				})
			}()
			var routeErr error
			var lastRecoveryEpoch uint64
			recoverySent := false
			// A verified healthy selection remains sticky for the remainder of this
			// sweep, even above 500ms. New sweeps may reconsider latency.
			committedHealthy := ""
			initialGroup := response.Proxies[r.group]
			expectedSelection := initialGroup.Now
			graceUntil := time.Now().Add(activeProbeGrace)
			evaluate := func() {
				// Only selection, not probing, is serialized with reload. Re-read selector
				// state under the lock so manual changes and reloads cannot be overwritten.
				healthyNormalSelected := false
				var selectedEpoch uint64
				if p.app != nil {
					p.app.engineMu.Lock()
					defer func() {
						p.app.engineMu.Unlock()
						if healthyNormalSelected && ctx.Err() == nil && (!recoverySent || selectedEpoch != lastRecoveryEpoch) {
							recoverySent = true
							lastRecoveryEpoch = selectedEpoch
							p.app.recoverAfterHealthyScan(generation, recoveryRoutingGeneration, selectedEpoch)
						}
					}()
					if generation != p.app.engineGeneration || (r.kind == "gstatic" && p.app.primaryAlternativeActive) {
						return
					}
				}
				p.selectionMu.Lock()
				defer p.selectionMu.Unlock()
				current, e := p.snapshot(ctx)
				if e != nil {
					routeErr = e
					return
				}
				manual := false
				if p.app != nil {
					prefs, err := readManualNodes()
					if err != nil {
						routeErr = err
						return
					}
					// Desired manual mode remains authoritative even if its node is
					// absent or the engine has not yet activated it.
					manual = prefs[r.group] != ""
					if manual {
						return
					}
				}
				group, ok := current.Proxies[r.group]
				if !ok || !strings.EqualFold(group.Type, "Selector") {
					return
				}
				// An external selector write during the sweep invalidates its
				// decisions, including AI decisions from an earlier selection.
				if group.Now != expectedSelection {
					return
				}
				members := map[string]bool{}
				for _, n := range group.All {
					members[n] = true
				}
				// Membership changes outside clashhub invalidate the scan as well.
				old, ok := response.Proxies[r.group]
				if !ok || !sameMembers(old.All, group.All) {
					return
				}
				oldJP := response.Proxies["JP-ALL"]
				newJP := current.Proxies["JP-ALL"]
				if !sameMembers(oldJP.All, newJP.All) {
					return
				}
				previous := map[string]string{}
				if b, e := os.ReadFile(p.selectionPath); e == nil {
					_ = json.Unmarshal(b, &previous)
				}
				active, resolved := resolveActiveNode(current, r.group)
				fresh := func(v probeResult) bool { return !v.at.IsZero() && time.Since(v.at) <= nodeResultMaxAge }
				success := func(v probeResult) bool { return fresh(v) && !v.failed && !v.uncertain && v.delay > 0 }
				healthy := func(n string) bool {
					if r.kind == "ai" {
						if len(aiSet) == 0 || !jp[n] || !members[n] {
							return false
						}
						for target := range aiSet {
							tv, ok := aiResults[n][target]
							if !ok || !success(tv) {
								return false
							}
						}
						return true
					}
					v, ok := results[n]
					if !ok || !jp[n] || !members[n] || !success(v) {
						return false
					}
					return true
				}
				// A scan validates the selector at this instant only. Until the aggregate
				// status can be bound to its selector and expiry at read time, retain
				// unknown rather than advertising a potentially stale healthy route.
				publishAI := func(node string) {
					if p.app == nil || r.kind != "ai" {
						return
					}
					_ = node
					_ = aiGeneration
					_ = p.app.persist()
				}
				if manual {
					if resolved {
						publishAI(active)
					}
					return
				}
				// Never replace a current Japanese node based on an inconclusive
				// controller response; stale results do not block fresh alternatives.
				if !resolved {
					return
				}
				if active == "REJECT" && strings.EqualFold(current.Proxies[active].Type, "Reject") {
					// REJECT is a safe temporary route, not a node awaiting a probe.
					active = "FALLBACK-LOCAL"
				}
				// Give an eligible active node a bounded chance to complete before
				// considering an alternative; fallback and exclusions do not wait.
				explicitFailure := func(n string) bool {
					if r.kind == "ai" {
						for target := range aiSet {
							tv, ok := aiResults[n][target]
							if ok && p.deadForSelection("ai-"+target, n, tv) {
								return true
							}
						}
						return false
					}
					v, ok := results[n]
					return ok && p.deadForSelection(r.kind, n, v)
				}
				// Inconclusive results are not failures. selectNodeChoice can still
				// choose a freshly healthy, policy-eligible alternative; without one
				// it retains the current selector instead of falling back.
				// Candidates are the JP members of the selector, sorted so that
				// equal delays resolve deterministically.
				candidates := make([]string, 0, len(jp))
				for n := range jp {
					if members[n] {
						candidates = append(candidates, n)
					}
				}
				sort.Strings(candidates)
				priorities, priorityErr := readNodePriorities()
				if priorityErr != nil {
					routeErr = priorityErr
					return
				}
				_, activeDone := results[active]
				if !activeDone && active != "FALLBACK-LOCAL" && jp[active] && members[active] && !nodeExcluded(priorities, active) && time.Now().Before(graceUntil) {
					return
				}
				choice := selectNodeChoice(selectNodeChoiceInput{
					Priorities: priorities,
					Index:      idx,
					Candidates: candidates,
					Healthy:    healthy,
					Delay: func(n string) int64 {
						if r.kind == "ai" {
							if tv, ok := aiResults[n][firstAITarget(aiSet)]; ok {
								return tv.delay
							}
							return 0
						}
						return results[n].delay
					},
					Failed:   explicitFailure,
					Active:   active,
					Previous: previous[r.group],
					Strategy: p.app.strategyForKind(r.kind),
					// A nil strategy gap keeps the latencyGapMSDefault fallback
					// inside selectNodeChoice; the configured value only
					// matters when the strategy resolved to "latency".
					LatencyGapMS: p.app.latencyGapMS(),
				})
				// AI fallback needs fresh independent success against every selected
				// AI target, and every eligible node must explicitly fail at least
				// one. Pending, stale, excluded, and unknown evidence is not failure.
				fallbackAIOk := func() bool {
					if len(aiSet) == 0 {
						return false
					}
					for target := range aiSet {
						v, ok := aiResults["FALLBACK-LOCAL"][target]
						if !ok || !success(v) {
							return false
						}
					}
					return true
				}
				if choice == "" && r.kind == "ai" && members["FALLBACK-LOCAL"] && fallbackAIOk() {
					count, allFailed := 0, true
					for _, n := range candidates {
						if nodeExcluded(priorities, n) {
							continue
						}
						if idx.selectionActive() {
							id := idx.sourceID(n)
							if id == "" {
								allFailed = false // unattributed nodes are not evidence of failure
								continue
							}
							if !idx.enabled(id) {
								continue
							}
						}
						count++
						if !explicitFailure(n) {
							allFailed = false
						}
					}
					if count > 0 && allFailed {
						choice = "FALLBACK-LOCAL"
					}
				}
				if committedHealthy != "" && active == committedHealthy && healthy(active) && healthy(choice) && idx.sourceID(active) == idx.sourceID(choice) && effectiveNodePriority(priorities, idx, active) == effectiveNodePriority(priorities, idx, choice) {
					choice = active
				}
				if choice == "" || !members[choice] {
					return
				}
				// Keep each source's url-test group pinned to a node this sweep just
				// verified. mihomo re-tests lazily (60s) and otherwise keeps routing
				// through a node that has already died, which is invisible to the
				// top-level selector and poisons every request, not just probes.
				if src := idx.sourceID(choice); src != "" {
					if g, ok := current.Proxies["JP-"+src]; ok && strings.EqualFold(g.Type, "URLTest") && g.Now != choice && healthy(choice) {
						oldJP := g.Now
						committed, _ := p.commitAutomaticSelection(ctx, recoveryRoutingGeneration, recoveryRoutingAllowed, "JP-"+src, choice, nil)
						if !committed {
							return
						}
						// Tunnels dialed while the old JP node was active stay
						// bound to it. If this sweep proved that node dead,
						// fast-fail them; latency-driven repins keep them.
						if p.app != nil && explicitFailure(oldJP) {
							p.app.purgeEngineTunnels(fmt.Sprintf("JP-%s node %s -> %s", src, oldJP, choice))
						}
					}
				}
				if group.Now != choice {
					oldNode := group.Now
					committed, e := p.commitAutomaticSelection(ctx, recoveryRoutingGeneration, recoveryRoutingAllowed, r.group, choice, func() {
						if p.app != nil {
							if r.kind == "ai" {
								p.app.aiSelectionGeneration++
							} else {
								p.app.selectionGeneration++
							}
						}
					})
					if !committed {
						return
					}
					if e != nil {
						routeErr = e
						return
					}
					verified, e := p.snapshot(ctx)
					if e != nil {
						routeErr = fmt.Errorf("selector readback failed: %w", e)
						return
					}
					selected, ok := verified.Proxies[r.group]
					if !ok || !strings.EqualFold(selected.Type, "Selector") || selected.Now != choice {
						routeErr = fmt.Errorf("selector readback mismatch")
						return
					}
					expectedSelection = choice
					// Fast-fail tunnels stranded on the outgoing node, but only
					// with fresh failure evidence: a latency- or priority-driven
					// handover leaves the old node working, and killing its
					// tunnels would just churn client connections.
					if p.app != nil && explicitFailure(oldNode) {
						p.app.purgeEngineTunnels(fmt.Sprintf("%s node %s -> %s", r.group, oldNode, choice))
					}
					if healthy(choice) {
						committedHealthy = choice
					}
				}
				publishAI(choice)
				if choice != "FALLBACK-LOCAL" {
					previous[r.group] = choice
				}
				if e := saveNodeSelection(p.selectionPath, previous); e != nil {
					routeErr = fmt.Errorf("persist node selection: %w", e)
					return
				}
				if r.kind == "gstatic" && choice != "FALLBACK-LOCAL" && healthy(choice) {
					healthyNormalSelected = true
					if p.app != nil {
						selectedEpoch = p.app.selectionGeneration
					}
				}
			}
			graceTimer := time.NewTimer(time.Until(graceUntil))
			defer graceTimer.Stop()
			graceC := graceTimer.C
			for {
				var item completedProbe
				var ok bool
				select {
				case <-ctx.Done():
					goto finished
				case <-graceC:
					graceC = nil
					if routeErr == nil {
						evaluate()
					}
					continue
				case item, ok = <-completed:
					if !ok {
						goto finished
					}
				}
				results[item.name] = item.result
				if item.ai != nil {
					aiResults[item.name] = item.ai
				}
				// Coalesce a short burst of completed probes without waiting for a
				// slow node or holding any selection lock.
				if routeErr == nil && ctx.Err() == nil {
					timer := time.NewTimer(25 * time.Millisecond)
					select {
					case <-timer.C:
					case <-ctx.Done():
						timer.Stop()
					}
					for len(completed) > 0 {
						next := <-completed
						results[next.name] = next.result
						if next.ai != nil {
							aiResults[next.name] = next.ai
						}
					}
					if ctx.Err() == nil {
						evaluate()
					}
				}
			}
		finished:
			if routeErr != nil {
				errs <- routeErr
			} else if ctx.Err() != nil {
				errs <- ctx.Err()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// selectNodeChoiceInput carries the per-sweep evidence needed for automatic
// selection. Candidates are JP members of the selector, sorted by name.
// Strategy selects between "stability" (keep the active node until it fails),
// "latency" (chase the best node once the active one trails by more than
// LatencyGapMS), and "balanced" (the same chase rule with the looser
// BalanceGapMS threshold). Empty keeps the legacy sticky behavior.
type selectNodeChoiceInput struct {
	Priorities   map[string]string
	Index        sourceIndex
	Candidates   []string
	Healthy      func(string) bool // fresh success incl. JP and selector membership
	Delay        func(string) int64
	Failed       func(string) bool // explicit, fresh failure
	Active       string
	Previous     string
	Strategy     string
	LatencyGapMS int
	BalanceGapMS int
}

// selectNodeChoice implements source-priority selection. The sticky node is
// kept only when its source is the highest-priority enabled source with fresh
// success evidence; otherwise the previous node or the fastest node within
// that source wins. Sources with no freshly healthy candidate are skipped, not
// classified as failed. Without a proven eligible candidate the current route
// holds. Target-specific failures and policy exclusions alone never select an
// unverified FALLBACK-LOCAL.
func selectNodeChoice(in selectNodeChoiceInput) string {
	in = applyNodePriorities(in)
	gap := int64(in.LatencyGapMS)
	if gap <= 0 {
		gap = int64(latencyGapMSDefault)
	}
	balanceGap := int64(in.BalanceGapMS)
	if balanceGap <= 0 {
		balanceGap = int64(balanceGapMSDefault)
	}
	// bestExcluding returns the best healthy delay among candidates other than
	// the named node (the pool the active node is competing against).
	bestExcluding := func(skip string) int64 {
		best := int64(1 << 62)
		for _, n := range in.Candidates {
			if n == skip || n == "FALLBACK-LOCAL" {
				continue
			}
			if in.Healthy(n) && in.Delay(n) < best {
				best = in.Delay(n)
			}
		}
		return best
	}
	keepNode := func(n string, bestInScope int64) bool {
		if n == "" || !in.Healthy(n) {
			return false
		}
		switch in.Strategy {
		case "stability":
			// Keep a proven-working node regardless of latency.
			return true
		case "latency", "balanced":
			if bestInScope == int64(1)<<62 {
				return true // no competitor; hold the known-good node
			}
			limit := gap
			if in.Strategy == "balanced" {
				limit = balanceGap // 均衡：差距在阈值内就粘住，超过才切
			}
			return in.Delay(n)-bestInScope <= limit
		default: // legacy sticky: tolerate up to 500ms before chasing speed
			return in.Delay(n) <= 500
		}
	}
	if !in.Index.selectionActive() {
		if keepNode(in.Active, bestExcluding(in.Active)) {
			return in.Active
		}
		if keepNode(in.Previous, bestExcluding(in.Previous)) {
			return in.Previous
		}
		best, choice := int64(1<<62), ""
		for _, n := range in.Candidates {
			if in.Healthy(n) && in.Delay(n) < best {
				best, choice = in.Delay(n), n
			}
		}
		if choice != "" {
			return choice
		}
		// Even a complete set of failures is tied to one destination. Keep
		// the current route rather than infer a global node outage.
		return ""
	}
	for _, id := range in.Index.enabledOrder() {
		best, choice := int64(1<<62), ""
		for _, n := range in.Candidates {
			if n == "FALLBACK-LOCAL" || in.Index.sourceID(n) != id {
				continue
			}
			if in.Healthy(n) && in.Delay(n) < best {
				best, choice = in.Delay(n), n
			}
		}
		if choice == "" {
			continue // uncertain, pending and stale evidence remain inconclusive
		}
		if in.Index.sourceID(in.Active) == id && keepNode(in.Active, best) {
			return in.Active
		}
		if in.Index.sourceID(in.Previous) == id && keepNode(in.Previous, best) {
			return in.Previous
		}
		return choice
	}
	// A single target cannot establish that the pool is globally unusable.
	return ""
}

// Resolve only verified group edges and reject incomplete or cyclic controller data.
func resolveActiveNode(snapshot proxySnapshot, group string) (string, bool) {
	seen := map[string]bool{}
	for {
		if seen[group] {
			return "", false
		}
		seen[group] = true
		p, ok := snapshot.Proxies[group]
		if !ok {
			return "", false
		}
		switch strings.ToLower(p.Type) {
		case "selector", "urltest", "fallback", "loadbalance", "relay", "smart":
			found := false
			for _, name := range p.All {
				if name == p.Now {
					found = true
					break
				}
			}
			if !found || p.Now == "" {
				return "", false
			}
			group = p.Now
		default:
			if p.Type == "" || len(p.All) != 0 {
				return "", false
			}
			return group, true
		}
	}
}

func sameMembers(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := map[string]int{}
	for _, n := range a {
		counts[n]++
	}
	for _, n := range b {
		counts[n]--
		if counts[n] < 0 {
			return false
		}
	}
	return true
}

func saveNodeSelection(path string, selections map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.Marshal(selections)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".node-selection-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func (a *App) nodeHealthLoop(ctx context.Context) {
	p, err := newNodeProber(a.cfg.Controller, engineSecret(), a.cfg.ProbeURL, time.Duration(a.cfg.ProbeTimeoutSec)*time.Second, a.stats)
	if err != nil {
		a.logf("node health disabled: %v", err)
		return
	}
	ai, err := url.Parse(a.cfg.AIProbeURL)
	if err != nil || (ai.Scheme != "https" && ai.Scheme != "http") || ai.Host == "" || ai.User != nil || ai.Fragment != "" {
		a.logf("node AI health disabled: invalid URL")
	} else {
		p.aiURL = a.cfg.AIProbeURL
	}
	p.aiTargets = a.cfg.aiTargets()
	p.app = a
	defer p.client.CloseIdleConnections()
	// Ticks never start overlapping rounds. If a round lasts longer than the
	// interval, a pending tick may start the next round immediately; a complete
	// sweep is not guaranteed to finish within one minute.
	ticker := time.NewTicker(nodeProbeInterval)
	defer ticker.Stop()
	for {
		if err := p.cycle(ctx); err != nil && ctx.Err() == nil {
			a.logf("node health discovery failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-a.nodeScanWake:
		}
	}
}

// firstAITarget returns a deterministic first key of the probe set.
func firstAITarget(set map[string][][]aiProbeAttempt) string {
	best := ""
	for k := range set {
		if best == "" || k < best {
			best = k
		}
	}
	return best
}
