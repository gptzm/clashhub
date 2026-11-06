package main

// Subscription source support.
//
// 玖壹 (Office91): the subscription URL is read from 玖壹's local sqlite
// database in immutable read-only mode and mirrored into srhop's private cache
// (0600). FlClash profile sources resolve their subscription URL by profile id
// from FlClash's own sqlite database the same way; the fetch/validate/store
// cycle for those lives in source_runtime.go (profileRefresher). URLs and
// subscription content are secrets: they are never printed, logged, or
// embedded in error text — errors are reported as stable classes only.
//
// Fetching uses a dedicated direct transport (Proxy: nil), so it can never be
// routed through srhop's own proxy or a system proxy: that would make the
// fetch depend on (and potentially loop through) the engine it feeds.
//
// 玖壹 request signing has not been verified. Automatic requests fail closed
// until a verified implementation is available; the previous node cache stays
// untouched. The experimental signing helpers are not used by the refresher.
//
// On fetch or parse failure the last successful node cache is kept untouched
// and the failure is exposed as an error status; success is never faked.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxSubscriptionBytes = 8 << 20 // 8 MiB
	maxSubscriptionURL   = 2048
	// Minimum refresh period; anything shorter is treated as a misconfig.
	minSubscriptionRefresh = time.Minute
	// jiuyiUserAgent mirrors the app's UA layout (template 0x1eab90: space-
	// joined key=value pairs; device fields come from device_info_plus).
	jiuyiUserAgent = "BedofChaos/1.0 (iOS 18.6.2; iPhone14,2) clientx/2.1.1+62 appVersion/2.1.1"
)

// signedSubscriptionHeaders builds the X-Ts / X-Nonce / X-Sign header set for a
// 玖壹 API GET. component is the exact request URL; body is "" for GETs.
// now/nonce are injectable for tests.
func signedSubscriptionHeaders(key []byte, component, body string, now func() time.Time, nonce func() []byte) (http.Header, error) {
	ts := now().Unix()
	nb := nonce()
	if len(nb) != 16 {
		return nil, fmt.Errorf("nonce")
	}
	nonceHex := hex.EncodeToString(nb)
	msg := strings.Join([]string{component, body, strconv.FormatInt(ts, 10), nonceHex, component}, "\n")
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msg))
	h := make(http.Header, 3)
	h.Set("X-Ts", strconv.FormatInt(ts, 10))
	h.Set("X-Nonce", nonceHex)
	h.Set("X-Sign", hex.EncodeToString(mac.Sum(nil)))
	return h, nil
}

func sourcesCacheDir() string { return filepath.Join(appSupportDir(), "sources") }

// subscriptionYAMLPath is the node cache consumed by gen_config.py
// (~/Library/Application Support/srhop/sources/<id>.yaml).
func subscriptionYAMLPath(sourceID string) string {
	return filepath.Join(sourcesCacheDir(), sourceID+".yaml")
}

// subscriptionURLCachePath is the private mirror of the 玖壹 subscription URL.
func subscriptionURLCachePath(sourceID string) string {
	return filepath.Join(sourcesCacheDir(), sourceID+".url")
}

func subscriptionProxyAddrs(cfg Config) []string {
	addrs := make([]string, 0, len(cfg.FallbackAddrs)+2)
	addrs = append(addrs, cfg.HTTPListen, cfg.FallbackAddr)
	addrs = append(addrs, cfg.FallbackAddrs...)
	return addrs
}

// profileBackupPath holds the 0600 backup of the previous FlClash profile file
// for a profile-type source, written before a refresh replaces that file.
func profileBackupPath(sourceID string) string {
	return filepath.Join(sourcesCacheDir(), sourceID+".yaml.bak")
}

// profileUserinfoPath is the private cache of the last subscription-userinfo
// header observed while refreshing a profile-type source.
func profileUserinfoPath(sourceID string) string {
	return filepath.Join(sourcesCacheDir(), sourceID+".userinfo")
}

func jiuyiDatabasePath() string {
	return filepath.Join(userHome(), "Library", "Application Support", "com.abysswalker.office91", "database.sqlite")
}

func flclashDatabasePath() string {
	return filepath.Join(userHome(), "Library", "Application Support", "com.follow.clash", "database.sqlite")
}

