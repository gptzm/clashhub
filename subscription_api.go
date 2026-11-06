package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type subscriptionInfo struct {
	Upload   *int64 `json:"upload"`
	Download *int64 `json:"download"`
	Total    *int64 `json:"total"`
	Expire   *int64 `json:"expire"`
}
type subscriptionProfile struct {
	Source           string           `json:"source"`
	ID               string           `json:"id"`
	Name             *string          `json:"name"`
	SubscriptionInfo subscriptionInfo `json:"subscription_info"`
	LastUpdateDate   *int64           `json:"last_update_date"`
	Status           string           `json:"status"`
	// RefreshSource is "direct" when subscription_info came from srhop's own
	// direct-fetch cache rather than the local database record. The
	// subscription URL itself is never part of this payload.
	SourceID        string              `json:"source_id,omitempty"`
	RefreshStatus   *SubscriptionStatus `json:"refresh_status,omitempty"`
	DirectFetchedAt *int64              `json:"direct_fetched_at,omitempty"`
	RefreshSource   string              `json:"refresh_source,omitempty"`
}
type subscriptionTarget struct{ source, id, db string }
type subscriptionCache struct {
	mu              sync.Mutex
	targets         []subscriptionTarget
	profiles        []subscriptionProfile
	updated         time.Time
	loading         bool
	read            func(subscriptionTarget) subscriptionProfile
	discover        func() ([]subscriptionProfile, error)
	discoveryStatus string
	offline         *office91OfflineRuntime
	// directInfo maps "source:profile-id" to the srhop source id whose
	// private userinfo cache should overlay the database record (enabled
	// profile-type sources only).
	directInfo map[string]string
}

var profileIDPattern = regexp.MustCompile(`^[0-9]+$`)

func subscriptionTargets(paths []string, home string) []subscriptionTarget {
	roots := []struct{ source, dir string }{
		{"flclash", filepath.Join(home, "Library", "Application Support", "com.follow.clash")},
		{"jiuyi", filepath.Join(home, "Library", "Application Support", "com.abysswalker.office91")},
	}
	result := make([]subscriptionTarget, 0, len(paths))
	seen := map[string]bool{}
	for _, path := range paths {
		clean := filepath.Clean(path)
		for _, root := range roots {
			if !filepath.IsAbs(path) || filepath.Dir(clean) != filepath.Join(root.dir, "profiles") || filepath.Ext(clean) != ".yaml" {
				continue
			}
			id := strings.TrimSuffix(filepath.Base(clean), ".yaml")
			key := root.source + ":" + id
			if !profileIDPattern.MatchString(id) || seen[key] {
				continue
			}
			seen[key] = true
			result = append(result, subscriptionTarget{root.source, id, filepath.Join(root.dir, "database.sqlite")})
		}
	}
	return result
}
func unknownSubscription(t subscriptionTarget) subscriptionProfile {
	return subscriptionProfile{Source: t.source, ID: t.id, Status: "unknown"}
}
func newSubscriptionCache(cfg Config) *subscriptionCache {
	home := userHome()
	c := &subscriptionCache{
		targets:         subscriptionTargets(cfg.SourceProfiles, home),
		read:            readSubscription,
		discoveryStatus: "unknown",
		directInfo:      map[string]string{},
		offline:         newOffice91OfflineRuntime(cfg),
	}
	db := filepath.Join(home, "Library", "Application Support", "com.abysswalker.office91", "database.sqlite")
	c.discover = func() ([]subscriptionProfile, error) { return discoverJiuyiSubscriptions(db) }
	c.profiles = make([]subscriptionProfile, 0, len(c.targets))
	for _, t := range c.targets {
		c.profiles = append(c.profiles, unknownSubscription(t))
	}
	for _, s := range cfg.Sources {
		if s.Type != SourceTypeProfile || !s.Enabled || s.Path == "" {
			continue
		}
		for _, t := range subscriptionTargets([]string{s.Path}, home) {
			c.directInfo[t.source+":"+t.id] = s.ID
		}
	}
	return c
}

