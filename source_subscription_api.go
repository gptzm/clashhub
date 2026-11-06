package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const customSubscriptionSourcePrefix = "custom-sub-"

type subscriptionSourceRequest struct {
	Action string  `json:"action"`
	ID     string  `json:"id,omitempty"`
	Label  string  `json:"label,omitempty"`
	URL    *string `json:"url,omitempty"`
}

func newCustomSubscriptionSourceID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return customSubscriptionSourcePrefix + hex.EncodeToString(b), nil
}

func (s *statsIntegration) serveSubscriptionSources(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		s.deleteSubscriptionSource(w, r, id)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		http.Error(w, "JSON required", http.StatusUnsupportedMediaType)
		return
	}
	var req subscriptionSourceRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil || dec.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	switch req.Action {
	case "save":
		s.saveSubscriptionSource(w, r, req)
	case "refresh":
		s.refreshSubscriptionSource(w, r, req)
	default:
		http.Error(w, "invalid request", http.StatusBadRequest)
	}
}

func (s *statsIntegration) saveSubscriptionSource(w http.ResponseWriter, r *http.Request, req subscriptionSourceRequest) {
	label := strings.TrimSpace(req.Label)
	if label == "" || len(label) > 256 {
		http.Error(w, "invalid source label", http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(req.ID)
	isNew := id == ""
	if isNew {
		var err error
		id, err = newCustomSubscriptionSourceID()
		if err != nil {
			http.Error(w, "source creation failed", http.StatusInternalServerError)
			return
		}
	}
	if !strings.HasPrefix(id, customSubscriptionSourcePrefix) || !sourceIDPattern.MatchString(id) {
		http.Error(w, "invalid source id", http.StatusBadRequest)
		return
	}
	newURL := ""
	if req.URL != nil {
		newURL = strings.TrimSpace(*req.URL)
	}
	if newURL != "" {
		if err := validateSubscriptionURL(newURL); err != nil {
			http.Error(w, "invalid subscription URL", http.StatusBadRequest)
			return
		}
	}
	if isNew && newURL == "" {
		http.Error(w, "subscription URL required", http.StatusBadRequest)
		return
	}
	s.sourceMu.Lock()
	defer s.sourceMu.Unlock()
	customSubscriptionCommitMu.Lock()
	locked := true
	defer func() {
		if locked {
			customSubscriptionCommitMu.Unlock()
		}
	}()

	path := subscriptionURLCachePath(id)
	oldURL, oldErr := os.ReadFile(path)
	if oldErr != nil && !os.IsNotExist(oldErr) {
		http.Error(w, "subscription storage unavailable", http.StatusInternalServerError)
		return
	}
	if newURL == "" {
		if oldErr != nil || validateSubscriptionURL(strings.TrimSpace(string(oldURL))) != nil {
			http.Error(w, "subscription URL required", http.StatusBadRequest)
			return
		}
	}
	if newURL != "" {
		if err := writePrivateFile(path, newURL+"\n"); err != nil {
			http.Error(w, "subscription storage unavailable", http.StatusInternalServerError)
			return
		}
	}
	src := SourceConfig{ID: id, Label: label, Type: SourceTypeSubscription, Enabled: true}
	if err := persistCustomSubscriptionSource(configPath(), src, isNew); err != nil {
		if newURL != "" {
			if oldErr == nil {
				_ = writePrivateFile(path, string(oldURL))
			} else {
				_ = os.Remove(path)
			}
		}
		status := http.StatusInternalServerError
		if err == errCustomSubscriptionRejected {
			status = http.StatusBadRequest
		}
		http.Error(w, "source save failed", status)
		return
	}
	customSubscriptionCommitMu.Unlock()
	locked = false
	if s.registerSubscription == nil || !s.registerSubscription(r.Context(), src, true) {
		// Keep the durable source and its private URL; the daemon may be in its
		// startup/shutdown edge. The next launch discovers it from config.json.
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "saved"})
		return
	}
	status := "saved"
	if st, ok := subscriptionStatusSnapshot(id); ok && st.Status == "ok" {
		status = "ok"
	} else {
		status = "refreshing"
	}
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "status": status})
}

