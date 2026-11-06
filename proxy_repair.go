package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// repairProxyService only re-enables switches that still point at srhop. It
// never rewrites addresses, bypasses, PAC or credentials. The original restore
// snapshot remains untouched. Re-read before each write to detect a takeover.
func repairProxyService(name, httpListen, socksListen string, read func(string) (ServiceSnapshot, error), write func(...string) (string, error), stopping func() bool) (bool, error) {
	hh, hp, err := splitListen(httpListen)
	if err != nil {
		return false, err
	}
	sh, sp, err := splitListen(socksListen)
	if err != nil {
		return false, err
	}
	valid := func(s ServiceSnapshot) bool {
		match := func(p ProxySetting, host string, port int) bool {
			return !p.Auth && strings.EqualFold(p.Server, host) && p.Port == port
		}
		return !s.PACEnabled && !s.AutoDiscovery && match(s.Web, hh, hp) && match(s.Secure, hh, hp) && match(s.Socks, sh, sp)
	}
	changed := false
	for _, part := range []struct {
		command string
		enabled func(ServiceSnapshot) bool
	}{
		{"-setwebproxystate", func(s ServiceSnapshot) bool { return s.Web.Enabled }},
		{"-setsecurewebproxystate", func(s ServiceSnapshot) bool { return s.Secure.Enabled }},
		{"-setsocksfirewallproxystate", func(s ServiceSnapshot) bool { return s.Socks.Enabled }},
	} {
		if stopping() {
			return changed, fmt.Errorf("repair interrupted by shutdown")
		}
		current, err := read(name)
		if err != nil {
			return changed, err
		}
		if !valid(current) {
			return changed, fmt.Errorf("%s: proxy changed or conflicting settings; automatic repair skipped", name)
		}
		if part.enabled(current) {
			continue
		}
		if stopping() {
			return changed, fmt.Errorf("repair interrupted by shutdown")
		}
		if _, err := write(part.command, name, "on"); err != nil {
			return changed, err
		}
		current, err = read(name)
		if err != nil {
			return changed, err
		}
		if !valid(current) || !part.enabled(current) {
			return changed, fmt.Errorf("%s: proxy repair could not be verified", name)
		}
		changed = true
	}
	return changed, nil
}

func (a *App) repairSystemProxy() {
	a.applyMu.Lock()
	defer a.applyMu.Unlock()
	if a.shuttingDown.Load() {
		return
	}
	a.stateMu.Lock()
	st := *a.state
	st.Services = append([]ServiceSnapshot(nil), a.state.Services...)
	a.stateMu.Unlock()
	if !st.Active || !st.Ready || st.PID != os.Getpid() || !daemonMatches(&st) || st.AppliedHTTP != a.cfg.HTTPListen || st.AppliedSOCKS != a.cfg.SOCKSListen {
		return
	}
	if !portOpen(st.AppliedHTTP, 500*time.Millisecond) || !portOpen(st.AppliedSOCKS, 500*time.Millisecond) {
		return
	}
	vpns, err := vpnServiceNamesChecked()
	if err != nil {
		a.logf("system proxy auto-repair skipped: cannot identify VPN services: %v", err)
		return
	}
	for _, service := range st.Services {
		if vpns[service.Name] {
			a.logf("system proxy auto-repair skipped: %s is now a VPN service", service.Name)
			continue
		}
		changed, err := repairProxyService(service.Name, st.AppliedHTTP, st.AppliedSOCKS, snapshotService, netSetup, a.shuttingDown.Load)
		if err != nil {
			a.logf("system proxy auto-repair skipped: %v", err)
		} else if changed {
			a.logf("system proxy auto-repair: %s", service.Name)
		}
	}
}

func (a *App) proxyRepairLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-a.stopCh:
			return
		case <-ticker.C:
			a.repairSystemProxy()
		}
	}
}
