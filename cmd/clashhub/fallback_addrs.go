package main

import "strings"

// fallbackAddr is the external proxy currently preferred. It is read from many
// request paths, so it must never be empty once the app is initialized.
func (a *App) fallbackAddr() string {
	a.fallbackMu.RLock()
	defer a.fallbackMu.RUnlock()
	if a.fallbackActive != "" {
		return a.fallbackActive
	}
	return a.cfg.FallbackAddr
}

func (a *App) fallbackList() []string {
	a.fallbackMu.RLock()
	defer a.fallbackMu.RUnlock()
	if len(a.fallbackAddrs) > 0 {
		return append([]string(nil), a.fallbackAddrs...)
	}
	return []string{a.cfg.FallbackAddr}
}

// setFallbackAddrs replaces the list. The first entry becomes active; callers
// pass the already validated and de-duplicated order.
func (a *App) setFallbackAddrs(list []string) {
	clean := make([]string, 0, len(list))
	seen := map[string]bool{}
	for _, addr := range list {
		addr = strings.TrimSpace(addr)
		if addr == "" || seen[addr] {
			continue
		}
		seen[addr] = true
		clean = append(clean, addr)
	}
	if len(clean) == 0 {
		clean = []string{a.cfg.FallbackAddr}
	}
	a.fallbackMu.Lock()
	defer a.fallbackMu.Unlock()
	a.fallbackAddrs = clean
	if a.fallbackActive == "" || !seen[a.fallbackActive] {
		a.fallbackActive = clean[0]
	}
}

// rotateFallback moves to the next configured address after a failure and
// returns it, or "" when the list has only one entry.
func (a *App) rotateFallback(failed string) string {
	a.fallbackMu.Lock()
	defer a.fallbackMu.Unlock()
	if len(a.fallbackAddrs) < 2 {
		return ""
	}
	for i, addr := range a.fallbackAddrs {
		if addr != failed {
			continue
		}
		next := a.fallbackAddrs[(i+1)%len(a.fallbackAddrs)]
		if next == failed {
			return ""
		}
		a.fallbackActive = next
		return next
	}
	a.fallbackActive = a.fallbackAddrs[0]
	return a.fallbackActive
}