func (s *statsIntegration) refreshSubscriptionSource(w http.ResponseWriter, r *http.Request, req subscriptionSourceRequest) {
	id := strings.TrimSpace(req.ID)
	if !strings.HasPrefix(id, customSubscriptionSourcePrefix) || !sourceIDPattern.MatchString(id) {
		http.Error(w, "invalid source id", http.StatusBadRequest)
		return
	}
	s.sourceMu.Lock()
	src, ok, err := readCustomSubscriptionSource(configPath(), id)
	s.sourceMu.Unlock()
	if err != nil {
		http.Error(w, "source unavailable", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	if s.registerSubscription == nil || !s.registerSubscription(r.Context(), src, true) {
		http.Error(w, "subscription runtime unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "queued"})
}

// deleteSubscriptionSource removes a user-added subscription together with its
// private URL and node cache, then regenerates the engine so its nodes leave
// the pool immediately. Only custom-sub sources are removable; built-in
// profile/subscription entries are managed elsewhere.
func (s *statsIntegration) deleteSubscriptionSource(w http.ResponseWriter, r *http.Request, id string) {
	if !strings.HasPrefix(id, customSubscriptionSourcePrefix) || !sourceIDPattern.MatchString(id) {
		http.Error(w, "invalid source id", http.StatusBadRequest)
		return
	}
	s.sourceMu.Lock()
	defer s.sourceMu.Unlock()
	s.sourceApplyMu.RLock()
	apply := s.sourceApply
	s.sourceApplyMu.RUnlock()
	if apply == nil {
		http.Error(w, "source runtime unavailable; changes not applied", http.StatusServiceUnavailable)
		return
	}
	// applySourceConfigChange serializes on customSubscriptionCommitMu itself;
	// take no second copy of that lock here (it is not reentrant).
	err := apply(r.Context(), func() error {
		doc, sources, mode, err := readConfigSources(configPath())
		if err != nil {
			return err
		}
		remaining := make([]SourceConfig, 0, len(sources))
		found := false
		for _, src := range sources {
			if src.ID == id {
				if src.Type != SourceTypeSubscription {
					return errCustomSubscriptionProtected
				}
				found = true
				continue
			}
			remaining = append(remaining, src)
		}
		if !found {
			return os.ErrNotExist
		}
		return writeConfigSources(configPath(), doc, remaining, mode)
	})
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		http.NotFound(w, r)
		return
	case errors.Is(err, errCustomSubscriptionProtected):
		http.Error(w, "source is not removable", http.StatusBadRequest)
		return
	default:
		http.Error(w, "config write failed", http.StatusInternalServerError)
		return
	}
	// The refresher notices the missing source on its next tick and stops.
	_ = os.Remove(subscriptionURLCachePath(id))
	_ = os.Remove(filepath.Join(appSupportDir(), "sources", id+".yaml"))
	subscriptionStatusMu.Lock()
	delete(subscriptionStatuses, id)
	subscriptionStatusMu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "deleted"})
}

var errCustomSubscriptionProtected = errors.New("source is protected")

var errCustomSubscriptionRejected = os.ErrInvalid

func readConfigSources(path string) (map[string]json.RawMessage, []SourceConfig, os.FileMode, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, 0, err
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(b, &doc) != nil || len(doc) == 0 {
		return nil, nil, 0, errCustomSubscriptionRejected
	}
	raw, ok := doc["sources"]
	if !ok || json.Unmarshal(raw, new([]SourceConfig)) != nil {
		return nil, nil, 0, errCustomSubscriptionRejected
	}
	var sources []SourceConfig
	if json.Unmarshal(raw, &sources) != nil {
		return nil, nil, 0, errCustomSubscriptionRejected
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, 0, err
	}
	return doc, sources, info.Mode().Perm(), nil
}

func readCustomSubscriptionSource(path, id string) (SourceConfig, bool, error) {
	_, sources, _, err := readConfigSources(path)
	if err != nil {
		return SourceConfig{}, false, err
	}
	for _, src := range sources {
		if src.ID == id && src.Type == SourceTypeSubscription && strings.HasPrefix(src.ID, customSubscriptionSourcePrefix) {
			return src, true, nil
		}
	}
	return SourceConfig{}, false, nil
}

func persistCustomSubscriptionSource(path string, src SourceConfig, isNew bool) error {
	doc, sources, mode, err := readConfigSources(path)
	if err != nil {
		return err
	}
	found := false
	for i := range sources {
		if sources[i].ID != src.ID {
			continue
		}
		if isNew || sources[i].Type != SourceTypeSubscription || !strings.HasPrefix(sources[i].ID, customSubscriptionSourcePrefix) {
			return errCustomSubscriptionRejected
		}
		sources[i].Label = src.Label
		found = true
	}
	if !found {
		if !isNew {
			return errCustomSubscriptionRejected
		}
		sources = append(sources, src)
	}
	raw, err := json.Marshal(sources)
	if err != nil {
		return err
	}
	doc["sources"] = raw
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config-subscription-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(out, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// writeConfigSources rewrites only the sources array while preserving every
// other key and the file mode.
func writeConfigSources(path string, doc map[string]json.RawMessage, sources []SourceConfig, mode os.FileMode) error {
	raw, err := json.Marshal(sources)
	if err != nil {
		return err
	}
	doc["sources"] = raw
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-subscription-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(out, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// serveOffice91Module applies the opt-in flag and regenerates the engine in the
// same transaction, so disabling it immediately removes 玖壹 nodes instead of
// leaving them in the running config until the next unrelated reload.
func (s *statsIntegration) serveOffice91Module(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		http.Error(w, "JSON required", http.StatusUnsupportedMediaType)
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if dec.Decode(&body) != nil || dec.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	s.sourceMu.Lock()
	defer s.sourceMu.Unlock()
	s.sourceApplyMu.RLock()
	apply := s.sourceApply
	s.sourceApplyMu.RUnlock()
	if apply == nil {
		http.Error(w, "source runtime unavailable; changes not applied", http.StatusServiceUnavailable)
		return
	}
	doc, sources, mode, err := readConfigSources(configPath())
	if err != nil {
		http.Error(w, "source unavailable", http.StatusInternalServerError)
		return
	}
	if body.Enabled {
		doc["office91_enabled"] = json.RawMessage("true")
	} else {
		doc["office91_enabled"] = json.RawMessage("false")
	}
	if err := apply(r.Context(), func() error {
		return writeConfigSources(configPath(), doc, sources, mode)
	}); err != nil {
		http.Error(w, "config write failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"enabled": body.Enabled, "status": "applied"})
}

// serveSourcesUpdate asks every enabled, refreshable source to fetch its
// subscription now. Each refresher uses a direct transport first and falls back
// to the configured local proxies, so an update does not depend on the engine
// it feeds.
func (s *statsIntegration) serveSourcesUpdate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		running, total, remaining, started, updated := sourceUpdateSnapshot()
		percent := 100
		if total > 0 {
			percent = (total - remaining) * 100 / total
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"running": running, "total": total, "remaining": remaining,
			"percent": percent, "started_at": started, "updated_at": updated,
		})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	doc, ok := loadNodeSources()
	if !ok {
		http.Error(w, "sources not configured", http.StatusConflict)
		return
	}
	var ids []string
	skipped := 0
	for _, src := range doc.Sources {
		if !src.Enabled || src.Type == SourceTypeVPN {
			continue
		}
		if src.ID == sourceIDOffice91 && !office91ModuleEnabled() {
			skipped++
			continue
		}
		if !sourceRefreshAvailable(src.ID) {
			skipped++
			continue
		}
		ids = append(ids, src.ID)
	}
	beginSourceUpdate(ids)
	queued := 0
	for _, id := range ids {
		if requestSourceRefresh(id) {
			queued++
		}
	}
	// A batch that could not be started must not report a stale percentage.
	if queued == 0 {
		beginSourceUpdate(nil)
	}
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]int{"queued": queued, "skipped": skipped, "total": len(ids)})
}