// jiuyiURLReader extracts the subscription URL from 玖壹's database. The
// database is opened with mode=ro&immutable=1 (never created or written) and
// only the url column of the lowest-id profile row is read. Exit codes are
// stable classes; no URL content ever reaches stderr.
const jiuyiURLReader = `import sys, sqlite3, pathlib
try:
    path = pathlib.Path(sys.argv[1])
    if not path.is_file():
        sys.exit(3)
    db = sqlite3.connect(path.as_uri() + '?mode=ro', uri=True, timeout=0.2)
    try:
        row = db.execute('SELECT url FROM profiles ORDER BY id LIMIT 1').fetchone()
    finally:
        db.close()
    if row is None or not isinstance(row[0], str):
        sys.exit(1)
    url = row[0].strip()
    if not url.startswith(('http://', 'https://')) or len(url) > 2048 or any(c.isspace() for c in url):
        sys.exit(2)
    sys.stdout.write(url)
except Exception:
    sys.exit(1)`

// clashStoreReader parses a Clash YAML payload from stdin and atomically
// replaces the node cache at argv[1] with a 0600 file holding the extracted
// proxies and rules. Payload content is never echoed; exit codes classify the
// failure (2 = not Clash format, 4 = no usable nodes, 5 = parse error).
const clashStoreReader = `import sys, os, json, tempfile
import yaml
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
    fd, tmp = tempfile.mkstemp(prefix='.sub-', dir=os.path.dirname(os.path.abspath(sys.argv[1])))
    try:
        with os.fdopen(fd, 'w', encoding='utf-8') as f:
            yaml.safe_dump({'proxies': kept, 'rules': rules}, f, allow_unicode=True, sort_keys=False, width=10000)
        os.chmod(tmp, 0o600)
        os.replace(tmp, sys.argv[1])
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)
    print(json.dumps({'proxies': len(kept), 'rules': len(rules)}))
except SystemExit:
    raise
except Exception:
    sys.exit(5)`

// readJiuyiSubscriptionURL returns the subscription URL and a stable error
// class ("db-missing", "no-profile", "invalid-url", "no-url"). The URL is
// returned in memory only.
func readJiuyiSubscriptionURL(dbPath string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "python3", "-c", jiuyiURLReader, dbPath).Output()
	if err != nil {
		var ee *exec.ExitError
		switch {
		case errors.As(err, &ee) && ee.ExitCode() == 3:
			return "", fmt.Errorf("db-missing")
		case errors.As(err, &ee) && ee.ExitCode() == 2:
			return "", fmt.Errorf("invalid-url")
		case errors.As(err, &ee) && ee.ExitCode() == 1:
			return "", fmt.Errorf("no-profile")
		}
		return "", fmt.Errorf("no-url")
	}
	u := strings.TrimSpace(string(out))
	if u == "" || len(u) > maxSubscriptionURL || strings.ContainsAny(u, " \t\r\n") {
		return "", fmt.Errorf("invalid-url")
	}
	return u, nil
}

// flclashURLReader extracts the subscription URL of every FlClash profile from
// FlClash's database. The database is opened with mode=ro&immutable=1 (never
// created or written) and only the id and url columns are read; rows without a
// plausible http(s) URL are skipped. The result is a JSON object mapping the
// profile id to its subscription URL; no URL content reaches stderr.
const flclashURLReader = `import sys, sqlite3, json, pathlib
try:
    path = pathlib.Path(sys.argv[1])
    if not path.is_file():
        sys.exit(3)
    db = sqlite3.connect(path.as_uri() + '?mode=ro', uri=True, timeout=0.2)
    try:
        rows = db.execute('SELECT id, url FROM profiles').fetchall()
    finally:
        db.close()
    out = {}
    for ident, url in rows:
        if not isinstance(ident, int) or not isinstance(url, str):
            continue
        u = url.strip()
        if not u.startswith(('http://', 'https://')) or len(u) > 2048 or any(c.isspace() for c in u):
            continue
        out[str(ident)] = u
    sys.stdout.write(json.dumps(out))
except Exception:
    sys.exit(1)`