// readProfileUserinfo loads srhop's private userinfo cache for a source.
// Missing, oversized, malformed or empty records read as absent so the
// database record is preserved. The cache only ever holds usage counters and a
// timestamp — never the subscription URL.
func readProfileUserinfo(path string) (profileUserinfo, bool) {
	b, err := os.ReadFile(path)
	if err != nil || len(b) > 4096 {
		return profileUserinfo{}, false
	}
	var info profileUserinfo
	if json.Unmarshal(b, &info) != nil {
		return profileUserinfo{}, false
	}
	if info.FetchedAt <= 0 || (info.Upload == nil && info.Download == nil && info.Total == nil && info.Expire == nil) {
		return profileUserinfo{}, false
	}
	return info, true
}

// mergeDirectUserinfo overlays srhop's own direct-fetch userinfo cache onto a
// flclash profile record when the cache is strictly newer than the database
// record's last update. Any doubt keeps the database record untouched.
func mergeDirectUserinfo(p *subscriptionProfile, path string) {
	if p.Status != "ok" {
		return
	}
	info, ok := readProfileUserinfo(path)
	if !ok || info.ProfileID != p.ID {
		return
	}
	if p.LastUpdateDate != nil {
		last := *p.LastUpdateDate
		// FlClash versions store epoch seconds or milliseconds. Compare at
		// Cache time has second precision; ties conservatively favor the DB.
		if last >= 100000000000 {
			last /= 1000
		}
		if info.FetchedAt <= last {
			return
		}
	}
	p.SubscriptionInfo = info.subscriptionInfo
	p.DirectFetchedAt = &info.FetchedAt
	p.RefreshSource = "direct"
}

// Only these four columns are ever selected; the database is opened read-only.
// The helper emits a strictly constructed object, never raw records or errors.
const subscriptionReader = `import sys, sqlite3, json, pathlib
try:
 db=sqlite3.connect(pathlib.Path(sys.argv[1]).as_uri()+'?mode=ro', uri=True, timeout=0.2)
 try:
  row=db.execute('SELECT id, label, subscription_info, last_update_date FROM profiles WHERE id = ?', (int(sys.argv[2]),)).fetchone()
 finally: db.close()
 if row is None: sys.exit(1)
 def number(v):
  return v if type(v) is int and 0 <= v <= 9223372036854775807 else None
 try:
  info=json.loads(row[2]) if row[2] is not None else {}
  if not isinstance(info,dict): info={}
 except (ValueError,TypeError): info={}
 name=row[1] if isinstance(row[1],str) and len(row[1]) <= 256 else None
 if name and ('://' in name or '?' in name or '@' in name or '\n' in name or '\r' in name): name=None
 print(json.dumps({'name':name, 'subscription_info':{k:number(info.get(k)) for k in ('upload','download','total','expire')}, 'last_update_date':number(row[3])}))
except Exception: sys.exit(1)`

func readSubscription(t subscriptionTarget) subscriptionProfile {
	p := unknownSubscription(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "python3", "-c", subscriptionReader, t.db, t.id).Output()
	if err != nil || len(out) > 4096 {
		return p
	}
	var data struct {
		Name *string          `json:"name"`
		Info subscriptionInfo `json:"subscription_info"`
		Last *int64           `json:"last_update_date"`
	}
	if json.Unmarshal(out, &data) != nil {
		return p
	}
	p.Name, p.SubscriptionInfo, p.LastUpdateDate, p.Status = data.Name, data.Info, data.Last, "ok"
	return p
}

// Discover existing 玖壹 profiles independently of node-generation sources.
// Never select URL or secret columns, or emit the raw subscription JSON.
const jiuyiDiscoveryReader = `import sys, sqlite3, json, pathlib
try:
 db=sqlite3.connect(pathlib.Path(sys.argv[1]).as_uri()+'?mode=ro', uri=True, timeout=0.2)
 try:
  rows=db.execute('SELECT id, label, subscription_info, last_update_date FROM profiles ORDER BY id LIMIT 10001').fetchall()
 finally: db.close()
 if len(rows)>10000: sys.exit(1)
 def number(v):
  return v if type(v) is int and 0 <= v <= 9223372036854775807 else None
 result=[]
 for ident,name,raw,last in rows:
  if number(ident) is None: continue
  try:
   info=json.loads(raw) if raw is not None else {}
   if not isinstance(info,dict): info={}
  except (ValueError,TypeError): info={}
  if not isinstance(name,str) or len(name)>256 or any(s in name for s in ('://','?','@','\n','\r')): name=None
  result.append({'source':'jiuyi','id':str(ident),'name':name,'subscription_info':{k:number(info.get(k)) for k in ('upload','download','total','expire')},'last_update_date':number(last),'status':'ok'})
 print(json.dumps(result))
except Exception: sys.exit(1)`

