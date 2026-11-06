package main

// Runtime wiring for subscription-type sources (玖壹 / office91) and
// profile-type sources (FlClash).
//
// The refresher in source_subscription.go owns 玖壹 URL resolution, fetching
// and parsing; this file runs it inside the daemon, records its status for the
// /api/sources view, and regenerates + reloads the engine config when a cache
// is actually updated.
//
// profileRefresher reads subscription metadata without modifying FlClash's
// profile files. The generator continues to consume those files as maintained
// by FlClash.
//
// A missing or unreachable subscription must never block startup or other
// sources: the first fetch happens asynchronously, and only a successful cache
// write triggers a reload. When a profile cache is replaced, the engine
// watcher also notices the file change on its own 60s poll; the shared updates
// channel and regenerateDebounce absorb the burst into one reload.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// regenerateDebounce absorbs bursts of cache writes (for example a subscription
// refresh racing with a manual reload) into a single engine reload.
const regenerateDebounce = 2 * time.Second

var subscriptionStatusMu sync.Mutex
var subscriptionStatuses = map[string]SubscriptionStatus{}

// Offline status is separate from late diagnostics of a previously online fetch.
var office91OfflineStatuses = map[string]SubscriptionStatus{}
var refreshRequests = map[string]chan struct{}{}

type subscriptionRegistration struct {
	src     SourceConfig
	refresh bool
	ack     chan bool
}

func requestSourceRefresh(id string) bool {
	subscriptionStatusMu.Lock()
	defer subscriptionStatusMu.Unlock()
	ch := refreshRequests[id]
	if ch == nil {
		return false
	}
	select {
	case ch <- struct{}{}:
	default:
	}
	return true
}

// sourceRefreshAvailable reports whether a live refresher is registered for the
// source. Used to size an update batch before waking it.
func sourceRefreshAvailable(id string) bool {
	subscriptionStatusMu.Lock()
	defer subscriptionStatusMu.Unlock()
	return refreshRequests[id] != nil
}

// sourceUpdateProgress tracks one user-triggered "update all configs" batch so
// the UI can show a real percentage instead of an indefinite spinner.
type sourceUpdateTracker struct {
	mu      sync.Mutex
	pending map[string]bool
	total   int
	started time.Time
	updated time.Time
}

var sourceUpdateProgress = sourceUpdateTracker{pending: map[string]bool{}}

func beginSourceUpdate(ids []string) {
	sourceUpdateProgress.mu.Lock()
	defer sourceUpdateProgress.mu.Unlock()
	sourceUpdateProgress.pending = make(map[string]bool, len(ids))
	for _, id := range ids {
		sourceUpdateProgress.pending[id] = true
	}
	sourceUpdateProgress.total = len(ids)
	sourceUpdateProgress.started = time.Now()
	sourceUpdateProgress.updated = sourceUpdateProgress.started
}

func completeSourceUpdate(id string) {
	sourceUpdateProgress.mu.Lock()
	defer sourceUpdateProgress.mu.Unlock()
	if _, ok := sourceUpdateProgress.pending[id]; !ok {
		return
	}
	delete(sourceUpdateProgress.pending, id)
	sourceUpdateProgress.updated = time.Now()
}

func sourceUpdateSnapshot() (running bool, total, remaining int, started, updated time.Time) {
	sourceUpdateProgress.mu.Lock()
	defer sourceUpdateProgress.mu.Unlock()
	remaining = len(sourceUpdateProgress.pending)
	return remaining > 0, sourceUpdateProgress.total, remaining, sourceUpdateProgress.started, sourceUpdateProgress.updated
}

func recordSubscriptionStatus(st SubscriptionStatus) {
	if st.SourceID == "" {
		return
	}
	subscriptionStatusMu.Lock()
	subscriptionStatuses[st.SourceID] = st
	subscriptionStatusMu.Unlock()
	completeSourceUpdate(st.SourceID)
}

func recordOffice91OfflineStatus(st SubscriptionStatus) {
	subscriptionStatusMu.Lock()
	office91OfflineStatuses[appSupportDir()] = st
	subscriptionStatuses[st.SourceID] = st
	subscriptionStatusMu.Unlock()
}

