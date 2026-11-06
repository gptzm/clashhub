package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func nodePrioritiesPath() string { return filepath.Join(appSupportDir(), "node-priorities.json") }

// priorityLevels is ordered from strongest to weakest. Source defaults and
// per-node overrides share the same five tiers.
var priorityLevels = []string{"highest", "high", "medium", "low", "lowest"}

func validPriorityLevel(v string) bool {
	for _, l := range priorityLevels {
		if l == v {
			return true
		}
	}
	return false
}

// downgradePriority returns the next weaker tier, floored at lowest.
func downgradePriority(v string) string {
	for i, l := range priorityLevels {
		if l == v {
			if i+1 < len(priorityLevels) {
				return priorityLevels[i+1]
			}
			return "lowest"
		}
	}
	return "lowest"
}

// validNodePriority: per-node stored values. "disabled" excludes a node from
// automatic selection entirely (the GUI renders it as a checkbox).
func validNodePriority(v string) bool {
	if v == "disabled" {
		return true
	}
	return validPriorityLevel(strings.TrimPrefix(v, "disabled:"))
}

func nodePriorityOverride(v string) string {
	return strings.TrimPrefix(v, "disabled:")
}

// nodePriorityRank orders tiers for auto selection; higher wins. Disabled
// ranks below everything and is filtered out before ranking anyway.
func nodePriorityRank(v string) int {
	switch v {
	case "highest":
		return 4
	case "high":
		return 3
	case "low":
		return 1
	case "lowest":
		return 0
	case "disabled":
		return -1
	default:
		return 2
	}
}

// effectiveNodePriority resolves the tier a node is actually treated as:
// explicit per-node override > source default, with source downgrade patterns
// (matched against the original pre-aggregation name, case-insensitive)
// dropping a node one tier below its source default, floored at low.
// An unbound index cannot resolve sources or names; those nodes rank as
// medium exactly as before, so legacy behaviour is preserved verbatim.
func effectiveNodePriority(prefs map[string]string, idx sourceIndex, node string) string {
	if v := nodePriorityOverride(prefs[node]); validPriorityLevel(v) {
		return v
	}
	if !idx.bound {
		return "medium"
	}
	id := idx.sourceID(node)
	if id == "" {
		return "medium"
	}
	def := idx.defaultPriority(id)
	// A subscription that is about to expire should be used before it is lost.
	return boostNearExpiry(def, idx.expiry[id], time.Now())
}

// raisePriority moves one tier toward highest, capped there.
func raisePriority(v string) string {
	for i, l := range priorityLevels {
		if l == v {
			if i > 0 {
				return priorityLevels[i-1]
			}
			return "highest"
		}
	}
	return "highest"
}

// boostNearExpiry raises a source's default tier when its subscription is about
// to expire: under 3 days by one tier, under 1 day by two. Already expired
// subscriptions get no boost; their nodes should not be preferred at all.
func boostNearExpiry(level string, expiresAt int64, now time.Time) string {
	if expiresAt <= 0 {
		return level
	}
	remaining := time.Unix(expiresAt, 0).Sub(now)
	if remaining <= 0 {
		return level
	}
	steps := 0
	switch {
	case remaining < 24*time.Hour:
		steps = 2
	case remaining < 72*time.Hour:
		steps = 1
	}
	for i := 0; i < steps; i++ {
		level = raisePriority(level)
	}
	return level
}

// nodeExcluded is true only for an explicit per-node disable; source defaults
// and name patterns reduce the tier but never remove the node.
func nodeExcluded(prefs map[string]string, node string) bool {
	return prefs[node] == "disabled" || strings.HasPrefix(prefs[node], "disabled:")
}
func readNodePriorities() (map[string]string, error) {
	b, err := os.ReadFile(nodePrioritiesPath())
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var prefs map[string]string
	if json.Unmarshal(b, &prefs) != nil || prefs == nil {
		return nil, fmt.Errorf("invalid node priorities")
	}
	for n, v := range prefs {
		if n == "" || !validNodePriority(v) {
			return nil, fmt.Errorf("invalid node priorities")
		}
	}
	return prefs, nil
}

// Priority ranks fresh healthy candidates across enabled sources. Source order,
// stickiness and delay remain tie breakers inside the highest healthy tier.
// Per-source defaults and downgrade patterns apply without any stored prefs;
// explicitly disabled nodes are never candidates. With no fresh healthy
// candidate, preserve the original conservative failure and uncertainty
// handling unchanged.
func applyNodePriorities(in selectNodeChoiceInput) selectNodeChoiceInput {
	eligible := func(n string) bool {
		if nodeExcluded(in.Priorities, n) {
			return false
		}
		if !in.Index.selectionActive() {
			return true
		}
		id := in.Index.sourceID(n)
		return id != "" && in.Index.enabled(id)
	}
	// Exclusion applies even without fresh healthy evidence and to sticky nodes.
	originalHealthy := in.Healthy
	in.Healthy = func(n string) bool { return eligible(n) && originalHealthy(n) }
	candidates := make([]string, 0, len(in.Candidates))
	for _, n := range in.Candidates {
		if !nodeExcluded(in.Priorities, n) {
			candidates = append(candidates, n)
		}
	}
	in.Candidates = candidates
	best, found := -1, false
	for _, n := range in.Candidates {
		if !eligible(n) || !in.Healthy(n) {
			continue
		}
		rank := nodePriorityRank(effectiveNodePriority(in.Priorities, in.Index, n))
		if !found || rank > best {
			best, found = rank, true
		}
	}
	if !found {
		return in
	}
	// Restrict candidates and sticky choices to the proven healthy top tier.
	// Uncertain higher-tier nodes do not prevent using a proven healthy node.
	tierHealthy := in.Healthy
	in.Healthy = func(n string) bool {
		return tierHealthy(n) &&
			nodePriorityRank(effectiveNodePriority(in.Priorities, in.Index, n)) == best
	}
	candidates = make([]string, 0, len(in.Candidates))
	for _, n := range in.Candidates {
		if in.Healthy(n) {
			candidates = append(candidates, n)
		}
	}
	in.Candidates = candidates
	return in
}

