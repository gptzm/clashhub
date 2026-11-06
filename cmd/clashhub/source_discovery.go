package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"

	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func readFlClashProfileLabels(db string) (map[string]string, error) {
	dbh, err := openSQLiteReadOnly(db)
	if err != nil {
		return nil, fmt.Errorf("discovery-failed")
	}
	defer dbh.Close()
	rows, err := dbh.Query("SELECT id, label FROM profiles WHERE type='url' ORDER BY id LIMIT 10001")
	if err != nil {
		return nil, fmt.Errorf("discovery-failed")
	}
	defer rows.Close()
	labels := map[string]string{}
	for rows.Next() {
		var id int64
		var label sql.NullString
		if err := rows.Scan(&id, &label); err != nil || id < 0 {
			continue
		}
		if label.Valid && strings.TrimSpace(label.String) != "" {
			labels[strconv.FormatInt(id, 10)] = label.String
		} else {
			labels[strconv.FormatInt(id, 10)] = ""
		}
	}
	if rows.Err() != nil {
		return nil, fmt.Errorf("discovery-failed")
	}
	return labels, nil
}

// reconcileDiscoveredSources appends FlClash URL profiles that clashhub neither
// configured by id nor by their profile path. Existing entries keep their
// order, labels, enablement and priority; nothing is removed or overridden.
// It only touches clashhub's own config file, never FlClash's files or database.
func reconcileDiscoveredSources(path string) ([]SourceConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(b, &doc) != nil {
		return nil, fmt.Errorf("invalid config document")
	}
	rawSources, ok := doc["sources"]
	if !ok || string(rawSources) == "null" {
		return nil, nil
	}
	var entries []map[string]json.RawMessage
	if json.Unmarshal(rawSources, &entries) != nil {
		return nil, fmt.Errorf("invalid config sources")
	}
	profileDir := filepath.Join(filepath.Dir(flclashDatabasePath()), "profiles")
	covered := map[string]bool{}
	for _, e := range entries {
		var id, path string
		_ = json.Unmarshal(e["id"], &id)
		_ = json.Unmarshal(e["path"], &path)
		if id != "" {
			covered[id] = true
		}
		if path != "" {
			covered["path:"+filepath.Clean(path)] = true
		}
		if path != "" && filepath.Dir(filepath.Clean(path)) == filepath.Clean(profileDir) {
			covered[strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))] = true
		}
	}
	labels, _ := readFlClashProfileLabels(flclashDatabasePath())
	ids := make([]string, 0, len(labels))
	for id := range labels {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		return len(ids[i]) < len(ids[j]) || (len(ids[i]) == len(ids[j]) && ids[i] < ids[j])
	})
	var added []SourceConfig
	for _, id := range ids {
		if covered[id] || covered["flclash-"+id] {
			continue
		}
		path := filepath.Join(profileDir, id+".yaml")
		if _, err := os.Stat(path); err != nil {
			continue
		}
		label := strings.TrimSpace(labels[id])
		if label == "" {
			label = "FlClash " + id
		}
		entry := SourceConfig{ID: "flclash-" + id, Label: label, Type: SourceTypeProfile, Path: path, Enabled: true}
		raw, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil {
			continue
		}
		entries = append(entries, obj)
		covered[id] = true
		added = append(added, entry)
	}
	for _, entry := range discoverOtherClashProfiles(userHome()) {
		if covered[entry.ID] || covered["path:"+filepath.Clean(entry.Path)] {
			continue
		}
		raw, _ := json.Marshal(entry)
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) == nil {
			entries = append(entries, obj)
			covered[entry.ID] = true
			covered["path:"+filepath.Clean(entry.Path)] = true
			added = append(added, entry)
		}
	}
	if len(added) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		return nil, err
	}
	doc["sources"] = encoded
	// Mirror the derived rollback list so the next load does not rewrite again.
	var paths []string
	for _, e := range entries {
		var typ, p string
		enabled := true
		_ = json.Unmarshal(e["type"], &typ)
		_ = json.Unmarshal(e["path"], &p)
		if raw, ok := e["enabled"]; ok {
			_ = json.Unmarshal(raw, &enabled)
		}
		if enabled && typ == SourceTypeProfile && p != "" {
			paths = append(paths, p)
		}
	}
	if len(paths) > 0 {
		if raw, err := json.Marshal(paths); err == nil {
			doc["source_profiles"] = raw
		}
	}
	out, err := json.MarshalIndent(doc, "", " ")
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return nil, err
	}
	name := tmp.Name()
	if _, err := tmp.Write(append(out, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return nil, err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return nil, err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return nil, err
	}
	return added, nil
}

// Discover only bounded, explicit profile locations. Never recurse into caches,
// providers, generated runtime files, or encrypted client databases.
func discoverOtherClashProfiles(home string) []SourceConfig {
	type location struct{ label, path string }
	locations := []location{
		{"ClashX", filepath.Join(home, ".config", "clash")},
		{"Clash Verge Rev", filepath.Join(home, "Library", "Application Support", "io.github.clash-verge-rev.clash-verge-rev", "profiles")},
	}
	var out []SourceConfig
	for _, loc := range locations {
		files, err := os.ReadDir(loc.path)
		if err != nil || len(files) > 1000 {
			continue
		}
		for _, f := range files {
			ext := strings.ToLower(filepath.Ext(f.Name()))
			if f.Type()&os.ModeSymlink != 0 || f.IsDir() || (ext != ".yaml" && ext != ".yml") {
				continue
			}
			path := filepath.Join(loc.path, f.Name())
			info, err := f.Info()
			if err != nil || !info.Mode().IsRegular() || info.Size() > 10<<20 {
				continue
			}
			body, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			if _, _, err := validateClashPayload(body); err != nil {
				continue
			}
			h := sha256.Sum256([]byte(path))
			out = append(out, SourceConfig{ID: fmt.Sprintf("local-%x", h[:12]), Label: loc.label + " · " + strings.TrimSuffix(f.Name(), filepath.Ext(f.Name())), Type: SourceTypeProfile, Path: path, Enabled: false})
		}
	}
	return out
}
