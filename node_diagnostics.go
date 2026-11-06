package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Each source has its own process-wide limit, shared by periodic and manual
// probers. Unattributed nodes share one conservative bucket.
type nodeProbeBucket struct {
	slots chan struct{}
	refs  int // holders and waiters
}

var nodeProbeLimiter = struct {
	sync.Mutex
	buckets map[string]*nodeProbeBucket
}{buckets: make(map[string]*nodeProbeBucket)}

func acquireSourceProbe(ctx context.Context, source string) (func(), bool) {
	if ctx.Err() != nil {
		return func() {}, false
	}
	nodeProbeLimiter.Lock()
	bucket := nodeProbeLimiter.buckets[source]
	if bucket == nil {
		bucket = &nodeProbeBucket{slots: make(chan struct{}, nodeProbeConcurrency)}
		nodeProbeLimiter.buckets[source] = bucket
	}
	bucket.refs++
	nodeProbeLimiter.Unlock()
	drop := func() {
		nodeProbeLimiter.Lock()
		bucket.refs--
		if bucket.refs == 0 {
			delete(nodeProbeLimiter.buckets, source)
		}
		nodeProbeLimiter.Unlock()
	}
	select {
	case bucket.slots <- struct{}{}:
		if ctx.Err() != nil {
			<-bucket.slots
			drop()
			return func() {}, false
		}
		var once sync.Once
		return func() { once.Do(func() { <-bucket.slots; drop() }) }, true
	case <-ctx.Done():
		drop()
		return func() {}, false
	}
}

var nodeProbeNames = struct {
	sync.Mutex
	entries map[string]*nodeProbeBucket
}{entries: make(map[string]*nodeProbeBucket)}

func (p *nodeProber) acquireNodeProbe(ctx context.Context, name, kind string) (func(), bool) {
	if ctx.Err() != nil {
		return func() {}, false
	}
	key := name + "\x00" + kind
	nodeProbeNames.Lock()
	entry := nodeProbeNames.entries[key]
	if entry == nil {
		entry = &nodeProbeBucket{slots: make(chan struct{}, 1)}
		nodeProbeNames.entries[key] = entry
	}
	entry.refs++
	nodeProbeNames.Unlock()
	drop := func() {
		nodeProbeNames.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(nodeProbeNames.entries, key)
		}
		nodeProbeNames.Unlock()
	}
	select {
	case entry.slots <- struct{}{}:
	case <-ctx.Done():
		drop()
		return func() {}, false
	}
	releaseSource, ok := acquireSourceProbe(ctx, loadSourceIndex().sourceID(name))
	if !ok {
		<-entry.slots
		drop()
		return func() {}, false
	}
	var once sync.Once
	return func() { once.Do(func() { releaseSource(); <-entry.slots; drop() }) }, true
}

// Dispatch independent source queues so a saturated source cannot occupy the
// workers needed by another source. The number of queued names is bounded by
// the controller snapshot; at most three workers are created per source.
func forEachProbeNode(ctx context.Context, names []string, idx sourceIndex, run func(string)) {
	groups := make(map[string][]string)
	for _, name := range names {
		id := idx.sourceID(name)
		groups[id] = append(groups[id], name)
	}
	var wg sync.WaitGroup
	for _, group := range groups {
		jobs := make(chan string)
		for i := 0; i < nodeProbeConcurrency && i < len(group); i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for name := range jobs {
					if ctx.Err() != nil {
						return
					}
					run(name)
				}
			}()
		}
		wg.Add(1)
		go func(group []string) {
			defer wg.Done()
			defer close(jobs)
			for _, name := range group {
				select {
				case jobs <- name:
				case <-ctx.Done():
					return
				}
			}
		}(group)
	}
	wg.Wait()
}

var errNodeDiagnosticReload = errors.New("engine configuration changed during probe")

func authorizeNodeDiagnostics(r *http.Request) bool {
	b, err := os.ReadFile(filepath.Join(appSupportDir(), "api-token"))
	token := strings.TrimSpace(string(b))
	return err == nil && len(token) == 64 && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) == 1
}

