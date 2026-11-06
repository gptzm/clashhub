package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func manualNodePath() string { return filepath.Join(appSupportDir(), "manual-nodes.json") }

func readManualNodes() (map[string]string, error) {
	b, err := os.ReadFile(manualNodePath())
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err = json.Unmarshal(b, &m); err != nil || m == nil {
		return nil, fmt.Errorf("invalid manual node preferences")
	}
	for group, node := range m {
		if !validNodeGroup(group) || node == "" {
			return nil, fmt.Errorf("invalid manual node preferences")
		}
	}
	return m, nil
}
func validNodeGroup(g string) bool { return g == "主代理" || g == "AI主代理" }

// Caller holds app.engineMu, which serializes all selector changes and preference writes.
func (p *nodeProber) manualSelectionLocked(group string, snap proxySnapshot) (bool, error) {
	m, err := readManualNodes()
	if err != nil {
		return false, err
	}
	nodes, ok := selectableNodes(snap, group)
	return ok && m[group] != "" && containsNode(nodes, m[group]) && snap.Proxies[group].Now == m[group], nil
}
func (a *App) selectionProber() (*nodeProber, error) {
	return newNodeProber(a.cfg.Controller, engineSecret(), a.cfg.ProbeURL, time.Duration(a.cfg.ProbeTimeoutSec)*time.Second, a.stats)
}
func selectableNodes(s proxySnapshot, group string) ([]string, bool) {
	g, ok := s.Proxies[group]
	if !ok || !strings.EqualFold(g.Type, "selector") {
		return nil, false
	}
	var nodes []string
	for _, n := range g.All {
		if n == "FALLBACK-LOCAL" {
			continue
		}
		p, exists := s.Proxies[n]
		if !exists || p.Type == "" || len(p.All) != 0 {
			continue
		}
		switch strings.ToLower(p.Type) {
		case "selector", "urltest", "fallback", "loadbalance", "relay", "smart", "direct", "reject", "rejectdrop", "pass", "compatible", "dns":
			continue
		}
		nodes = append(nodes, n)
	}
	return nodes, true
}

type nodeGroupStatus struct {
	RecoveryStatus string   `json:"recovery_status,omitempty"`
	RecoveryReason string   `json:"recovery_reason,omitempty"`
	Group          string   `json:"group"`
	Mode           string   `json:"mode"`
	Selected       string   `json:"selected"`
	ManualNode     string   `json:"manual_node"`
	Nodes          []string `json:"nodes"`
	DesiredMode    string   `json:"desired_mode"`
	Warning        string   `json:"warning,omitempty"`
	// Source attribution of the selected and manually pinned nodes. Manual
	// pinning may cross sources; the API still reports which source each node
	// belongs to. Fields are omitted when attribution is unavailable.
	SelectedSource        string `json:"selected_source,omitempty"`
	SelectedSourceLabel   string `json:"selected_source_label,omitempty"`
	ManualNodeSource      string `json:"manual_node_source,omitempty"`
	ManualNodeSourceLabel string `json:"manual_node_source_label,omitempty"`
}

func groupStatus(s proxySnapshot, prefs map[string]string, group string) (nodeGroupStatus, bool) {
	return groupStatusWithSources(s, prefs, group, sourceIndex{})
}