// flclashProfileIDs discovers profile IDs without exposing subscription URLs.
const flclashProfileIDReader = `import sys, sqlite3, json, pathlib
try:
 db=sqlite3.connect(pathlib.Path(sys.argv[1]).as_uri()+'?mode=ro', uri=True, timeout=0.2)
 try: rows=db.execute('SELECT id FROM profiles ORDER BY id LIMIT 10001').fetchall()
 finally: db.close()
 if len(rows)>10000: sys.exit(1)
 print(json.dumps([str(r[0]) for r in rows if type(r[0]) is int and r[0]>=0]))
except Exception: sys.exit(1)`

func discoverFlClashIDs(db string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "python3", "-c", flclashProfileIDReader, db).Output()
	if err != nil || len(out) > 300000 {
		return nil, fmt.Errorf("discovery-failed")
	}
	var ids []string
	if json.Unmarshal(out, &ids) != nil {
		return nil, fmt.Errorf("discovery-failed")
	}
	return ids, nil
}

// readFlClashProfileURLs returns the profile id -> subscription URL mapping
// from FlClash's database. Stable error classes: "db-missing" (FlClash has no
// database here), "no-url" (unreadable, no table, or no valid mapping). The
// URLs are returned in memory only.
func readFlClashProfileURLs(dbPath string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "python3", "-c", flclashURLReader, dbPath).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 3 {
			return nil, fmt.Errorf("db-missing")
		}
		return nil, fmt.Errorf("no-url")
	}
	if len(out) > 1<<20 {
		return nil, fmt.Errorf("no-url")
	}
	var urls map[string]string
	if json.Unmarshal(out, &urls) != nil {
		return nil, fmt.Errorf("no-url")
	}
	for id, u := range urls {
		if id == "" || u == "" || len(u) > maxSubscriptionURL || strings.ContainsAny(u, " \t\r\n") {
			delete(urls, id)
		}
	}
	return urls, nil
}

// writePrivateFile atomically writes data at path with 0600 permissions.
func writePrivateFile(path, data string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".private-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.WriteString(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

// atomicWriteFile replaces path with data via a temp file in the same
// directory with the given permissions, so concurrent readers (FlClash, the
// engine watcher) never observe a partial file.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".profile-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Chmod(name, perm); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

// directSubscriptionClient returns a client whose transport explicitly has no
// proxy: neither srhop's own listener nor any environment/system proxy can be
// used, which prevents fetch loops through the engine being configured.
func directSubscriptionClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          2,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func subscriptionProxyClient(addr string, timeout time.Duration) (*http.Client, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil, fmt.Errorf("invalid-proxy")
	}
	pu, err := url.Parse("http://" + addr)
	if err != nil || pu.Host == "" {
		return nil, fmt.Errorf("invalid-proxy")
	}
	base := directSubscriptionClient(timeout)
	transport := base.Transport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(pu)
	base.Transport = transport
	return base, nil
}

func validateSubscriptionURL(raw string) error {
	if raw == "" || len(raw) > maxSubscriptionURL || strings.ContainsAny(raw, " \t\r\n") {
		return fmt.Errorf("invalid-url")
	}
	u, err := url.ParseRequestURI(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("invalid-url")
	}
	return nil
}

func fetchSubscriptionWithProxyRetry(ctx context.Context, target string, headers http.Header, direct *http.Client, proxyAddrs []string) ([]byte, string, error) {
	if err := validateSubscriptionURL(target); err != nil {
		return nil, "", err
	}
	if direct == nil {
		direct = directSubscriptionClient(30 * time.Second)
	}
	body, userinfo, err := fetchSubscription(ctx, direct, target, headers)
	if err == nil {
		return body, userinfo, nil
	}
	lastErr := err
	seen := map[string]bool{}
	for _, addr := range proxyAddrs {
		addr = strings.TrimSpace(addr)
		if addr == "" || seen[addr] {
			continue
		}
		seen[addr] = true
		client, clientErr := subscriptionProxyClient(addr, 30*time.Second)
		if clientErr != nil {
			continue
		}
		body, userinfo, err = fetchSubscription(ctx, client, target, headers)
		client.CloseIdleConnections()
		if err == nil {
			return body, userinfo, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("network")
	}
	return nil, "", lastErr
}

// fetchSubscription returns only bounded body bytes and the optional quota
// header. Transport errors are sanitized before they can reach status/API.
func fetchSubscription(ctx context.Context, client *http.Client, target string, headers http.Header) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "", fmt.Errorf("invalid-url")
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("%s", sanitizeSubscriptionError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("http-%d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSubscriptionBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("%s", sanitizeSubscriptionError(err))
	}
	if len(body) > maxSubscriptionBytes {
		return nil, "", fmt.Errorf("too-large")
	}
	return body, resp.Header.Get("subscription-userinfo"), nil
}

// cryptoNonce16 returns 16 cryptographic random bytes (the app's X-Nonce
// source: Random.secure().nextInt(256) x16, hex-encoded).
func cryptoNonce16() []byte {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil
	}
	return b
}

