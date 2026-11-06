package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const maxSourceURL = 4096

func validSubscriptionURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxSourceURL || strings.ContainsAny(raw, " \t\r\n") {
		return false
	}
	return strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://")
}

// configuredSourceURL returns srhop's own override for a source id.
func configuredSourceURL(id string) string {
	b, err := os.ReadFile(configPath())
	if err != nil {
		return ""
	}
	var doc struct {
		Sources []SourceConfig `json:"sources"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return ""
	}
	for _, s := range doc.Sources {
		if s.ID == id {
			return strings.TrimSpace(s.URL)
		}
	}
	return ""
}

// persistSourceURL sets or clears one source's subscription override. Every
// other key in the config document is preserved byte-for-byte.
func persistSourceURL(path, id, rawURL string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(b, &doc) != nil || len(doc) == 0 {
		return fmt.Errorf("invalid config document")
	}
	var entries []map[string]json.RawMessage
	if json.Unmarshal(doc["sources"], &entries) != nil {
		return fmt.Errorf("invalid config sources")
	}
	found := false
	for _, e := range entries {
		var eid string
		if json.Unmarshal(e["id"], &eid) != nil || eid != id {
			continue
		}
		found = true
		if rawURL == "" {
			delete(e, "url")
			break
		}
		val, err := json.Marshal(rawURL)
		if err != nil {
			return err
		}
		e["url"] = val
		break
	}
	if !found {
		return fmt.Errorf("source not found")
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	doc["sources"] = encoded
	out, err := json.MarshalIndent(doc, "", " ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(append(out, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

// serveSourceSubscription reads or updates one source's subscription address.
// The URL is only returned to the authenticated local caller and never logged.
func (a *App) serveSourceSubscription(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if r.Method == http.MethodPut {
		var req struct {
			ID  string `json:"id"`
			URL string `json:"url"`
		}
		if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
			http.Error(w, "JSON required", http.StatusUnsupportedMediaType)
			return
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		dec.DisallowUnknownFields()
		if dec.Decode(&req) != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		req.ID = strings.TrimSpace(req.ID)
		req.URL = strings.TrimSpace(req.URL)
		if req.ID == "" || (req.URL != "" && !validSubscriptionURL(req.URL)) {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if err := persistSourceURL(configPath(), req.ID, req.URL); err != nil {
			http.Error(w, "config write failed", http.StatusInternalServerError)
			return
		}
		status := "saved"
		if requestSourceRefresh(req.ID) {
			status = "queued"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": status, "source_id": req.ID})
		return
	}
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	url := configuredSourceURL(id)
	editable := true
	if url == "" {
		// Fall back to FlClash's own record for profile sources.
		var doc struct {
			Sources []SourceConfig `json:"sources"`
		}
		if b, err := os.ReadFile(configPath()); err == nil && json.Unmarshal(b, &doc) == nil {
			for _, s := range doc.Sources {
				if s.ID != id || s.Path == "" {
					continue
				}
				base := filepath.Base(filepath.Clean(s.Path))
				profileID := strings.TrimSuffix(base, filepath.Ext(base))
				if urls, err := readFlClashProfileURLs(flclashDatabasePath()); err == nil {
					url = urls[profileID]
				}
				break
			}
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "url": url, "editable": editable})
}