func discoverJiuyiSubscriptions(db string) ([]subscriptionProfile, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "python3", "-c", jiuyiDiscoveryReader, db).Output()
	if err != nil {
		return nil, err
	}
	var profiles []subscriptionProfile
	if err := json.Unmarshal(out, &profiles); err != nil {
		return nil, err
	}
	return profiles, nil
}

func (c *subscriptionCache) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
			http.Error(w, "JSON required", http.StatusUnsupportedMediaType)
			return
		}
		var body struct {
			SourceID      string `json:"source_id"`
			Mode          string `json:"mode,omitempty"`
			ProfilePath   string `json:"profile_path,omitempty"`
			KeyPath       string `json:"key_path,omitempty"`
			PythonPath    string `json:"python_path,omitempty"`
			ValidatorPath string `json:"validator_path,omitempty"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		dec.DisallowUnknownFields()
		if dec.Decode(&body) != nil || !sourceIDPattern.MatchString(body.SourceID) {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		var extra interface{}
		if dec.Decode(&extra) != io.EOF {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if body.SourceID == sourceIDOffice91 && !office91ModuleEnabled() {
			http.Error(w, "offline-source-disabled", http.StatusForbidden)
			return
		}
		offline := body.Mode == "offline" || body.ProfilePath != "" || body.KeyPath != "" || body.PythonPath != "" || body.ValidatorPath != ""
		if offline {
			if body.Mode != "offline" || body.SourceID != sourceIDOffice91 || c.offline == nil {
				http.Error(w, "invalid offline request", http.StatusBadRequest)
				return
			}
			queued, err := c.offline.enqueue(office91OfflineRequest{
				ProfilePath: body.ProfilePath, KeyPath: body.KeyPath,
				PythonPath: body.PythonPath, ValidatorPath: body.ValidatorPath,
			})
			if err != nil {
				status := http.StatusBadRequest
				// An accepted offline import must be able to enter the daemon
				// transaction. During startup/shutdown the hook is absent; reject
				// explicitly instead of allowing a cache-only false success.
				if office91OfflineErrorClass(err) == "offline-runtime-not-ready" {
					status = http.StatusServiceUnavailable
				}
				http.Error(w, office91OfflineErrorClass(err), status)
				return
			}
			status := "queued"
			if !queued {
				status = "already_running"
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": status, "source_id": body.SourceID})
			return
		}
		if body.Mode == "online" {
			if body.SourceID != sourceIDOffice91 || c.offline == nil {
				http.Error(w, "invalid online request", http.StatusBadRequest)
				return
			}
			queued, err := c.offline.switchOnline()
			if err != nil {
				http.Error(w, office91OfflineErrorClass(err), http.StatusConflict)
				return
			}
			status := "mode_set"
			if queued {
				status = "queued"
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": status, "source_id": body.SourceID})
			return
		}
		if body.Mode != "" {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if body.SourceID == sourceIDOffice91 {
			active, modeErr := office91OfflineModeActive()
			if active || modeErr != nil {
				http.Error(w, "source is in offline mode", http.StatusConflict)
				return
			}
		}
		if !requestSourceRefresh(body.SourceID) {
			http.Error(w, "source not refreshable", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "queued", "source_id": body.SourceID})
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	c.mu.Lock()
	if !c.loading && (c.updated.IsZero() || time.Since(c.updated) >= time.Minute) {
		c.loading = true
		go c.refresh()
	}
	profiles := append([]subscriptionProfile{}, c.profiles...)
	for i := range profiles {
		if profiles[i].SourceID != "" {
			if st, ok := subscriptionStatusSnapshot(profiles[i].SourceID); ok {
				profiles[i].RefreshStatus = &st
			}
		}
	}
	loading, updated, discoveryStatus := c.loading, c.updated, c.discoveryStatus
	c.mu.Unlock()
	moduleEnabled := office91ModuleEnabled()
	if !moduleEnabled {
		visible := profiles[:0]
		for _, profile := range profiles {
			if profile.Source != "jiuyi" && profile.SourceID != sourceIDOffice91 {
				visible = append(visible, profile)
			}
		}
		profiles = visible
	}
	var updatedAt *time.Time
	if !updated.IsZero() {
		updatedAt = &updated
	}
	subscriptionStatusMu.Lock()
	statuses := make(map[string]SubscriptionStatus, len(subscriptionStatuses))
	for id, st := range subscriptionStatuses {
		statuses[id] = st
	}
	subscriptionStatusMu.Unlock()
	office91Mode, modeErr := readOffice91Mode()
	if modeErr != nil {
		office91Mode = "unknown"
	}
	offlineRunning := c.offline != nil && c.offline.Running()
	// An online fetch started before the offline ownership change may publish
	// its final diagnostic later. It cannot replace offline operation status.
	if c.offline != nil && (offlineRunning || office91Mode == office91ModeOffline) {
		statuses[sourceIDOffice91] = c.offline.Snapshot()
	}
	_ = json.NewEncoder(w).Encode(struct {
		Office91Enabled        bool                          `json:"office91_enabled"`
		Office91Mode           string                        `json:"office91_mode"`
		Office91OfflineRunning bool                          `json:"office91_offline_running"`
		Profiles               []subscriptionProfile         `json:"profiles"`
		RefreshStatuses        map[string]SubscriptionStatus `json:"refresh_statuses"`
		Refreshing             bool                          `json:"refreshing"`
		UpdatedAt              *time.Time                    `json:"updated_at"`
		JiuyiDiscoveryStatus   string                        `json:"jiuyi_discovery_status"`
	}{moduleEnabled, office91Mode, offlineRunning, profiles, statuses, loading, updatedAt, discoveryStatus})
}
func office91ModuleEnabled() bool {
	b, err := os.ReadFile(configPath())
	if err != nil {
		return false
	}
	var cfg Config
	return json.Unmarshal(b, &cfg) == nil && cfg.Office91Enabled
}
func (c *subscriptionCache) refresh() {
	profiles := make([]subscriptionProfile, 0, len(c.targets))
	for _, t := range c.targets {
		if t.source == "jiuyi" && !office91ModuleEnabled() {
			continue
		}
		p := c.read(t)
		profiles = append(profiles, p)
	}
	ids, discoveryErr := discoverFlClashIDs(flclashDatabasePath())
	seenFl := map[string]bool{}
	for _, p := range profiles {
		if p.Source == "flclash" {
			seenFl[p.ID] = true
		}
	}
	if discoveryErr == nil {
		for _, id := range ids {
			if seenFl[id] {
				continue
			}
			profiles = append(profiles, c.read(subscriptionTarget{source: "flclash", id: id, db: flclashDatabasePath()}))
		}
	}
	urls, urlErr := readFlClashProfileURLs(flclashDatabasePath())
	for i := range profiles {
		p := &profiles[i]
		if p.Source != "flclash" {
			continue
		}
		sid := c.directInfo[p.Source+":"+p.ID]
		if sid == "" {
			sid = "flclash-" + p.ID
		}
		p.SourceID = sid
		if st, ok := subscriptionStatusSnapshot(sid); ok {
			p.RefreshStatus = &st
		}
		if urlErr == nil && urls[p.ID] != "" {
			info, ok := readProfileUserinfo(profileUserinfoPath(sid))
			if ok && info.URLDigest == filepath.Base(flclashCachePath(p.ID, urls[p.ID])) {
				mergeDirectUserinfo(p, profileUserinfoPath(sid))
			}
		}
	}
	status := "unknown"
	if c.discover != nil && office91ModuleEnabled() {
		found, err := c.discover()
		if err == nil {
			status = "ok"
			seen := make(map[string]bool)
			for _, p := range profiles {
				seen[p.Source+":"+p.ID] = true
			}
			for _, p := range found {
				if !seen[p.Source+":"+p.ID] {
					profiles = append(profiles, p)
					seen[p.Source+":"+p.ID] = true
				}
			}
		}
	}
	// The 玖壹 direct cache is not bound to a profile ID.
	c.mu.Lock()
	if status == "unknown" && office91ModuleEnabled() {
		seen := make(map[string]bool)
		for _, p := range profiles {
			seen[p.Source+":"+p.ID] = true
		}
		for _, p := range c.profiles {
			if p.Source == "jiuyi" && !seen[p.Source+":"+p.ID] {
				profiles = append(profiles, unknownSubscription(subscriptionTarget{source: p.Source, id: p.ID}))
			}
		}
	}
	c.profiles, c.updated, c.loading, c.discoveryStatus = profiles, time.Now().UTC(), false, status
	writeSourceExpiryCache(profiles)
	c.mu.Unlock()
}
