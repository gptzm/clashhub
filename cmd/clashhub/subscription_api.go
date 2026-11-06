package main

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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
	// RefreshSource is "direct" when subscription_info came from clashhub's own
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
	// directInfo maps "source:profile-id" to the clashhub source id whose
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

// readProfileUserinfo loads clashhub's private userinfo cache for a source.
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

// mergeDirectUserinfo overlays clashhub's own direct-fetch userinfo cache onto a
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

func readSubscription(t subscriptionTarget) subscriptionProfile {
	p := unknownSubscription(t)
	db, err := openSQLiteReadOnly(t.db)
	if err != nil {
		return p
	}
	defer db.Close()
	var id int64
	var label sql.NullString
	var raw sql.NullString
	var last sql.NullInt64
	if err := db.QueryRow("SELECT id, label, subscription_info, last_update_date FROM profiles WHERE id = ?", t.id).Scan(&id, &label, &raw, &last); err != nil {
		return p
	}
	if label.Valid {
		name := label.String
		if len(name) <= 256 && !strings.Contains(name, "://") && !strings.ContainsAny(name, "?@\n\r") {
			p.Name = &name
		}
	}
	p.SubscriptionInfo = parseSubscriptionInfo(raw.String)
	if last.Valid && last.Int64 >= 0 {
		v := last.Int64
		p.LastUpdateDate = &v
	}
	p.Status = "ok"
	return p
}

func parseSubscriptionInfo(raw string) subscriptionInfo {
	var info subscriptionInfo
	if strings.TrimSpace(raw) == "" {
		return info
	}
	var fields map[string]any
	if json.Unmarshal([]byte(raw), &fields) != nil {
		return info
	}
	num := func(key string) *int64 {
		v, ok := fields[key].(float64)
		if !ok || v < 0 || v > 9223372036854775807 {
			return nil
		}
		n := int64(v)
		return &n
	}
	info.Upload = num("upload")
	info.Download = num("download")
	info.Total = num("total")
	info.Expire = num("expire")
	return info
}

func discoverJiuyiSubscriptions(db string) ([]subscriptionProfile, error) {
	dbh, err := openSQLiteReadOnly(db)
	if err != nil {
		return nil, err
	}
	defer dbh.Close()
	rows, err := dbh.Query("SELECT id, label, subscription_info, last_update_date FROM profiles ORDER BY id LIMIT 10001")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []subscriptionProfile
	for rows.Next() {
		var id int64
		var label sql.NullString
		var raw sql.NullString
		var last sql.NullInt64
		if err := rows.Scan(&id, &label, &raw, &last); err != nil {
			continue
		}
		if id < 0 {
			continue
		}
		p := subscriptionProfile{Source: "jiuyi", ID: strconv.FormatInt(id, 10), SubscriptionInfo: parseSubscriptionInfo(raw.String), Status: "ok"}
		if label.Valid && len(label.String) <= 256 && !strings.Contains(label.String, "://") && !strings.ContainsAny(label.String, "?@\n\r") {
			name := label.String
			p.Name = &name
		}
		if last.Valid && last.Int64 >= 0 {
			v := last.Int64
			p.LastUpdateDate = &v
		}
		out = append(out, p)
	}
	return out, rows.Err()
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
