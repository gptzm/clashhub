package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
)

type fallbackConfigPayload struct {
	Addresses []string `json:"addresses"`
	Active    string   `json:"active,omitempty"`
}

// serveFallback exposes the configured external fallback proxies. Addresses are
// validated host:port pairs; nothing else in the config file is rewritten.
func (a *App) serveFallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Method == http.MethodPut {
		if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
			http.Error(w, "JSON required", http.StatusUnsupportedMediaType)
			return
		}
		var req fallbackConfigPayload
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		dec.DisallowUnknownFields()
		if dec.Decode(&req) != nil || dec.Decode(&struct{}{}) != io.EOF {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if len(req.Addresses) == 0 || len(req.Addresses) > 16 {
			http.Error(w, "one to sixteen addresses are required", http.StatusBadRequest)
			return
		}
		clean := make([]string, 0, len(req.Addresses))
		seen := map[string]bool{}
		for _, addr := range req.Addresses {
			addr = strings.TrimSpace(addr)
			if validateFallbackAddr(addr) != nil {
				http.Error(w, "invalid address", http.StatusBadRequest)
				return
			}
			if seen[addr] {
				continue
			}
			seen[addr] = true
			clean = append(clean, addr)
		}
		if err := persistFallbackAddresses(configPath(), clean); err != nil {
			http.Error(w, "config write failed", http.StatusInternalServerError)
			return
		}
		a.setFallbackAddrs(clean)
		a.engineMu.Lock()
		a.advanceEngineGenerationLocked()
		a.engineMu.Unlock()
	}
	_ = json.NewEncoder(w).Encode(fallbackConfigPayload{Addresses: a.fallbackList(), Active: a.fallbackAddr()})
}

// persistFallbackAddresses writes fallback_addr and fallback_addrs while
// preserving every other key and value in the config document.
func persistFallbackAddresses(path string, addrs []string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(b, &doc) != nil || len(doc) == 0 {
		return errInvalidFallbackConfig
	}
	first, err := json.Marshal(addrs[0])
	if err != nil {
		return err
	}
	list, err := json.Marshal(addrs)
	if err != nil {
		return err
	}
	doc["fallback_addr"] = first
	doc["fallback_addrs"] = list
	out, err := json.MarshalIndent(doc, "", " ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dirOf(path), ".config-*")
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

var errInvalidFallbackConfig = &fallbackConfigError{}

type fallbackConfigError struct{}

func (*fallbackConfigError) Error() string { return "invalid config document" }

func dirOf(path string) string {
	if i := strings.LastIndex(path, "/"); i > 0 {
		return path[:i]
	}
	return "."
}