// subscriptionStatusSnapshot returns the last observed status for a source id.
func subscriptionStatusSnapshot(id string) (SubscriptionStatus, bool) {
	offline := false
	if id == sourceIDOffice91 {
		active, err := office91OfflineModeActive()
		offline = active || err != nil
	}
	subscriptionStatusMu.Lock()
	defer subscriptionStatusMu.Unlock()
	if offline {
		if st, ok := office91OfflineStatuses[appSupportDir()]; ok {
			return st, true
		}
		return SubscriptionStatus{SourceID: sourceIDOffice91, Status: "unknown"}, true
	}
	st, ok := subscriptionStatuses[id]
	return st, ok
}

// sourceRefresher is the common refresh loop contract shared by the 玖壹 and
// FlClash profile refreshers.
type sourceRefresher interface {
	RefreshOnce(ctx context.Context)
	Snapshot() SubscriptionStatus
	Interval() time.Duration
}

// runSourceRefresher refreshes immediately and then once per interval until
// the context is cancelled, recording each attempt for the /api/sources view.
// Intended to run on its own goroutine.
func runSourceRefresher(ctx context.Context, r sourceRefresher) {
	id := r.Snapshot().SourceID
	ch := make(chan struct{}, 1)
	subscriptionStatusMu.Lock()
	refreshRequests[id] = ch
	if _, ok := subscriptionStatuses[id]; !ok {
		subscriptionStatuses[id] = SubscriptionStatus{SourceID: id, Status: "unknown"}
	}
	subscriptionStatusMu.Unlock()
	defer func() { subscriptionStatusMu.Lock(); delete(refreshRequests, id); subscriptionStatusMu.Unlock() }()
	for {
		r.RefreshOnce(ctx)
		recordSubscriptionStatus(r.Snapshot())
		timer := time.NewTimer(r.Interval())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-ch:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// runSourceSubscriptions refreshes every enabled subscription and profile
// source on its own goroutine and reloads the engine when a cache changes. It
// blocks until ctx is cancelled and all refreshers have stopped.
func (a *App) runSourceSubscriptions(ctx context.Context) {
	if a.cfg.Sources == nil {
		return
	}
	refreshers := newSubscriptionRefreshers(a.cfg)
	profileRefreshers := newProfileRefreshers(a.cfg)
	manualSubscriptions := make(chan subscriptionRegistration, 16)
	a.stats.registerSubscription = func(request context.Context, src SourceConfig, refresh bool) bool {
		if request == nil {
			request = context.Background()
		}
		item := subscriptionRegistration{src: src, refresh: refresh, ack: make(chan bool, 1)}
		select {
		case <-request.Done():
			return false
		case <-ctx.Done():
			return false
		case manualSubscriptions <- item:
		}
		select {
		case <-request.Done():
			return false
		case <-ctx.Done():
			return false
		case ok := <-item.ack:
			return ok
		}
	}
	updates := make(chan struct{}, 1)
	a.stats.sourceApplyMu.Lock()
	a.stats.sourceApply = func(request context.Context, save func() error) error {
		return a.withOffice91SourceTransaction(ctx, request, func(_ context.Context, apply, rollback func() error) error {
			return applySourceConfigChange(configPath(), save, apply, func() error { return runOffice91GeneratorContext(ctx) }, rollback)
		}, runOffice91GeneratorContext, a.reloadEngine)
	}
	a.stats.sourceApplyMu.Unlock()
	onUpdate := func() {
		select {
		case updates <- struct{}{}:
		default:
		}
	}
	// Offline 玖壹 imports are explicit API actions, but a successful cache
	// replacement still needs to enter the same debounced regenerate/reload
	// path as an online source. Offline imports use the synchronous apply hook
	// so an HTTP caller is not told that a cache is active before regeneration
	// and engine reload have succeeded. The hooks are process-local and cleared
	// when this source runtime stops; imports remain safe when the daemon is
	// absent.
	setOffice91OfflineUpdate(onUpdate)
	setOffice91OfflineTransaction(func(requestCtx context.Context, body func(context.Context, func() error, func() error) error) error {
		return a.withOffice91SourceTransaction(ctx, requestCtx, body, runOffice91GeneratorContext, a.reloadEngine)
	})
	defer func() {
		a.stats.sourceApplyMu.Lock()
		a.stats.sourceApply = nil
		a.stats.sourceApplyMu.Unlock()
		a.stats.registerSubscription = nil
		setOffice91OfflineUpdate(nil)
		setOffice91OfflineApply(nil, nil)
		setOffice91OfflineTransaction(nil)
	}()
	for _, r := range refreshers {
		r.OnUpdate = onUpdate
		r.proxyAddrs = subscriptionProxyAddrs(a.cfg)
	}
	for _, r := range profileRefreshers {
		r.OnUpdate = onUpdate
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.subscriptionReloadLoop(ctx, updates)
	}()
	for _, r := range refreshers {
		r := r
		wg.Add(1)
		go func() {
			defer wg.Done()
			runSourceRefresher(ctx, r)
		}()
	}
	for _, r := range profileRefreshers {
		r := r
		wg.Add(1)
		go func() {
			defer wg.Done()
			runSourceRefresher(ctx, r)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		known := map[string]bool{}
		for _, r := range refreshers {
			known[r.sourceID] = true
		}
		for _, r := range profileRefreshers {
			known[r.sourceID] = true
		}
		lastIDs, _ := discoverFlClashIDs(flclashDatabasePath())
		lastURLs, _ := readFlClashProfileURLs(flclashDatabasePath())
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		var children sync.WaitGroup
		defer children.Wait()
		for {
			select {
			case <-ctx.Done():
				return
			case registration := <-manualSubscriptions:
				if registration.src.Type != SourceTypeSubscription || !strings.HasPrefix(registration.src.ID, customSubscriptionSourcePrefix) {
					registration.ack <- false
					continue
				}
				if known[registration.src.ID] {
					if registration.refresh {
						_ = requestSourceRefresh(registration.src.ID)
					}
					registration.ack <- true
					continue
				}
				r := newSubscriptionRefresher(registration.src, subscriptionRefreshInterval(a.cfg))
				r.proxyAddrs = subscriptionProxyAddrs(a.cfg)
				r.OnUpdate = onUpdate
				known[registration.src.ID] = true
				children.Add(1)
				go func() { defer children.Done(); runSourceRefresher(ctx, r) }()
				registration.ack <- true
			case <-ticker.C:
				if ids, err := discoverFlClashIDs(flclashDatabasePath()); err == nil {
					if strings.Join(ids, ",") != strings.Join(lastIDs, ",") {
						// New profile: record it as a source (with its own label and
						// priority slot) before regenerating, so the UI and the node
						// priority policy see it without a restart.
						_, _ = reconcileDiscoveredSources(configPath())
						onUpdate()
						lastIDs = ids
					}
				}
				if urls, err := readFlClashProfileURLs(flclashDatabasePath()); err == nil {
					changed := len(urls) != len(lastURLs)
					for id, u := range urls {
						if lastURLs[id] != u {
							changed = true
						}
					}
					if changed {
						onUpdate()
						lastURLs = urls
					}
				}
				for _, r := range newProfileRefreshers(a.cfg) {
					if known[r.sourceID] {
						continue
					}
					known[r.sourceID] = true
					r.OnUpdate = onUpdate
					children.Add(1)
					go func(r *profileRefresher) { defer children.Done(); runSourceRefresher(ctx, r) }(r)
				}
			}
		}
	}()
	wg.Wait()
}

// Serialize the entire tentative config change with custom subscription cache
// commits: no refresh may observe an uncommitted enabled state.
func applySourceConfigChange(path string, save, apply, regenerate, rollback func() error) error {
	customSubscriptionCommitMu.Lock()
	defer customSubscriptionCommitMu.Unlock()
	previous, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err = save(); err != nil {
		return err
	}
	if err = apply(); err == nil {
		return nil
	}
	if writePrivateFile(path, string(previous)) != nil {
		return errors.New("source-apply-and-restore-failed")
	}
	if regenerate() != nil {
		return errors.New("source-apply-and-regenerate-rollback-failed")
	}
	if rollback() != nil {
		return errors.New("source-apply-and-reload-rollback-failed")
	}
	return errors.New("source-apply-failed")
}

// subscriptionReloadLoop regenerates the node config after a subscription cache
// update and reloads the engine. Errors are logged without URLs or content.
func (a *App) subscriptionReloadLoop(ctx context.Context, updates <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-updates:
			select {
			case <-ctx.Done():
				return
			case <-time.After(regenerateDebounce):
			}
			if a.shuttingDown.Load() {
				return
			}
			a.sourceRegenMu.Lock()
			releaseProcess, err := acquireSourceProcessLock(ctx)
			if err == nil {
				err = ctx.Err()
			}
			if err == nil && a.shuttingDown.Load() {
				err = errors.New("source-runtime-stopping")
			}
			if err == nil {
				err = runOffice91GeneratorContext(ctx)
			}
			if err == nil {
				err = ctx.Err()
			}
			if err == nil && a.shuttingDown.Load() {
				err = errors.New("source-runtime-stopping")
			}
			if err == nil {
				err = a.reloadEngine()
			}
			if releaseProcess != nil {
				releaseProcess()
			}
			a.sourceRegenMu.Unlock()
			if err != nil {
				a.logf("subscription source: regenerate/reload failed: %v", err)
				continue
			}
			a.logf("subscription source cache updated; engine config regenerated and reloaded")
			a.stateMu.Lock()
			a.state.LastReason = "subscription source updated"
			a.stateMu.Unlock()
			a.persist()
		}
	}
}

// profileUserinfo is one profile source's parsed subscription-userinfo header,
// mirrored to srhop's private cache. FetchedAt (unix seconds) is when srhop
// observed the header, so the subscriptions API can tell a fresh direct fetch
// from FlClash's own database record.
type profileUserinfo struct {
	subscriptionInfo
	FetchedAt int64  `json:"fetched_at"`
	ProfileID string `json:"profile_id,omitempty"`
	URLDigest string `json:"url_digest,omitempty"`
}

// parseSubscriptionUserinfo parses a subscription-userinfo response header
// ("upload=..; download=..; total=..; expire=.."). It reports false when the
// header is absent or holds none of the four counters, so a broken header can
// never fabricate usage data.
func parseSubscriptionUserinfo(header string) (profileUserinfo, bool) {
	var info profileUserinfo
	for _, part := range strings.Split(header, ";") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil || n < 0 {
			continue
		}
		value := n
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "upload":
			info.Upload = &value
		case "download":
			info.Download = &value
		case "total":
			info.Total = &value
		case "expire":
			info.Expire = &value
		}
	}
	ok := info.Upload != nil || info.Download != nil || info.Total != nil || info.Expire != nil
	return info, ok
}

