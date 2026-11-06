package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maxBlockedDomainsBody = 1 << 20

func blockedDomainPath() string { return filepath.Join(appSupportDir(), "blocked-domains.json") }

type blockedDomainList struct {
	Domains []string `json:"domains"`
}

// normalizeDomain accepts ASCII DNS hostnames, not URLs, addresses, or patterns.
func normalizeDomain(s string) (string, error) {
	for _, c := range s {
		if c > 127 {
			return "", fmt.Errorf("invalid domain")
		}
	}
	s = strings.ToLower(strings.TrimSuffix(s, "."))
	if len(s) == 0 || len(s) > 253 || net.ParseIP(s) != nil {
		return "", fmt.Errorf("invalid domain")
	}
	labels := strings.Split(s, ".")
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("invalid domain")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", fmt.Errorf("invalid domain")
			}
		}
	}
	// Numeric final labels can be interpreted as noncanonical IPv4 addresses.
	last := labels[len(labels)-1]
	if last[0] >= '0' && last[0] <= '9' {
		return "", fmt.Errorf("invalid domain")
	}
	return s, nil
}

func validateBlockedDomains(domains []string) (map[string]struct{}, error) {
	if domains == nil {
		return nil, fmt.Errorf("domains must be an array")
	}
	m := make(map[string]struct{}, len(domains))
	for _, d := range domains {
		n, err := normalizeDomain(d)
		if err != nil {
			return nil, fmt.Errorf("invalid domain %q", d)
		}
		m[n] = struct{}{}
	}
	return m, nil
}

func sortedBlockedDomains(m map[string]struct{}) []string {
	result := make([]string, 0, len(m))
	for d := range m {
		result = append(result, d)
	}
	sort.Strings(result)
	return result
}

func decodeBlockedDomains(r io.Reader) (map[string]struct{}, error) {
	var req blockedDomainList
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return nil, err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("invalid trailing data")
	}
	return validateBlockedDomains(req.Domains)
}

func readBlockedDomains(path string) (map[string]struct{}, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return map[string]struct{}{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return decodeBlockedDomains(io.LimitReader(f, maxBlockedDomainsBody+1))
}

func writeBlockedDomains(path string, domains map[string]struct{}) error {
	b, err := json.Marshal(blockedDomainList{Domains: sortedBlockedDomains(domains)})
	if err != nil {
		return err
	}
	if len(b)+1 > maxBlockedDomainsBody {
		return fmt.Errorf("blocked domain list too large")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".blocked-domains-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func (a *App) domainBlocked(host string) bool {
	n, err := normalizeDomain(host)
	if err != nil {
		return false
	}
	a.blockedMu.RLock()
	_, ok := a.blockedDomains[n]
	a.blockedMu.RUnlock()
	return ok
}

func (a *App) serveBlockedDomains(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut && r.Method != http.MethodPost && r.Method != http.MethodDelete {
		w.Header().Set("Allow", "GET, PUT, POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Method == http.MethodPost || r.Method == http.MethodDelete {
		if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
			http.Error(w, "JSON required", http.StatusUnsupportedMediaType)
			return
		}
		var req struct {
			Domain string `json:"domain"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBlockedDomainsBody))
		dec.DisallowUnknownFields()
		if dec.Decode(&req) != nil || dec.Decode(&struct{}{}) != io.EOF {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		domain, err := normalizeDomain(req.Domain)
		if err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		a.blockedMu.Lock()
		defer a.blockedMu.Unlock()
		m := make(map[string]struct{}, len(a.blockedDomains)+1)
		for d := range a.blockedDomains {
			m[d] = struct{}{}
		}
		if r.Method == http.MethodPost {
			m[domain] = struct{}{}
		} else {
			delete(m, domain)
		}
		if err := writeBlockedDomains(blockedDomainPath(), m); err != nil {
			http.Error(w, "preference write failed", http.StatusInternalServerError)
			return
		}
		a.blockedDomains = m
		_ = json.NewEncoder(w).Encode(blockedDomainList{Domains: sortedBlockedDomains(m)})
		return
	}
	if r.Method == http.MethodPut {
		if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
			http.Error(w, "JSON required", http.StatusUnsupportedMediaType)
			return
		}
		m, err := decodeBlockedDomains(http.MaxBytesReader(w, r.Body, maxBlockedDomainsBody))
		if err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		a.blockedMu.Lock()
		defer a.blockedMu.Unlock()
		if err := writeBlockedDomains(blockedDomainPath(), m); err != nil {
			http.Error(w, "preference write failed", http.StatusInternalServerError)
			return
		}
		a.blockedDomains = m
		_ = json.NewEncoder(w).Encode(blockedDomainList{Domains: sortedBlockedDomains(m)})
		return
	}
	a.blockedMu.RLock()
	domains := sortedBlockedDomains(a.blockedDomains)
	a.blockedMu.RUnlock()
	_ = json.NewEncoder(w).Encode(blockedDomainList{Domains: domains})
}