// nodeDiagnostics holds only in-memory results; node names are never persisted here.
type nodeDiagnostics struct {
	mu                    sync.Mutex
	app                   *App
	traffic               *nodeTrafficService
	fallback              *fallbackProbe
	results               map[string]map[string]nodeDiagnosticResult
	running               bool
	startedAt, finishedAt time.Time
	err                   string
	// scan progress, updated as each node finishes its probes
	done, total int
}
type nodeDiagnosticResult struct {
	Status    string    `json:"status"`
	DelayMS   int64     `json:"delay_ms,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Stale     bool      `json:"stale"`
	Error     string    `json:"error,omitempty"`
}
type nodeDiagnosticRow struct {
	Name    string                          `json:"name"`
	Normal  *nodeDiagnosticResult           `json:"normal,omitempty"`
	AI      *nodeDiagnosticResult           `json:"ai,omitempty"`
	ChatGPT *nodeDiagnosticResult           `json:"chatgpt,omitempty"`
	Targets map[string]nodeDiagnosticResult `json:"targets,omitempty"`
}

func newNodeDiagnostics() *nodeDiagnostics {
	return &nodeDiagnostics{results: make(map[string]map[string]nodeDiagnosticResult)}
}
func (d *nodeDiagnostics) bind(a *App) {
	d.mu.Lock()
	d.app = a
	d.mu.Unlock()
	d.startFallbackProbe(a)
	if d.traffic != nil {
		if err := d.traffic.start(a); err != nil && d.traffic.logf != nil {
			d.traffic.logf("node traffic sampling unavailable")
		}
	}
}

// The caller holds engineMu. All generation transitions invalidate published data.
func (a *App) advanceEngineGenerationLocked() {
	a.engineGeneration++
	a.invalidateAIHealth()
	if a.stats != nil && a.stats.diagnostics != nil {
		d := a.stats.diagnostics
		d.mu.Lock()
		d.results = make(map[string]map[string]nodeDiagnosticResult)
		d.mu.Unlock()
	}
}

func (p *nodeProber) publishNodeProbe(generation uint64, name, kind string, v probeResult) {
	if p.app != nil {
		p.app.engineMu.Lock()
		defer p.app.engineMu.Unlock()
	}
	p.publishNodeProbeLocked(generation, name, kind, v)
}

// Caller holds engineMu when p.app is present, including the periodic re-probe.
func (p *nodeProber) publishNodeProbeLocked(generation uint64, name, kind string, v probeResult) {
	if p.app != nil && p.app.engineGeneration != generation {
		return
	}
	p.recordNodeProbe(name, kind, v)
}

// An older completion must not overwrite a newer measurement. Canceled probes
// do not have a timestamp and are not published.
func (p *nodeProber) recordNodeProbe(name, kind string, v probeResult) {
	if p.stats == nil || p.stats.diagnostics == nil || v.at.IsZero() {
		return
	}
	d := p.stats.diagnostics
	result := nodeDiagnosticResult{Status: "ok", DelayMS: v.delay, CheckedAt: v.at.UTC(), ExpiresAt: v.at.Add(nodeResultMaxAge).UTC()}
	if v.failed {
		result.Status = "failed"
		result.Error = "node probe failed"
	}
	if v.uncertain {
		result.Status = "error"
		result.Error = "controller probe error"
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.results[name] == nil {
		d.results[name] = make(map[string]nodeDiagnosticResult)
	}
	if old, ok := d.results[name][kind]; !ok || !old.CheckedAt.After(result.CheckedAt) {
		d.results[name][kind] = result
	}
}

// serveNodeDiagnostics must only be mounted behind the authenticated local API.
func (s *statsIntegration) serveNodeDiagnostics(w http.ResponseWriter, r *http.Request) {
	d := s.diagnostics
	if d == nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/api/node-health" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", 405)
			return
		}
		d.mu.Lock()
		rows := make([]nodeDiagnosticRow, 0, len(d.results))
		now := time.Now()
		for name, kinds := range d.results {
			row := nodeDiagnosticRow{Name: name}
			if v, ok := kinds["gstatic"]; ok {
				v.Stale = !now.Before(v.ExpiresAt)
				row.Normal = &v
			}
			if v, ok := kinds["ai"]; ok {
				v.Stale = !now.Before(v.ExpiresAt)
				row.AI = &v
			}
			if v, ok := kinds["chatgpt"]; ok {
				v.Stale = !now.Before(v.ExpiresAt)
				row.ChatGPT = &v
			}
			// Per-provider evidence: the node table renders one column per
			// configured target, so each published target key must reach the UI.
			for _, t := range aiTargetOrder {
				v, ok := kinds[t]
				if !ok {
					continue
				}
				v.Stale = !now.Before(v.ExpiresAt)
				if row.Targets == nil {
					row.Targets = make(map[string]nodeDiagnosticResult, len(aiTargetOrder))
				}
				row.Targets[t] = v
			}
			rows = append(rows, row)
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
		fallback := fallbackProbeResult{Status: "pending"}
		if d.fallback != nil {
			fallback = d.fallback.snapshot(now)
		}
		payload := struct {
			FallbackProbe fallbackProbeResult `json:"fallback_probe"`
			Nodes         []nodeDiagnosticRow `json:"nodes"`
			Running       bool                `json:"running"`
			StartedAt     time.Time           `json:"started_at"`
			FinishedAt    time.Time           `json:"finished_at"`
			Error         string              `json:"error,omitempty"`
			ProgressDone  int                 `json:"progress_done"`
			ProgressTotal int                 `json:"progress_total"`
		}{fallback, rows, d.running, d.startedAt, d.finishedAt, d.err, d.done, d.total}
		d.mu.Unlock()
		_ = json.NewEncoder(w).Encode(payload)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", 405)
		return
	}
	d.mu.Lock()
	if d.app == nil {
		d.mu.Unlock()
		http.Error(w, "unavailable", 503)
		return
	}
	if d.running {
		d.mu.Unlock()
		http.Error(w, "probe already running", 409)
		return
	}
	d.running = true
	d.startedAt = time.Now().UTC()
	d.finishedAt = time.Time{}
	d.err = ""
	d.done, d.total = 0, 0
	a := d.app
	d.mu.Unlock()
	go d.scan(a)
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "running"})
}

func (d *nodeDiagnostics) scan(a *App) {
	var scanErr error
	defer func() {
		d.mu.Lock()
		d.running = false
		d.finishedAt = time.Now().UTC()
		if scanErr != nil {
			d.err = scanErr.Error()
		}
		d.mu.Unlock()
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-a.stopCh:
			cancel()
		case <-done:
		}
	}()
	p, err := newNodeProber(a.cfg.Controller, engineSecret(), a.cfg.ProbeURL, time.Duration(a.cfg.ProbeTimeoutSec)*time.Second, a.stats)
	if err != nil {
		scanErr = err
		return
	}
	defer p.client.CloseIdleConnections()
	p.app = a
	ai, err := url.Parse(a.cfg.AIProbeURL)
	if err == nil && (ai.Scheme == "http" || ai.Scheme == "https") && ai.Host != "" && ai.User == nil && ai.Fragment == "" {
		p.aiURL = a.cfg.AIProbeURL
	}
	aiSet := a.cfg.aiProbeSet()
	a.engineMu.Lock()
	generation := a.engineGeneration
	names, err := p.nodes(ctx)
	a.engineMu.Unlock()
	if err != nil {
		scanErr = err
		return
	}
	idx := loadSourceIndex()
	d.mu.Lock()
	d.total = len(names)
	d.done = 0
	d.mu.Unlock()
	forEachProbeNode(ctx, names, idx, func(name string) {
		defer func() {
			d.mu.Lock()
			d.done++
			d.mu.Unlock()
		}()
		ordinary := []struct {
			kind, target string
			expected     int
		}{{"gstatic", p.probeURL, nodeProbeExpectedCode(p.probeURL)}}
		for _, j := range ordinary {
			if j.target == "" || ctx.Err() != nil {
				continue
			}
			result := p.probeGeneration(ctx, name, j.kind, j.target, j.expected, generation, true, false)
			p.publishProbe(generation, name, j.kind, result)
		}
		for target, checks := range aiSet {
			if ctx.Err() != nil {
				break
			}
			for _, check := range checks {
				for i, attempt := range check {
					final := p.probeGeneration(ctx, name, target, attempt.URL, attempt.Expected, generation, true, false)
					p.publishProbe(generation, name, target, final)
					if !final.failed && !final.uncertain && final.delay > 0 {
						break
					}
					if i == len(check)-1 {
						break
					}
				}
			}
		}
	})
	if ctx.Err() != nil {
		scanErr = ctx.Err()
	}
	a.engineMu.Lock()
	changed := generation != a.engineGeneration
	a.engineMu.Unlock()
	if changed {
		scanErr = errNodeDiagnosticReload
	}
}