// sanitizeSubscriptionError maps a transport error to a safe class. Error
// strings from net/http embed the full URL, so only the error category is
// kept ("timeout" or "network").
func sanitizeSubscriptionError(err error) string {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	return "network"
}

// storeClashNodes runs the parser helper and returns the stored node counts.
func storeClashNodes(body []byte, dst string) (proxies, rules int, err error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return 0, 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", "-c", clashStoreReader, dst)
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
	if jsonErr := json.Unmarshal(out, &res); jsonErr != nil || res.Proxies == 0 {
		return 0, 0, fmt.Errorf("parse")
	}
	if err := os.Chmod(dst, 0o600); err != nil {
		return 0, 0, fmt.Errorf("store")
	}
	return res.Proxies, res.Rules, nil
}

// SubscriptionStatus is the externally visible state of one subscription
// source. Error is a stable class string, never URL or content.
type SubscriptionStatus struct {
	SourceID    string     `json:"source_id"`
	Status      string     `json:"status"` // unknown | ok | error
	Error       string     `json:"error,omitempty"`
	Proxies     int        `json:"proxies"`
	Rules       int        `json:"rules"`
	LastAttempt *time.Time `json:"last_attempt,omitempty"`
	LastSuccess *time.Time `json:"last_success,omitempty"`
}

