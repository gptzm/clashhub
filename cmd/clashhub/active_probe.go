package main

import (
	"context"
	"net/url"
	"time"
)

// activeNodeProbe keeps the node(s) currently carrying traffic fresh between full
// sweeps. A low-traffic link produces no failure signal of its own, so it is
// probed more often than a busy one.
const (
	activeProbeIdleInterval  = 10 * time.Second
	activeProbeBusyInterval  = 30 * time.Second
	activeProbeBusyThreshold = 100 * 1024 // bytes/second observed on the node
)

func (a *App) nodeTrafficService() *nodeTrafficService {
	if a.stats == nil {
		return nil
	}
	return a.stats.nodeTraffic
}

func (a *App) currentActiveNodes(ctx context.Context, p *nodeProber) map[string]string {
	a.engineMu.Lock()
	snap, err := p.snapshot(ctx)
	a.engineMu.Unlock()
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, group := range []string{"主代理", "AI主代理"} {
		active, ok := resolveActiveNode(snap, group)
		if !ok || active == "" || active == "FALLBACK-LOCAL" || active == "REJECT" {
			continue
		}
		out[group] = active
	}
	return out
}

// activeNodeProbeLoop runs while clashhub is primary. It never changes selectors;
// it only refreshes probe evidence for the node already in use.
func (a *App) activeNodeProbeLoop(ctx context.Context) {
	p, err := newNodeProber(a.cfg.Controller, engineSecret(), a.cfg.ProbeURL, time.Duration(a.cfg.ProbeTimeoutSec)*time.Second, a.stats)
	if err != nil {
		return
	}
	p.app = a
	if ai, err := url.Parse(a.cfg.AIProbeURL); err == nil && ai.Host != "" {
		p.aiURL = a.cfg.AIProbeURL
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	last := map[string]time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if a.shuttingDown.Load() {
			return
		}
		mode, _ := a.snapshotMode()
		if mode != ModePrimary {
			continue
		}
		active := a.currentActiveNodes(ctx, p)
		if len(active) == 0 {
			continue
		}
		a.engineMu.Lock()
		generation := a.engineGeneration
		a.engineMu.Unlock()
		now := time.Now()
		for group, node := range active {
			interval := activeProbeIdleInterval
			if svc := a.nodeTrafficService(); svc != nil {
				if rate, ok := svc.currentRate(node); ok && rate.Upload+rate.Download > activeProbeBusyThreshold {
					interval = activeProbeBusyInterval
				}
			}
			kinds := []struct {
				kind, target string
				expected     int
			}{{"gstatic", p.probeURL, nodeProbeExpectedCode(p.probeURL)}}
			if group == "AI主代理" {
				for target, checks := range a.cfg.aiProbeSet() {
					for _, check := range checks {
						if len(check) > 0 {
							kinds = append(kinds, struct {
								kind, target string
								expected     int
							}{target, check[0].URL, check[0].Expected})
						}
					}
				}
			}
			for _, kind := range kinds {
				key := node + "|" + kind.kind
				if at, ok := last[key]; ok && now.Sub(at) < interval {
					continue
				}
				last[key] = now
				v := p.probeGeneration(ctx, node, kind.kind, kind.target, kind.expected, generation, true, false)
				p.publishProbe(generation, node, kind.kind, v)
			}
		}
	}
}