// clashPayloadValidator parses a Clash YAML payload from stdin and reports the
// usable node and rule counts without writing anything. Payload content is
// never echoed; exit codes classify the failure (2 = not Clash format,
// 4 = no usable nodes, 5 = parse error). The usable-node bar mirrors
// clashStoreReader so a refreshed profile file can never be something
// gen_config.py would choke on.
const clashPayloadValidator = `import sys, json, yaml
try:
    conf = yaml.safe_load(sys.stdin.buffer.read().decode('utf-8', 'replace'))
    if not isinstance(conf, dict) or not isinstance(conf.get('proxies'), list):
        sys.exit(2)
    kept = [p for p in conf['proxies'] if isinstance(p, dict)
            and isinstance(p.get('name'), str) and p.get('name')
            and isinstance(p.get('server'), str) and p.get('server')
            and p.get('port')
            and str(p.get('type', '')).lower() not in ('subscribe', 'direct', 'reject')]
    if not kept:
        sys.exit(4)
    rules = conf.get('rules')
    rules = [r for r in rules if isinstance(r, str)] if isinstance(rules, list) else []
    print(json.dumps({'proxies': len(kept), 'rules': len(rules)}))
except SystemExit:
    raise
except Exception:
    sys.exit(5)`

// validateClashPayload runs the parser helper on body and returns node counts.
func validateClashPayload(body []byte) (proxies, rules int, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", "-c", clashPayloadValidator)
	cmd.Stdin = strings.NewReader(string(body))
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		switch {
		case errors.As(err, &ee) && ee.ExitCode() == 2:
			return 0, 0, fmt.Errorf("not-clash")
		case errors.As(err, &ee) && ee.ExitCode() == 4:
			return 0, 0, fmt.Errorf("empty")
		}
		return 0, 0, fmt.Errorf("parse")
	}
	var res struct {
		Proxies int `json:"proxies"`
		Rules   int `json:"rules"`
	}
	if json.Unmarshal(out, &res) != nil || res.Proxies == 0 {
		return 0, 0, fmt.Errorf("parse")
	}
	return res.Proxies, res.Rules, nil
}

