package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

func netSetup(args ...string) (string, error) {
	cmd := exec.Command("networksetup", args...)
	out, err := cmd.CombinedOutput()
	text := string(out)
	if err != nil {
		return text, fmt.Errorf("networksetup %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(text))
	}
	return text, nil
}

func listNetworkServices() ([]string, error) {
	out, err := netSetup("-listallnetworkservices")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "An asterisk") {
			continue
		}
		line = strings.TrimPrefix(line, "*")
		line = strings.TrimSpace(line)
		if line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

var vpnNameRe = regexp.MustCompile(`"([^"]+)"\s+\[VPN:`)

func vpnServiceNames() map[string]bool {
	names, _ := vpnServiceNamesChecked()
	return names
}

func vpnServiceNamesChecked() (map[string]bool, error) {
	out, err := exec.Command("scutil", "--nc", "list").Output()
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, m := range vpnNameRe.FindAllStringSubmatch(string(out), -1) {
		names[m[1]] = true
	}
	return names, nil
}

func parseProxy(text string) ProxySetting {
	var p ProxySetting
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch k {
		case "Enabled":
			p.Enabled = strings.EqualFold(v, "yes")
		case "Server":
			p.Server = v
		case "Port":
			p.Port, _ = strconv.Atoi(v)
		case "Authenticated Proxy Enabled":
			p.Auth = v == "1"
		}
	}
	return p
}

func parseBypass(text string) []string {
	if strings.Contains(text, "There aren't any bypass domains") {
		return nil
	}
	var out []string
	for _, line := range strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == '\n' }) {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func parsePAC(text string) (string, bool) {
	var u string
	var enabled bool
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "URL":
			v = strings.TrimSpace(v)
			if v != "(null)" {
				u = v
			}
		case "Enabled":
			enabled = strings.EqualFold(strings.TrimSpace(v), "yes")
		}
	}
	return u, enabled
}

func parseAutoDiscovery(text string) bool {
	return strings.Contains(strings.ToLower(text), "on")
}

func snapshotService(name string) (ServiceSnapshot, error) {
	s := ServiceSnapshot{Name: name}
	out, err := netSetup("-getwebproxy", name)
	if err != nil {
		return s, err
	}
	s.Web = parseProxy(out)

	out, err = netSetup("-getsecurewebproxy", name)
	if err != nil {
		return s, err
	}
	s.Secure = parseProxy(out)

	out, err = netSetup("-getsocksfirewallproxy", name)
	if err != nil {
		return s, err
	}
	s.Socks = parseProxy(out)

	out, err = netSetup("-getproxybypassdomains", name)
	if err != nil {
		return s, err
	}
	s.Bypass = parseBypass(out)

	out, err = netSetup("-getautoproxyurl", name)
	if err != nil {
		return s, err
	}
	s.PACURL, s.PACEnabled = parsePAC(out)

	out, err = netSetup("-getproxyautodiscovery", name)
	if err != nil {
		return s, err
	}
	s.AutoDiscovery = parseAutoDiscovery(out)

	return s, nil
}

func snapshotAllServices() ([]ServiceSnapshot, error) {
	names, err := listNetworkServices()
	if err != nil {
		return nil, err
	}
	vpns, err := vpnServiceNamesChecked()
	if err != nil {
		return nil, fmt.Errorf("identify VPN services: %w", err)
	}
	var out []ServiceSnapshot
	var errs []string
	for _, name := range names {
		if vpns[name] {
			continue
		}
		s, err := snapshotService(name)
		if err == nil && (s.Web.Auth || s.Secure.Auth || s.Socks.Auth) {
			err = fmt.Errorf("%s: authenticated proxy cannot be safely restored", name)
		}
		if err == nil && (s.PACEnabled || s.AutoDiscovery) {
			err = fmt.Errorf("%s: enabled PAC or proxy autodiscovery cannot be safely taken over", name)
		}
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		out = append(out, s)
	}
	if len(errs) > 0 || len(out) == 0 {
		return nil, fmt.Errorf("no usable network services: %s", strings.Join(errs, "; "))
	}
	return out, nil
}

func splitListen(addr string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, err
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return host, port, nil
}

var defaultBypass = []string{
	"localhost", "127.*", "*.local",
	"10.*", "172.16.*", "172.17.*", "172.18.*", "172.19.*",
	"172.20.*", "172.21.*", "172.22.*", "172.23.*", "172.24.*",
	"172.25.*", "172.26.*", "172.27.*", "172.28.*", "172.29.*",
	"172.30.*", "172.31.*", "192.168.*", "169.254/16",
}

func mergeBypass(existing []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, d := range existing {
		add(d)
	}
	for _, d := range defaultBypass {
		if !seen[d] {
			add(d)
		}
	}
	return out
}

func applyOneProxy(name string, s *ServiceSnapshot, httpHost string, httpPort, socksPort int) error {
	var errs []string

	step := func(args ...string) {
		if _, err := netSetup(args...); err != nil {
			errs = append(errs, err.Error())
		}
	}

	step("-setwebproxy", name, httpHost, strconv.Itoa(httpPort))
	step("-setwebproxystate", name, "on")
	step("-setsecurewebproxy", name, httpHost, strconv.Itoa(httpPort))
	step("-setsecurewebproxystate", name, "on")
	step("-setsocksfirewallproxy", name, httpHost, strconv.Itoa(socksPort))
	step("-setsocksfirewallproxystate", name, "on")

	// PAC would override the manual proxy; turn it off while srhop is in charge.
	step("-setautoproxystate", name, "off")
	step("-setproxyautodiscovery", name, "off")

	bypass := mergeBypass(s.Bypass)
	if len(bypass) > 0 {
		args := append([]string{"-setproxybypassdomains", name}, bypass...)
		step(args...)
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s: %s", name, strings.Join(errs, "; "))
	}
	return nil
}