// groupStatusWithSources adds source attribution for the selected and manually
// pinned nodes. An unbound index (missing or stale labels) adds nothing.
func groupStatusWithSources(s proxySnapshot, prefs map[string]string, group string, idx sourceIndex) (nodeGroupStatus, bool) {
	nodes, ok := selectableNodes(s, group)
	if !ok {
		return nodeGroupStatus{}, false
	}
	if nodes == nil {
		nodes = []string{}
	}
	result := nodeGroupStatus{Group: group, Mode: "auto", DesiredMode: "auto", Selected: s.Proxies[group].Now, ManualNode: prefs[group], Nodes: nodes}
	if prefs[group] != "" {
		result.DesiredMode = "manual"
		if !containsNode(nodes, prefs[group]) {
			result.Warning = "manual_node_unavailable"
		} else if result.Selected != prefs[group] {
			result.Warning = "manual_node_not_active"
		} else {
			result.Mode = "manual"
		}
	}
	if idx.bound {
		result.SelectedSource, result.SelectedSourceLabel = sourceDisplay(idx, result.Selected)
		if result.ManualNode != "" {
			result.ManualNodeSource, result.ManualNodeSourceLabel = sourceDisplay(idx, result.ManualNode)
		}
	}
	return result, true
}
func containsNode(nodes []string, name string) bool {
	for _, n := range nodes {
		if n == name {
			return true
		}
	}
	return false
}

// Source manifest and attribution.
//
// config.json carries "sources" (array order is the selection priority, ids
// are stable) and "base_rules_source". node-labels.json maps every generated
// node name to its source id and carries "_generation" (first 16 hex chars of
// the engine config SHA-256) plus "_sources" (id -> label). Only the fields
// below are interpreted here; the rest of config.json stays owned by
// config.go.

const (
	sourceStatusOK            = "ok"
	sourceStatusNoSources     = "no-sources"
	sourceStatusLabelsMissing = "labels-missing"
	sourceStatusLabelsStale   = "labels-stale"
	sourceStatusEngineMissing = "engine-config-missing"

	// The engine fallback node and the local fallback port both terminate on
	// the office91 VPN port; per-source traffic reports them as one dedicated
	// row that is never merged into a source's node row.
	sourceOffice91    = "office91"
	sourcePortRowID   = "office91-port"
	sourcePortNote    = "（端口兜底）"
	sourceUnknown     = "unknown"
	defaultSourcePort = uint16(7890)

	nodeLabelsMaxFile = 8 << 20
)

// sourceDef mirrors one config.json "sources" entry.
type sourceDef struct {
	ID                string   `json:"id"`
	Label             string   `json:"label"`
	Type              string   `json:"type"` // profile | subscription | vpn
	Enabled           bool     `json:"enabled"`
	DefaultPriority   string   `json:"default_priority,omitempty"` // high | medium | low
	DowngradePatterns []string `json:"downgrade_patterns"`         // nil uses built-in rules; [] clears them
	IncludeKeywords   []string `json:"include_keywords"`
	ExcludeKeywords   []string `json:"exclude_keywords"`
}

// sourcePriorityDefaults fills policy fields a source entry does not carry.
// 玖壹 starts at medium; other sources start at high. Its built-in
// 家宽 / x20 rules lower matching nodes by one tier.
func (d sourceDef) sourcePriorityDefaults() (string, []string) {
	dp := d.DefaultPriority
	if !validPriorityLevel(dp) {
		if d.ID == sourceOffice91 {
			dp = "medium"
		} else {
			dp = "high"
		}
	}
	pats := d.DowngradePatterns
	if d.ID == sourceOffice91 && pats == nil {
		pats = []string{"家宽", "x20"}
	}
	return dp, pats
}

type nodeSourcesDoc struct {
	Sources         []sourceDef `json:"sources"`
	BaseRulesSource string      `json:"base_rules_source"`
	FallbackAddr    string      `json:"fallback_addr"`
}

// loadNodeSources reports ok=false when config.json has no usable sources
// manifest; callers then keep the legacy whole-pool behavior.
func loadNodeSources() (nodeSourcesDoc, bool) {
	var doc nodeSourcesDoc
	b, err := os.ReadFile(configPath())
	if err != nil {
		return doc, false
	}
	if json.Unmarshal(b, &doc) != nil || len(doc.Sources) == 0 {
		return doc, false
	}
	seen := map[string]bool{}
	for _, s := range doc.Sources {
		if s.ID == "" || seen[s.ID] {
			return doc, false
		}
		seen[s.ID] = true
	}
	return doc, true
}