// subscriptionRefresher keeps one subscription source's node cache fresh.
// All hooks are fields so tests can inject fakes without network access.
type subscriptionRefresher struct {
	sourceID     string
	label        string
	dbPath       string
	urlPath      string
	yamlPath     string
	userinfoPath string // sources/<id>.userinfo private cache of the official quota
	interval     time.Duration
	fetchTimeout time.Duration

	client     *http.Client
	proxyAddrs []string
	// signHeaders builds the signed header set for one request; injectable for
	// tests (set to nil to send unsigned headers).
	signHeaders func(component, body string) (http.Header, error)
	// resolveURL returns the subscription URL. Default: 玖壹 database with
	// fallback to the private URL cache when the database is unavailable.
	resolveURL func() (string, error)
	// parseStore validates a payload and stores the node cache. Default:
	// storeClashNodes.
	parseStore func(body []byte, dst string) (int, int, error)
	now        func() time.Time
	// OnUpdate, when set, is invoked after every successful cache store so
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

// subscriptionRefreshInterval normalizes the configured refresh period: the
// default (6h) when unset, clamped to the minimum sane period.
func subscriptionRefreshInterval(cfg Config) time.Duration {
	interval := time.Duration(cfg.SubscriptionRefreshHours) * time.Hour
	if interval <= 0 {
		interval = 6 * time.Hour
	}
	if interval < minSubscriptionRefresh {
		interval = minSubscriptionRefresh
	}
	return interval
}

// newSubscriptionRefreshers builds one refresher per enabled subscription
// source (in priority order). Returns nil when the config has none.
func newSubscriptionRefreshers(cfg Config) []*subscriptionRefresher {
	var out []*subscriptionRefresher
	for _, s := range cfg.Sources {
		if s.Type != SourceTypeSubscription || !s.Enabled {
			continue
		}
		if s.ID == sourceIDOffice91 && !office91SourceEnabledInConfig(cfg) {
			continue
		}
		r := newSubscriptionRefresher(s, subscriptionRefreshInterval(cfg))
		r.proxyAddrs = subscriptionProxyAddrs(cfg)
		out = append(out, r)
	}
	return out
}

func newSubscriptionRefresher(src SourceConfig, interval time.Duration) *subscriptionRefresher {
	r := &subscriptionRefresher{
		sourceID:     src.ID,
		label:        src.Label,
		dbPath:       jiuyiDatabasePath(),
		urlPath:      subscriptionURLCachePath(src.ID),
		yamlPath:     subscriptionYAMLPath(src.ID),
		interval:     interval,
		fetchTimeout: 30 * time.Second,
		client:       directSubscriptionClient(30 * time.Second),
		now:          time.Now,
		status:       "unknown",
	}
	if src.ID == sourceIDOffice91 || !strings.HasPrefix(src.ID, customSubscriptionSourcePrefix) {
		r.resolveURL = r.defaultResolveURL
		r.signHeaders = func(component, body string) (http.Header, error) {
			return nil, fmt.Errorf("signing-unverified")
		}
	} else {
		r.resolveURL = r.cachedURL
		r.signHeaders = nil
	}
	r.parseStore = storeClashNodes
	r.userinfoPath = profileUserinfoPath(src.ID)
	return r
}

// YAMLPath returns the node cache path (watch it to detect cache updates).
func (r *subscriptionRefresher) YAMLPath() string { return r.yamlPath }

// defaultResolveURL prefers the 玖壹 database and falls back to the last
// privately cached URL when the database is unreadable or empty.
func (r *subscriptionRefresher) defaultResolveURL() (string, error) {
	u, err := readJiuyiSubscriptionURL(r.dbPath)
	if err == nil {
		if werr := writePrivateFile(r.urlPath, u+"\n"); werr == nil {
			return u, nil
		}
		// Cache write failed but the URL is valid; still use it.
		return u, nil
	}
	if b, rerr := os.ReadFile(r.urlPath); rerr == nil {
		cached := strings.TrimSpace(string(b))
		if cached != "" && len(cached) <= maxSubscriptionURL && !strings.ContainsAny(cached, " \t\r\n") {
			return cached, nil
		}
	}
	return "", err
}

func (r *subscriptionRefresher) cachedURL() (string, error) {
	b, err := os.ReadFile(r.urlPath)
	if err != nil {
		return "", fmt.Errorf("no-url")
	}
	u := strings.TrimSpace(string(b))
	if err := validateSubscriptionURL(u); err != nil {
		return "", err
	}
	return u, nil
}

// Snapshot returns the current status for external exposure.
func (r *subscriptionRefresher) Snapshot() SubscriptionStatus {
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

func (r *subscriptionRefresher) markAttempt() {
	r.mu.Lock()
	r.lastAttempt = r.now()
	r.mu.Unlock()
}

func (r *subscriptionRefresher) markSuccess(proxies, rules int) {
	r.mu.Lock()
	r.status, r.errClass = "ok", ""
	r.proxies, r.rules = proxies, rules
	r.lastSuccess = r.now()
	r.mu.Unlock()
}

func (r *subscriptionRefresher) markError(class string) {
	r.mu.Lock()
	r.status, r.errClass = "error", class
	r.mu.Unlock()
}

// storeUserinfo mirrors the official subscription-userinfo header to the
// private cache (0600, with fetched_at) for the subscriptions API overlay. A
// failed or absent cache write never fails the refresh itself.
func (r *subscriptionRefresher) storeUserinfo(header string, fetchedAt time.Time) {
	info, ok := parseSubscriptionUserinfo(header)
	if !ok {
		return
	}
	info.FetchedAt = fetchedAt.Unix()
	if b, err := json.Marshal(info); err == nil {
		_ = writePrivateFile(r.userinfoPath, string(b)+"\n")
	}
}

// RefreshOnce runs one resolve/fetch/parse cycle. On failure the previous
// node cache is left untouched and an error status is recorded; success is
// never faked. The default signer fails closed until signing is verified.
var customSubscriptionCommitMu sync.Mutex

// customSourceCurrent verifies durable configuration and URL under the shared
// commit lock. It never logs the subscription URL.
func customSourceCurrent(id, expected string) bool {
	_, sources, _, err := readConfigSources(configPath())
	if err != nil {
		return false
	}
	for _, s := range sources {
		if s.ID == id && s.Type == SourceTypeSubscription && s.Enabled {
			b, err := os.ReadFile(subscriptionURLCachePath(id))
			return err == nil && strings.TrimSpace(string(b)) == expected
		}
	}
	return false
}

func (r *subscriptionRefresher) RefreshOnce(ctx context.Context) {
	r.markAttempt()
	if strings.HasPrefix(r.sourceID, customSubscriptionSourcePrefix) {
		customSubscriptionCommitMu.Lock()
		u, err := r.cachedURL()
		allowed := err == nil && customSourceCurrent(r.sourceID, u)
		customSubscriptionCommitMu.Unlock()
		if !allowed {
			r.markError("source-disabled")
			return
		}
	}
	if r.sourceID == sourceIDOffice91 {
		if !office91OnlineModuleEnabled() {
			r.markError("module-disabled")
			return
		}
		active, modeErr := office91OfflineModeActive()
		if active || modeErr != nil {
			r.markError("offline-mode")
			return
		}
	}
	u, err := r.resolveURL()
	if err != nil {
		r.markError(errClass(err))
		return
	}
	if err := validateSubscriptionURL(u); err != nil {
		r.markError("invalid-url")
		return
	}
	headers := make(http.Header)
	if r.signHeaders != nil {
		sh, serr := r.signHeaders(u, "")
		if serr != nil {
			r.markError(errClass(serr))
			return
		}
		headers = sh
	}
	if headers == nil {
		headers = make(http.Header)
	}
	userAgent := "srhop/1.0"
	if r.sourceID == sourceIDOffice91 {
		userAgent = jiuyiUserAgent
	}
	headers.Set("User-Agent", userAgent)
	headers.Set("Accept", "*/*")
	body, userinfo, err := fetchSubscriptionWithProxyRetry(ctx, u, headers, r.client, r.proxyAddrs)
	if err != nil {
		r.markError(errClass(err))
		return
	}
	custom := strings.HasPrefix(r.sourceID, customSubscriptionSourcePrefix)
	if custom {
		customSubscriptionCommitMu.Lock()
		defer customSubscriptionCommitMu.Unlock()
		if ctx.Err() != nil || !customSourceCurrent(r.sourceID, u) {
			r.markError("source-changed")
			return
		}
	}
	office91SourceWriteMu.Lock()
	if r.sourceID == sourceIDOffice91 {
		active, modeErr := office91OfflineModeActive()
		if active || modeErr != nil {
			office91SourceWriteMu.Unlock()
			r.markError("offline-mode")
			return
		}
	}
	proxies, rules, err := r.parseStore(body, r.yamlPath)
	if err != nil {
		office91SourceWriteMu.Unlock()
		r.markError(errClass(err))
		return
	}
	r.storeUserinfo(userinfo, r.now())
	office91SourceWriteMu.Unlock()
	r.markSuccess(proxies, rules)
	if r.OnUpdate != nil {
		r.OnUpdate()
	}
}

// office91OnlineModuleEnabled checks only srhop's own configuration. The
// Office91 database and URL cache are not touched unless the module and source
// are explicitly enabled in the current persisted config.
func office91OnlineModuleEnabled() bool {
	b, err := os.ReadFile(configPath())
	if err != nil {
		return false
	}
	var cfg Config
	return json.Unmarshal(b, &cfg) == nil && office91SourceEnabledInConfig(cfg)
}

// Run refreshes immediately and then on every interval tick until ctx is
// cancelled. Intended to run on its own goroutine.
func (r *subscriptionRefresher) Run(ctx context.Context) {
	r.RefreshOnce(ctx)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.RefreshOnce(ctx)
		}
	}
}

// errClass extracts the stable class from errors produced above; any wrapped
// or unknown error degrades to "error" so URL or content can never leak.
func errClass(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	for _, class := range []string{"db-missing", "no-profile", "invalid-url", "no-url", "not-clash", "empty", "parse", "store", "timeout", "network", "too-large", "signing-unverified"} {
		if msg == class {
			return class
		}
	}
	if strings.HasPrefix(msg, "http-") {
		if _, err := strconv.Atoi(strings.TrimPrefix(msg, "http-")); err == nil {
			return msg
		}
	}
	return "error"
}