func applyOurProxy(services []ServiceSnapshot, httpListen, socksListen string) error {
	host, port, err := splitListen(httpListen)
	if err != nil {
		return err
	}
	_, socksPort, err := splitListen(socksListen)
	if err != nil {
		return err
	}
	var errs []string
	for i := range services {
		if services[i].PACEnabled || services[i].AutoDiscovery {
			errs = append(errs, services[i].Name+": enabled PAC or proxy autodiscovery cannot be safely taken over")
			continue
		}
		current, err := snapshotService(services[i].Name)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		// Turning on takes over unconditionally: a third-party client (for
		// example the fallback client) may rewrite these settings between the
		// snapshot and now. Refusing here left the system with a partially
		// applied proxy (HTTP disabled) and no way to reach the network, so
		// always apply; the snapshot still records what to restore later.
		_ = current
		if err := applyOneProxy(services[i].Name, &services[i], host, port, socksPort); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// Restore only fields still equal to what srhop installed. A conflict or failed
// command leaves the snapshot available for a later, explicit retry.
func restoreOneProxy(s *ServiceSnapshot, httpListen, socksListen string) error {
	host, hp, err := splitListen(httpListen)
	if err != nil {
		return err
	}
	socksHost, sp, err := splitListen(socksListen)
	if err != nil {
		return err
	}
	_ = socksHost // applyOurProxy uses the HTTP host for both protocols.
	current, err := snapshotService(s.Name)
	if err != nil {
		return err
	}
	var errs []string
	step := func(args ...string) {
		if _, e := netSetup(args...); e != nil {
			errs = append(errs, e.Error())
		}
	}
	restoreProxy := func(label, set, state string, old, now ProxySetting, port int) {
		if now == old {
			return
		}
		if old.Auth {
			errs = append(errs, label+": authenticated proxy cannot be restored safely")
			return
		}
		ownedEndpoint := now.Server == host && now.Port == port && !now.Auth
		// A disabled proxy with our endpoint is the stable result of restoring
		// an originally disabled proxy whose empty address cannot be cleared.
		if !old.Enabled && old.Server == "" && old.Port <= 0 && ownedEndpoint {
			if now.Enabled {
				step(state, s.Name, "off")
			}
			fmt.Fprintf(os.Stderr, "%s: %s proxy disabled; original empty server cannot be cleared, inactive srhop endpoint remains\n", s.Name, label)
			return
		}
		if !now.Enabled || !ownedEndpoint {
			errs = append(errs, label+": changed externally")
			return
		}
		if old.Server == "" || old.Port <= 0 {
			errs = append(errs, label+": original proxy has no restorable server and port")
			return
		}
		step(set, s.Name, old.Server, strconv.Itoa(old.Port))
		step(state, s.Name, onOff(old.Enabled))
	}
	restoreProxy("web", "-setwebproxy", "-setwebproxystate", s.Web, current.Web, hp)
	restoreProxy("secure", "-setsecurewebproxy", "-setsecurewebproxystate", s.Secure, current.Secure, hp)
	restoreProxy("socks", "-setsocksfirewallproxy", "-setsocksfirewallproxystate", s.Socks, current.Socks, sp)
	if !reflect.DeepEqual(current.Bypass, s.Bypass) {
		if reflect.DeepEqual(current.Bypass, mergeBypass(s.Bypass)) {
			domains := s.Bypass
			if len(domains) == 0 {
				domains = []string{"Empty"}
			}
			step(append([]string{"-setproxybypassdomains", s.Name}, domains...)...)
		} else {
			errs = append(errs, "bypass: changed externally")
		}
	}
	// srhop changes only the enabled flags, never the PAC URL.
	if current.PACURL != s.PACURL {
		errs = append(errs, "PAC: changed externally")
	} else if current.PACEnabled != s.PACEnabled {
		if !current.PACEnabled {
			step("-setautoproxystate", s.Name, onOff(s.PACEnabled))
		} else {
			errs = append(errs, "PAC: changed externally")
		}
	}
	if current.AutoDiscovery != s.AutoDiscovery {
		if !current.AutoDiscovery {
			step("-setproxyautodiscovery", s.Name, onOff(s.AutoDiscovery))
		} else {
			errs = append(errs, "autodiscovery: changed externally")
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s: %s", s.Name, strings.Join(errs, "; "))
	}
	return nil
}

func restoreAllProxy(services []ServiceSnapshot, httpListen, socksListen string) error {
	return safeExitAll(services, httpListen, socksListen)
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

var (
	reHTTPPort  = regexp.MustCompile(`HTTPPort\s*:\s*(\d+)`)
	reSOCKSPort = regexp.MustCompile(`SOCKSPort\s*:\s*(\d+)`)
)

func currentSystemProxyPorts() (int, int) {
	out, err := exec.Command("scutil", "--proxy").Output()
	if err != nil {
		return 0, 0
	}
	text := string(out)
	httpPort, socksPort := 0, 0
	if m := reHTTPPort.FindStringSubmatch(text); len(m) == 2 {
		httpPort, _ = strconv.Atoi(m[1])
	}
	if m := reSOCKSPort.FindStringSubmatch(text); len(m) == 2 {
		socksPort, _ = strconv.Atoi(m[1])
	}
	return httpPort, socksPort
}