// profileRefresher keeps one FlClash profile-type source current. It resolves
// the subscription URL by profile id from FlClash's own database (read-only),
// fetches it with a direct transport and validates the Clash payload.
// The header is mirrored to a private cache; FlClash's profile is never written.
type profileRefresher struct {
	sourceID     string
	label        string
	profilePath  string // the FlClash profile file this source consumes
	dbPath       string // FlClash's own database.sqlite (read-only)
	backupPath   string // retained for compatibility; never written
	cachePath    string // private cache keyed by profile and URL digest
	userinfoPath string // sources/<id>.userinfo
	interval     time.Duration

	client *http.Client
	// resolveURL returns the profile's subscription URL. Default: FlClash
	// database lookup by profile id.
	resolveURL func() (string, error)
	// validate checks a payload and returns node counts without touching the
	// filesystem. Default: validateClashPayload.
	validate func(body []byte) (int, int, error)
	now      func() time.Time
	// OnUpdate, when set, is invoked after every successful profile store so
	// the runtime can regenerate and reload the engine config.
	OnUpdate func()

	mu          sync.Mutex
	status      string
	errClass    string
	proxies     int
	rules       int
	lastAttempt time.Time
	lastSuccess time.Time
}

// newProfileRefreshers builds one refresher per enabled profile-type source
// (in priority order). Returns nil when the config has none.
func newProfileRefreshers(cfg Config) []*profileRefresher {
	var out []*profileRefresher
	for _, s := range cfg.Sources {
		if s.Type != SourceTypeProfile || !s.Enabled || s.Path == "" {
			continue
		}
		targets := subscriptionTargets([]string{s.Path}, userHome())
		if len(targets) != 1 || targets[0].source != "flclash" {
			continue
		}
		if strings.HasPrefix(s.ID, "flclash-") && s.ID != "flclash-"+targets[0].id {
			continue
		}
		out = append(out, newProfileRefresher(s, subscriptionRefreshInterval(cfg)))
	}
	seen := map[string]bool{}
	taken := map[string]bool{}
	for _, s := range cfg.Sources {
		taken[s.ID] = true
	}
	for _, s := range cfg.Sources {
		targets := subscriptionTargets([]string{s.Path}, userHome())
		if s.Type == SourceTypeProfile && len(targets) == 1 && targets[0].source == "flclash" {
			seen[targets[0].id] = true
		}
	}
	ids, err := discoverFlClashIDs(flclashDatabasePath())
	if err == nil {
		for _, id := range ids {
			if seen[id] || taken["flclash-"+id] {
				continue
			}
			path := filepath.Join(filepath.Dir(flclashDatabasePath()), "profiles", id+".yaml")
			out = append(out, newProfileRefresher(SourceConfig{ID: "flclash-" + id, Label: "FlClash " + id, Type: SourceTypeProfile, Path: path, Enabled: true}, subscriptionRefreshInterval(cfg)))
		}
	}
	return out
}