// nodeLabelsDoc is the generated node-labels.json document.
type nodeLabelsDoc struct {
	Generation string                    `json:"_generation"`
	ConfigSHA  string                    `json:"config_sha256"` // legacy binding
	Sources    map[string]string         `json:"_sources"`
	Nodes      map[string]nodeLabelEntry `json:"nodes"`
}

type nodeLabelEntry struct {
	Profile  string `json:"profile"`
	Name     string `json:"name"`
	SourceID string `json:"source_id"`
}

func nodeLabelsPath() string { return filepath.Join(appSupportDir(), "node-labels.json") }

func loadNodeLabels() (nodeLabelsDoc, bool) {
	var doc nodeLabelsDoc
	b, err := os.ReadFile(nodeLabelsPath())
	if err != nil || len(b) > nodeLabelsMaxFile {
		return doc, false
	}
	if json.Unmarshal(b, &doc) != nil || doc.Nodes == nil {
		return doc, false
	}
	return doc, true
}

// engineConfigGeneration is the first 16 hex characters of the running engine
// config's SHA-256; node-labels.json records the same value for the config
// that produced its node names.
func engineConfigGeneration() string {
	b, err := os.ReadFile(engineConfigPath())
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

func labelsMatchGeneration(doc nodeLabelsDoc, gen string) bool {
	if len(gen) != 16 {
		return false
	}
	if doc.Generation != "" {
		return len(doc.Generation) == 16 && doc.Generation == gen
	}
	return len(doc.ConfigSHA) >= 16 && doc.ConfigSHA[:16] == gen
}

// sourceIndex binds configured sources, their priority and per-node
// attribution. nodeSource is only populated when node-labels.json is bound to
// the current engine config; missing or stale mappings are never guessed.
type sourceIndex struct {
	defs         []sourceDef
	baseRules    string
	fallbackPort uint16
	labels       map[string]string
	nodeSource   map[string]string
	nodeOriginal map[string]string
	// expiry holds each source's subscription expiry (unix seconds) so an
	// imminently expiring subscription can be used up before it is wasted.
	expiry map[string]int64
	bound  bool
	reason string
}

// defaultPriority is the per-source tier a node starts at when the user has
// not overridden it.
func (i sourceIndex) defaultPriority(id string) string {
	for _, d := range i.defs {
		if d.ID == id {
			dp, _ := d.sourcePriorityDefaults()
			return dp
		}
	}
	return "high"
}

// downgradePatterns are name substrings that drop a node one tier below its
// source default (floor: low).
func (i sourceIndex) downgradePatterns(id string) []string {
	for _, d := range i.defs {
		if d.ID == id {
			_, pats := d.sourcePriorityDefaults()
			return pats
		}
	}
	return nil
}

// originalName is the pre-aggregation node name from the bound labels.
func (i sourceIndex) originalName(node string) string {
	if i.nodeOriginal == nil {
		return ""
	}
	return i.nodeOriginal[node]
}

func (i sourceIndex) sourceID(node string) string {
	if !i.bound {
		return ""
	}
	return i.nodeSource[node]
}

func (i sourceIndex) enabled(id string) bool {
	for _, d := range i.defs {
		if d.ID == id {
			return d.Enabled
		}
	}
	return false
}

func (i sourceIndex) label(id string) string {
	if l := i.labels[id]; l != "" {
		return l
	}
	for _, d := range i.defs {
		if d.ID == id {
			return d.Label
		}
	}
	return ""
}

// enabledOrder returns enabled source ids in priority order.
func (i sourceIndex) enabledOrder() []string {
	out := make([]string, 0, len(i.defs))
	for _, d := range i.defs {
		if d.Enabled {
			out = append(out, d.ID)
		}
	}
	return out
}

// selectionActive marks priority selection as in effect: labels are bound to
// the engine config and node attribution can be trusted.
func (i sourceIndex) selectionActive() bool { return i.bound }

// priorityActive additionally requires at least one enabled source.
func (i sourceIndex) priorityActive() bool { return i.bound && len(i.enabledOrder()) > 0 }

func loadSourceIndex() sourceIndex {
	idx := sourceIndex{reason: sourceStatusNoSources, fallbackPort: defaultSourcePort, expiry: readSourceExpiryCache()}
	doc, ok := loadNodeSources()
	if !ok {
		return idx
	}
	idx.defs = doc.Sources
	idx.baseRules = doc.BaseRulesSource
	if p, _, err := net.SplitHostPort(doc.FallbackAddr); err == nil {
		if n, err := strconv.ParseUint(p, 10, 16); err == nil && n != 0 {
			idx.fallbackPort = uint16(n)
		}
	}
	idx.reason = sourceStatusLabelsMissing
	labels, ok := loadNodeLabels()
	if !ok {
		return idx
	}
	idx.labels = labels.Sources
	gen := engineConfigGeneration()
	if gen == "" {
		idx.reason = sourceStatusEngineMissing
		return idx
	}
	if !labelsMatchGeneration(labels, gen) {
		idx.reason = sourceStatusLabelsStale
		return idx
	}
	idx.bound = true
	idx.reason = sourceStatusOK
	known := make(map[string]bool, len(idx.defs))
	for _, d := range idx.defs {
		known[d.ID] = true
	}
	idx.nodeSource = make(map[string]string, len(labels.Nodes))
	idx.nodeOriginal = make(map[string]string, len(labels.Nodes))
	for name, entry := range labels.Nodes {
		if entry.SourceID != "" && known[entry.SourceID] {
			idx.nodeSource[name] = entry.SourceID
		}
		if entry.Name != "" {
			idx.nodeOriginal[name] = entry.Name
		}
	}
	return idx
}

// sourceDisplay reports the (id, label) shown for a node; "unknown" is
// explicit because an unattributed node is never guessed into a source.
func sourceDisplay(idx sourceIndex, node string) (string, string) {
	if !idx.bound {
		return "", ""
	}
	if id := idx.sourceID(node); id != "" {
		if label := idx.label(id); label != "" {
			return id, label
		}
		return id, id
	}
	return sourceUnknown, sourceUnknown
}

// sourceUpdateEntry is one row of a PUT /api/sources body.
type sourceUpdateEntry struct {
	ID                string    `json:"id"`
	Enabled           *bool     `json:"enabled"`
	DefaultPriority   *string   `json:"default_priority,omitempty"`
	DowngradePatterns *[]string `json:"downgrade_patterns,omitempty"`
	IncludeKeywords   *[]string `json:"include_keywords,omitempty"`
	ExcludeKeywords   *[]string `json:"exclude_keywords,omitempty"`
}

var errSourcesUpdateRejected = errors.New("sources update rejected")

// persistSourcesOrder rewrites config.json with a new source order and enabled
// flags. Every other top-level field (base_rules_source, source_profiles and
// anything else) is preserved. Validation failures wrap
// errSourcesUpdateRejected; the caller maps those to HTTP 400.
func persistSourcesOrder(path string, updates []sourceUpdateEntry) error {
	reject := func(reason string) error {
		return fmt.Errorf("%w: %s", errSourcesUpdateRejected, reason)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(b, &doc) != nil || len(doc) == 0 {
		return reject("invalid config document")
	}
	raw, ok := doc["sources"]
	if !ok {
		return reject("config has no sources")
	}
	var entries []map[string]json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return reject("invalid config sources")
	}
	byID := make(map[string]map[string]json.RawMessage, len(entries))
	for _, e := range entries {
		var id string
		if v, ok := e["id"]; !ok || json.Unmarshal(v, &id) != nil || id == "" {
			return reject("invalid config source entry")
		}
		if _, dup := byID[id]; dup {
			return reject("duplicate config source")
		}
		byID[id] = e
	}
	if len(updates) != len(byID) {
		return reject("source set mismatch")
	}
	next := make([]map[string]json.RawMessage, 0, len(updates))
	seen := make(map[string]bool, len(updates))
	for _, u := range updates {
		if u.ID == "" || u.Enabled == nil {
			return reject("invalid source update")
		}
		if seen[u.ID] {
			return reject("duplicate source update")
		}
		seen[u.ID] = true
		e, ok := byID[u.ID]
		if !ok {
			return reject(fmt.Sprintf("unknown source %q", u.ID))
		}
		enabled, err := json.Marshal(*u.Enabled)
		if err != nil {
			return err
		}
		e["enabled"] = enabled
		if u.DefaultPriority != nil {
			dp := strings.TrimSpace(*u.DefaultPriority)
			if dp == "" {
				delete(e, "default_priority") // back to the source's built-in default
			} else {
				if !validPriorityLevel(dp) {
					return reject(fmt.Sprintf("invalid default_priority for %q", u.ID))
				}
				ep, err := json.Marshal(dp)
				if err != nil {
					return err
				}
				e["default_priority"] = ep
			}
		}
		if u.DowngradePatterns != nil {
			patterns := *u.DowngradePatterns
			for _, pattern := range patterns {
				if strings.TrimSpace(pattern) == "" {
					return reject("empty downgrade pattern")
				}
			}
			encoded, err := json.Marshal(patterns)
			if err != nil {
				return err
			}
			e["downgrade_patterns"] = encoded
		}
		for key, values := range map[string]*[]string{"include_keywords": u.IncludeKeywords, "exclude_keywords": u.ExcludeKeywords} {
			if values == nil {
				continue
			}
			clean := make([]string, 0, len(*values))
			if len(*values) > 100 {
				return reject("too many keywords")
			}
			for _, value := range *values {
				value = strings.TrimSpace(value)
				if value == "" || len(value) > 256 {
					return reject("invalid keyword")
				}
				clean = append(clean, value)
			}
			encoded, err := json.Marshal(clean)
			if err != nil {
				return err
			}
			e[key] = encoded
		}
		next = append(next, e)
	}
	arr, err := json.Marshal(next)
	if err != nil {
		return err
	}
	doc["sources"] = arr
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".config-sources-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(append(out, '\n'))
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// An unavailable preference is retained for visibility, but does not lock automatic selection.
func (a *App) restoreManualNodesLocked() error {
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
		if snap.Proxies[group].Now != node {
			if err := p.selectNode(ctx, group, node); err != nil {
				return err
			}
		}
	}
	return nil
}

// selectionFailure makes a best-effort rollback with an independent deadline. A
// failed/timeout PUT can have been applied, so neither response alone proves state.
func (a *App) selectionFailure(w http.ResponseWriter, p *nodeProber, group, previous, reason string) {
	ctx := context.Background()
	rollback := "not_attempted"
	if previous != "" {
		if err := p.selectNode(ctx, group, previous); err != nil {
			rollback = "failed"
		} else {
			rollback = "requested"
		}
	}
	actual := ""
	known := false
	if snap, err := p.snapshot(ctx); err == nil {
		if g, ok := snap.Proxies[group]; ok && strings.EqualFold(g.Type, "selector") && g.Now != "" {
			known, actual = true, g.Now
			if actual == previous {
				rollback = "confirmed"
			}
		}
	}
	select {
	case a.nodeScanWake <- struct{}{}:
	default:
	}
	w.WriteHeader(http.StatusBadGateway)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": reason, "preference_changed": false, "rollback": rollback, "actual_known": known, "selected": actual, "warning": "controller state may change after a timed-out request; read back again"})
}

// Caller holds engineMu. The controller mode was read before the selector write;
// no node or egress probe is needed to honor a manual selection.
func (a *App) recoverManualSelectionLocked(group, controllerMode string, snap proxySnapshot) (string, string) {
	a.routingGate.Lock()
	defer a.routingGate.Unlock()
	mode, err := readRoutingMode()
	if err != nil || mode == "direct" {
		return "not_attempted", "routing preference unavailable or direct"
	}
	if controllerMode == "" || controllerMode != mode {
		return "not_attempted", "controller routing mode unavailable or differs from preference"
	}
	if group == "AI主代理" && mode == "global" {
		return "not_attempted", "AI selector is not used in global mode"
	}
	if mode == "global" && snap.Proxies["GLOBAL"].Now != "主代理" {
		return "not_attempted", "global selector is not using the primary group"
	}
	current, _ := a.snapshotMode()
	if current != ModeFallback {
		return "not_needed", "outer fallback is not active"
	}
	if a.shuttingDown.Load() {
		return "not_attempted", "shutting down"
	}
	// Invalidate even routing-only in-flight fallback decisions, without waiting
	// for transitionMu (which can be held during a slow network probe). Existing
	// streams are not canceled or closed.
	a.routingGeneration++
	a.mu.Lock()
	a.primaryFails = 0
	a.mu.Unlock()
	a.setMode(ModePrimary, "manual selector confirmed; actual path unverified")
	return "switched_unverified_path", "selector confirmed; node health, actual path and destination reachability are not verified"
}

func (a *App) nodeAPIHandler() (http.Handler, error) {
	host, port, err := net.SplitHostPort(a.cfg.HTTPListen)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("node API requires literal loopback HTTP listen address")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(raw)
	path := filepath.Join(appSupportDir(), "api-token")
	f, err := os.CreateTemp(filepath.Dir(path), ".api-token-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.WriteString(token + "\n")
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return nil, err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return nil, err
	}
	subscriptions := newSubscriptionCache(a.cfg)
	// Populate the expiry cache once at startup so the near-expiry priority
	// boost works before anyone opens the subscription panel.
	go func() {
		if subscriptions.loading {
			return
		}
		subscriptions.loading = true
		subscriptions.refresh()
	}()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.IsAbs() || (r.URL.Path != "/api/nodes" && r.URL.Path != "/api/node-priorities" && r.URL.Path != "/api/blocked-domains" && r.URL.Path != "/api/routing-mode" && r.URL.Path != "/api/subscriptions" && r.URL.Path != "/api/fallback" && r.URL.Path != "/api/route-traffic" && r.URL.Path != "/api/strategy" && r.URL.Path != "/api/source-subscription") {
			a.ServeHTTP(w, r)
			return
		}
		h, hp, e := net.SplitHostPort(r.Host)
		remote, _, re := net.SplitHostPort(r.RemoteAddr)
		rip := net.ParseIP(remote)
		if e != nil || h != host || hp != port || re != nil || rip == nil || !rip.IsLoopback() || r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		auth := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(auth), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/node-priorities" {
			a.serveNodePriorities(w, r)
		} else if r.URL.Path == "/api/subscriptions" {
			subscriptions.serveHTTP(w, r)
		} else if r.URL.Path == "/api/fallback" {
			a.serveFallback(w, r)
		} else if r.URL.Path == "/api/route-traffic" {
			a.serveRouteTraffic(w, r)
		} else if r.URL.Path == "/api/source-subscription" {
			a.serveSourceSubscription(w, r)
		} else if r.URL.Path == "/api/strategy" {
			a.serveStrategy(w, r)
		} else if r.URL.Path == "/api/blocked-domains" {
			a.serveBlockedDomains(w, r)
		} else if r.URL.Path == "/api/routing-mode" {
			a.serveRoutingMode(w, r)
		} else {
			a.serveNodeSelection(w, r)
		}
	}), nil
}
func (a *App) serveNodeSelection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Group string `json:"group"`
		Mode  string `json:"mode"`
		Node  string `json:"node"`
	}
	if r.Method == http.MethodPut {
		if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
			http.Error(w, "JSON required", 415)
			return
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		dec.DisallowUnknownFields()
		if dec.Decode(&req) != nil || dec.Decode(&struct{}{}) != io.EOF || !validNodeGroup(req.Group) || (req.Mode != "auto" && req.Mode != "manual") || (req.Mode == "manual" && req.Node == "") || (req.Mode == "auto" && req.Node != "") {
			http.Error(w, "invalid request", 400)
			return
		}
	}
	a.engineMu.Lock()
	locked := true
	defer func() {
		if locked {
			a.engineMu.Unlock()
		}
	}()
	if a.shuttingDown.Load() {
		http.Error(w, "shutting down", 503)
		return
	}
	prefs, err := readManualNodes()
	if err != nil {
		http.Error(w, "preference read failed", 500)
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
	if r.Method == http.MethodGet {
		idx := loadSourceIndex()
		groups := []nodeGroupStatus{}
		for _, name := range []string{"主代理", "AI主代理"} {
			if g, ok := groupStatusWithSources(snap, prefs, name, idx); ok {
				groups = append(groups, g)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"groups": groups})
		return
	}
	group, ok := groupStatus(snap, prefs, req.Group)
	if !ok {
		http.Error(w, "selector unavailable", 409)
		return
	}
	if req.Mode == "manual" && !containsNode(group.Nodes, req.Node) {
		http.Error(w, "node is not a selectable member", 400)
		return
	}
	if req.Mode == "manual" {
		priorities, e := readNodePriorities()
		if e != nil {
			http.Error(w, "priority read failed", 500)
			return
		}
		if nodeExcluded(priorities, req.Node) {
			http.Error(w, "node is disabled", 409)
			return
		}
	}
	// Invalidate in-flight scans before any controller write, even on failure.
	a.advanceEngineGenerationLocked()
	// In-flight request retries must not fall through to the old route after a
	// selector change; established streams are not canceled.
	a.routingGate.Lock()
	a.routingGeneration++
	a.routingGate.Unlock()
	// The main selector is the global-mode AI path, so either group may
	// change the route for which AI health was measured.
	a.invalidateAIHealth()
	_ = a.persist()
	if req.Mode == "auto" {
		delete(prefs, req.Group)
		if err := saveNodeSelection(manualNodePath(), prefs); err != nil {
			http.Error(w, "preference write failed", 500)
			return
		}
		select {
		case a.nodeScanWake <- struct{}{}:
		default:
		}
		result, _ := groupStatusWithSources(snap, prefs, req.Group, loadSourceIndex())
		_ = json.NewEncoder(w).Encode(result)
		return
	}
	// Read routing mode before committing the selector, so the post-save switch
	// never waits for a health or egress check.
	controllerMode, _ := controllerRoutingMode(r.Context(), p)
	old := group.Selected
	oldRoute := old
	if old != req.Node {
		if err := p.selectNode(r.Context(), req.Group, req.Node); err != nil {
			a.selectionFailure(w, p, req.Group, old, "selector_update_failed")
			return
		}
	}
	// A 204 is not proof of the active route. Read back before committing.
	actual, err := p.snapshot(context.Background())
	actualGroup, valid := groupStatus(actual, map[string]string{req.Group: req.Node}, req.Group)
	if err != nil || !valid || actualGroup.Mode != "manual" {
		a.selectionFailure(w, p, req.Group, old, "selector_readback_failed")
		return
	}
	prefs[req.Group] = req.Node
	if err := saveNodeSelection(manualNodePath(), prefs); err != nil {
		a.selectionFailure(w, p, req.Group, old, "preference_write_failed")
		return
	}
	// A manual handover abandons the previous route on purpose: fast-fail the
	// tunnels still riding it so they reconnect on the newly selected node.
	if oldRoute != "" && oldRoute != req.Node {
		a.purgeEngineTunnels(fmt.Sprintf("manual %s %s -> %s", req.Group, oldRoute, req.Node))
	}
	result, _ := groupStatusWithSources(actual, prefs, req.Group, loadSourceIndex())
	result.RecoveryStatus, result.RecoveryReason = a.recoverManualSelectionLocked(req.Group, controllerMode, actual)
	_ = json.NewEncoder(w).Encode(result)
}
