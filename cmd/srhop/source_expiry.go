package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// sourceExpiryPath caches each configured source's subscription expiry so the
// priority resolver does not have to read subscription databases per node.
func sourceExpiryPath() string {
	return filepath.Join(appSupportDir(), "source-expiry.json")
}

// readSourceExpiryCache returns source id -> unix expiry seconds. A missing or
// invalid cache simply disables the near-expiry boost.
func readSourceExpiryCache() map[string]int64 {
	b, err := os.ReadFile(sourceExpiryPath())
	if err != nil || len(b) > 1<<16 {
		return nil
	}
	var out map[string]int64
	if json.Unmarshal(b, &out) != nil || len(out) == 0 {
		return nil
	}
	for id, v := range out {
		if id == "" || v <= 0 {
			delete(out, id)
		}
	}
	return out
}

// writeSourceExpiryCache maps subscription profiles to configured source ids.
// Only expiry timestamps are stored: no URLs, credentials or node data.
func writeSourceExpiryCache(profiles []subscriptionProfile) {
	var doc struct {
		Sources []SourceConfig `json:"sources"`
	}
	if b, err := os.ReadFile(configPath()); err != nil || json.Unmarshal(b, &doc) != nil || len(doc.Sources) == 0 {
		return
	}
	pathToID := map[string]string{}
	for _, s := range doc.Sources {
		if s.Type == SourceTypeProfile && s.Path != "" {
			id := filepath.Base(filepath.Clean(s.Path))
			id = id[:len(id)-len(filepath.Ext(id))]
			pathToID[id] = s.ID
		}
	}
	out := map[string]int64{}
	for _, p := range profiles {
		if p.SubscriptionInfo.Expire == nil || *p.SubscriptionInfo.Expire <= 0 {
			continue
		}
		switch p.Source {
		case "jiuyi":
			out["office91"] = *p.SubscriptionInfo.Expire
		case "flclash":
			if id, ok := pathToID[p.ID]; ok {
				out[id] = *p.SubscriptionInfo.Expire
			} else {
				out["flclash-"+p.ID] = *p.SubscriptionInfo.Expire
			}
		}
	}
	if len(out) == 0 {
		return
	}
	current := readSourceExpiryCache()
	if len(current) == len(out) {
		same := true
		for k, v := range out {
			if current[k] != v {
				same = false
				break
			}
		}
		if same {
			return
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(sourceExpiryPath()), ".expiry-*")
	if err != nil {
		return
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return
	}
	_ = tmp.Chmod(0o600)
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return
	}
	if err := os.Rename(name, sourceExpiryPath()); err != nil {
		_ = os.Remove(name)
	}
}