func newProfileRefresher(src SourceConfig, interval time.Duration) *profileRefresher {
	r := &profileRefresher{
		sourceID:     src.ID,
		label:        src.Label,
		profilePath:  src.Path,
		dbPath:       flclashDatabasePath(),
		backupPath:   profileBackupPath(src.ID),
		userinfoPath: profileUserinfoPath(src.ID),
		interval:     interval,
		client:       directSubscriptionClient(30 * time.Second),
		now:          time.Now,
		status:       "unknown",
	}
	r.resolveURL = func() (string, error) {
		if override := configuredSourceURL(r.sourceID); override != "" {
			return override, nil
		}
		return r.defaultResolveURL()
	}
	r.validate = validateClashPayload
	return r
}

// Interval returns the refresh period (loop scheduling).
func (r *profileRefresher) Interval() time.Duration { return r.interval }

// Interval returns the refresh period (loop scheduling).
func (r *subscriptionRefresher) Interval() time.Duration { return r.interval }

// profileID derives the FlClash profile id from the profile file name
// (profiles/<id>.yaml).
func (r *profileRefresher) profileID() string {
	return strings.TrimSuffix(filepath.Base(r.profilePath), filepath.Ext(r.profilePath))
}

// defaultResolveURL looks the profile's subscription URL up in FlClash's
// database. Missing rows and file-type profiles (no URL) classify as
// "no-profile".
// configuredProxyAddr is srhop's own HTTP proxy listener, used as the second
// attempt when a subscription cannot be fetched directly.
func configuredProxyAddr() string {
	b, err := os.ReadFile(configPath())
	if err != nil {
		return ""
	}
	var cfg struct {
		HTTPListen string `json:"http_listen"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return ""
	}
	return strings.TrimSpace(cfg.HTTPListen)
}

func (r *profileRefresher) defaultResolveURL() (string, error) {
	urls, err := readFlClashProfileURLs(r.dbPath)
	if err != nil {
		return "", err
	}
	if u := urls[r.profileID()]; u != "" {
		return u, nil
	}
	return "", fmt.Errorf("no-profile")
}

// Snapshot returns the current status for external exposure.
func (r *profileRefresher) Snapshot() SubscriptionStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := SubscriptionStatus{
		SourceID: r.sourceID,
		Status:   r.status,
		Error:    r.errClass,
		Proxies:  r.proxies,
		Rules:    r.rules,
	}
	if !r.lastAttempt.IsZero() {
		attempt := r.lastAttempt
		st.LastAttempt = &attempt
	}
	if !r.lastSuccess.IsZero() {
		success := r.lastSuccess
		st.LastSuccess = &success
	}
	return st
}

func (r *profileRefresher) markAttempt() {
	r.mu.Lock()
	r.lastAttempt = r.now()
	r.mu.Unlock()
}

func (r *profileRefresher) markSuccess(proxies, rules int) {
	r.mu.Lock()
	r.status, r.errClass = "ok", ""
	r.proxies, r.rules = proxies, rules
	r.lastSuccess = r.now()
	r.mu.Unlock()
}

func (r *profileRefresher) markError(class string) {
	r.mu.Lock()
	r.status, r.errClass = "error", class
	r.mu.Unlock()
}

// backupPrevious copies the current profile file to sources/<id>.yaml.bak at
// 0600 so a bad refresh can always be recovered by hand.
func (r *profileRefresher) backupPrevious() error {
	b, err := os.ReadFile(r.profilePath)
	if err != nil {
		return fmt.Errorf("store")
	}
	if err := writePrivateFile(r.backupPath, string(b)); err != nil {
		return fmt.Errorf("store")
	}
	return nil
}

func flclashCachePath(profileID, url string) string {
	sum := sha256.Sum256([]byte(url))
	return filepath.Join(sourcesCacheDir(), "flclash-"+profileID+"-"+hex.EncodeToString(sum[:])+".yaml")
}

// storeProfile validates the response and caches official userinfo without
// writing to the source application or replacing its configuration.
func (r *profileRefresher) storeProfile(body []byte, userinfoHeader string) (int, int, error) {
	proxies, rules, err := r.validate(body)
	if err != nil {
		return 0, 0, err
	}
	if r.cachePath != "" {
		if err := writePrivateFile(r.cachePath, string(body)); err != nil {
			return 0, 0, fmt.Errorf("store")
		}
	}
	info, ok := parseSubscriptionUserinfo(userinfoHeader)
	if !ok {
		_ = os.Remove(r.userinfoPath)
	}
	if ok {
		info.FetchedAt = r.now().Unix()
		info.ProfileID = r.profileID()
		info.URLDigest = ""
		if r.cachePath != "" {
			info.URLDigest = filepath.Base(r.cachePath)
		}
		if b, merr := json.Marshal(info); merr == nil {
			if err := writePrivateFile(r.userinfoPath, string(b)+"\n"); err != nil {
				_ = os.Remove(r.userinfoPath)
			}
		}
	}
	return proxies, rules, nil
}

// RefreshOnce runs one resolve/fetch/validate/store cycle. On any failure the
// existing profile file is left untouched and an error status is recorded;
// success is never faked.
func (r *profileRefresher) RefreshOnce(ctx context.Context) {
	r.markAttempt()
	u, err := r.resolveURL()
	if err != nil {
		r.markError(errClass(err))
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		r.markError("invalid-url")
		return
	}
	req.Header.Set("User-Agent", "clash.meta/v1.19.2")
	req.Header.Set("Accept", "*/*")
	resp, err := r.client.Do(req)
	// Direct first. If that fails (blocked network, DNS, TLS) retry once through
	// srhop's own proxy, which has working upstream nodes.
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			_ = resp.Body.Close()
		}
		if proxyAddr := configuredProxyAddr(); proxyAddr != "" {
			if pc, perr := subscriptionProxyClient(proxyAddr, 30*time.Second); perr == nil {
				retry := req.Clone(ctx)
				if r2, e2 := pc.Do(retry); e2 == nil {
					resp, err = r2, nil
				}
			}
		}
		if err != nil {
			r.markError(sanitizeSubscriptionError(err))
			return
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		r.markError(fmt.Sprintf("http-%d", resp.StatusCode))
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSubscriptionBytes+1))
	if err != nil {
		r.markError(sanitizeSubscriptionError(err))
		return
	}
	if len(body) > maxSubscriptionBytes {
		r.markError("too-large")
		return
	}
	r.cachePath = flclashCachePath(r.profileID(), u)
	userinfo := resp.Header.Get("subscription-userinfo")
	proxies, rules, err := r.storeProfile(body, userinfo)
	if err != nil {
		r.markError(errClass(err))
		return
	}
	r.markSuccess(proxies, rules)
	if r.OnUpdate != nil {
		r.OnUpdate()
	}
}

// Lock order: sourceRegenMu -> office91SourceWriteMu (inside body) -> engineMu
// (inside reload). No callback acquires sourceRegenMu a second time.
func (a *App) withOffice91SourceTransaction(lifecycle, request context.Context, body func(context.Context, func() error, func() error) error, generate func(context.Context) error, reload func() error) error {
	for !a.sourceRegenMu.TryLock() {
		if a.shuttingDown.Load() {
			return errors.New("offline-runtime-unavailable")
		}
		select {
		case <-request.Done():
			return request.Err()
		case <-lifecycle.Done():
			return lifecycle.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer a.sourceRegenMu.Unlock()
	ctx, cancel := context.WithCancel(request)
	defer cancel()
	stop := context.AfterFunc(lifecycle, cancel)
	defer stop()
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if lifecycle.Err() != nil {
			return lifecycle.Err()
		}
		if a.shuttingDown.Load() {
			return errors.New("offline-runtime-unavailable")
		}
		return nil
	}
	if err := check(); err != nil {
		return err
	}
	releaseProcess, err := acquireSourceProcessLock(ctx)
	if err != nil {
		return err
	}
	defer releaseProcess()
	if err := check(); err != nil {
		return err
	}
	apply := func() error {
		if err := check(); err != nil {
			return err
		}
		if err := generate(ctx); err != nil {
			return err
		}
		if err := check(); err != nil {
			return err
		}
		if err := reload(); err != nil {
			return err
		}
		return check()
	}
	rollback := func() error {
		if a.shuttingDown.Load() {
			return errors.New("offline-rollback-failed")
		}
		return reload()
	}
	return body(ctx, apply, rollback)
}

func runOffice91GeneratorContext(ctx context.Context) error {
	pythonPath, err := exec.LookPath("python3")
	if err != nil {
		return errors.New("offline-generation-failed")
	}
	cmd, err := office91PythonCommand(ctx, pythonPath, genScriptPath())
	if err != nil {
		return errors.New("offline-generation-failed")
	}
	// Cancellation must stop the generator's Mihomo validation child too.
	// Content-bearing generator exceptions must not become API diagnostics.
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("offline-generation-failed")
	}
	return nil
}