type nodePriorityRow struct {
	Node     string `json:"node"`
	Priority string `json:"priority"`
	Override string `json:"override"`
	Enabled  bool   `json:"enabled"`
}

type nodePriorityUpdate struct {
	Node     string  `json:"node"`
	Priority *string `json:"priority"`
	Enabled  *bool   `json:"enabled"`
}

func (a *App) serveNodePriorities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", 405)
		return
	}
	var req nodePriorityUpdate
	if r.Method == http.MethodPut {
		if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
			http.Error(w, "JSON required", 415)
			return
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		dec.DisallowUnknownFields()
		if dec.Decode(&req) != nil || dec.Decode(&struct{}{}) != io.EOF || req.Node == "" || (req.Priority == nil && req.Enabled == nil) || (req.Priority != nil && *req.Priority != "default" && *req.Priority != "disabled" && !validPriorityLevel(*req.Priority)) || (req.Priority != nil && *req.Priority == "disabled" && req.Enabled != nil && *req.Enabled) {
			http.Error(w, "invalid request", 400)
			return
		}
	}
	a.engineMu.Lock()
	defer a.engineMu.Unlock()
	if a.shuttingDown.Load() {
		http.Error(w, "shutting down", 503)
		return
	}
	prefs, err := readNodePriorities()
	if err != nil {
		http.Error(w, "priority read failed", 500)
		return
	}
	p, err := a.selectionProber()
	if err != nil {
		http.Error(w, "controller unavailable", 502)
		return
	}
	defer p.client.CloseIdleConnections()
	snap, err := p.snapshot(r.Context())
	if err != nil {
		http.Error(w, "controller unavailable", 502)
		return
	}
	known := map[string]bool{}
	for _, g := range []string{"主代理", "AI主代理"} {
		nodes, _ := selectableNodes(snap, g)
		for _, n := range nodes {
			known[n] = true
		}
	}
	if r.Method == http.MethodPut {
		if !known[req.Node] {
			http.Error(w, "node is not a selectable member", 400)
			return
		}
		disabled := nodeExcluded(prefs, req.Node)
		override := nodePriorityOverride(prefs[req.Node])
		if override == "disabled" {
			override = ""
		}
		if req.Priority != nil {
			switch *req.Priority {
			case "disabled":
				disabled = true
			case "default":
				override = ""
			default:
				override = *req.Priority
			}
		}
		if req.Enabled != nil {
			disabled = !*req.Enabled
		}
		// Do not partially mutate selectors on a policy update. Require the user
		// to move active or dynamic routes first; no controller write occurs here.
		if disabled && !nodeExcluded(prefs, req.Node) {
			// Invalidate already-running scans even if a later controller write or
			// persistence fails after one selector was safely moved to REJECT.
			a.advanceEngineGenerationLocked()
			manual, e := readManualNodes()
			if e != nil {
				http.Error(w, "manual preference unavailable", 500)
				return
			}
			for _, group := range []string{"主代理", "AI主代理"} {
				g, exists := snap.Proxies[group]
				if !exists {
					http.Error(w, "cannot verify selector membership", 409)
					return
				}
				nodes, valid := selectableNodes(snap, group)
				if !valid {
					http.Error(w, "cannot verify selector membership", 409)
					return
				}
				if !containsNode(nodes, req.Node) {
					continue
				}
				if manual[group] == req.Node {
					http.Error(w, "clear manual selection before disabling node", 409)
					return
				}
				active, resolved := resolveActiveNode(snap, group)
				if !resolved || active == req.Node || g.Now != active {
					http.Error(w, "switch selector to a different verified node before disabling", 409)
					return
				}
			}
		}
		if disabled {
			if override == "" {
				prefs[req.Node] = "disabled"
			} else {
				prefs[req.Node] = "disabled:" + override
			}
		} else if override == "" {
			delete(prefs, req.Node)
		} else {
			prefs[req.Node] = override
		}
		if err := saveNodeSelection(nodePrioritiesPath(), prefs); err != nil {
			http.Error(w, "priority write failed", 500)
			return
		}
		a.advanceEngineGenerationLocked()
		select {
		case a.nodeScanWake <- struct{}{}:
		default:
		}
	}
	names := make([]string, 0, len(known))
	for n := range known {
		names = append(names, n)
	}
	sort.Strings(names)
	rows := make([]nodePriorityRow, 0, len(names))
	defaults := loadSourceIndex()
	for _, n := range names {
		override := nodePriorityOverride(prefs[n])
		if override == "" || override == "disabled" {
			override = "default"
		}
		rows = append(rows, nodePriorityRow{Node: n, Priority: effectiveNodePriority(prefs, defaults, n), Override: override, Enabled: !nodeExcluded(prefs, n)})
	}
	// Removed nodes retain preferences on disk, but are not offered as selectable.
	policies := make([]map[string]any, 0, len(defaults.defs))
	for _, source := range defaults.defs {
		priority, patterns := source.sourcePriorityDefaults()
		if patterns == nil {
			patterns = []string{}
		}
		policies = append(policies, map[string]any{"id": source.ID, "default_priority": priority, "downgrade_patterns": patterns})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"nodes": rows, "sources": policies, "default_priority": "per_source", "scope": "across_sources", "applies_to": "automatic_selection"})
}
