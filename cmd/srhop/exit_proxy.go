package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// exitProbe verifies a complete HTTPS request, not merely a listening socket.
var exitProbe = probeExitProxy
var exitSnapshot = snapshotService
var exitSetup = netSetup

func probeExitProxy(p ProxySetting, socks bool, target string, timeout time.Duration) error {
	if !p.Enabled || p.Auth || p.Server == "" || p.Port < 1 || p.Port > 65535 {
		return fmt.Errorf("invalid or authenticated proxy")
	}
	u, err := url.Parse(target)
	if err != nil || u == nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("invalid HTTPS probe URL")
	}
	addr := net.JoinHostPort(p.Server, strconv.Itoa(p.Port))
	d := &net.Dialer{Timeout: timeout}
	tr := &http.Transport{TLSHandshakeTimeout: timeout, ResponseHeaderTimeout: timeout, DisableKeepAlives: true, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	if socks {
		tr.DialContext = func(ctx context.Context, network, destination string) (net.Conn, error) {
			c, err := d.DialContext(ctx, "tcp", addr)
			if err != nil {
				return nil, err
			}
			good := false
			defer func() {
				if !good {
					c.Close()
				}
			}()
			deadline := time.Now().Add(timeout)
			if v, ok := ctx.Deadline(); ok && v.Before(deadline) {
				deadline = v
			}
			_ = c.SetDeadline(deadline)
			if _, err = c.Write([]byte{5, 1, 0}); err != nil {
				return nil, err
			}
			b := make([]byte, 2)
			if _, err = io.ReadFull(c, b); err != nil || b[0] != 5 || b[1] != 0 {
				return nil, fmt.Errorf("SOCKS authentication negotiation failed")
			}
			host, ps, err := net.SplitHostPort(destination)
			if err != nil || len(host) > 255 {
				return nil, fmt.Errorf("invalid SOCKS destination")
			}
			port, err := strconv.Atoi(ps)
			if err != nil {
				return nil, err
			}
			req := append([]byte{5, 1, 0, 3, byte(len(host))}, []byte(host)...)
			req = append(req, byte(port>>8), byte(port))
			if _, err = c.Write(req); err != nil {
				return nil, err
			}
			r := bufio.NewReader(c)
			h := make([]byte, 4)
			if _, err = io.ReadFull(r, h); err != nil || h[0] != 5 || h[1] != 0 {
				return nil, fmt.Errorf("SOCKS connect failed")
			}
			n := 0
			switch h[3] {
			case 1:
				n = 4
			case 4:
				n = 16
			case 3:
				v, e := r.ReadByte()
				if e != nil {
					return nil, e
				}
				n = int(v)
			default:
				return nil, fmt.Errorf("invalid SOCKS reply")
			}
			if _, err = io.CopyN(io.Discard, r, int64(n+2)); err != nil {
				return nil, err
			}
			if r.Buffered() != 0 {
				return nil, fmt.Errorf("unexpected SOCKS data")
			}
			_ = c.SetDeadline(time.Time{})
			good = true
			return c, nil
		}
	} else {
		tr.Proxy = http.ProxyURL(&url.URL{Scheme: "http", Host: addr})
		tr.DialContext = d.DialContext
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(target)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("probe returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// safeExitAll never treats a reachable port as proof of usable internet access.
func safeExitAll(services []ServiceSnapshot, httpListen, socksListen string) error {
	host, hp, err := splitListen(httpListen)
	if err != nil {
		return err
	}
	_, sp, err := splitListen(socksListen)
	if err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	target := cfg.ProbeURL
	if target == "" {
		target = defaultConfig().ProbeURL
	}
	type key struct {
		p     ProxySetting
		socks bool
	}
	cache := map[key]bool{}
	usable := func(p ProxySetting, socks bool) bool {
		if !p.Enabled || p.Auth || p.Server == "" || p.Port <= 0 || (p.Server == host && (p.Port == hp || p.Port == sp)) {
			return false
		}
		k := key{p, socks}
		if ok, seen := cache[k]; seen {
			return ok
		}
		ok := exitProbe(p, socks, target, 5*time.Second) == nil
		cache[k] = ok
		return ok
	}
	id, err := exitSessionID(services, httpListen, socksListen)
	if err != nil {
		return fmt.Errorf("identify exit session: %w", err)
	}
	journal, err := loadExitJournal(id)
	if err != nil {
		return err
	}
	fallbackHost, fallbackPort, err := splitListen(cfg.FallbackAddr)
	if err != nil {
		return fmt.Errorf("invalid fallback address: %w", err)
	}
	fallback := ProxySetting{Enabled: true, Server: fallbackHost, Port: fallbackPort}
	// Probe the configured external fallback, but prefer each valid original.
	fallbackHTTP := usable(fallback, false)
	fallbackSOCKS := usable(fallback, true)
	var errs []string
	for _, old := range services {
		cur, e := exitSnapshot(old.Name)
		if e != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", old.Name, e))
			continue
		}
		// Do not change a service if PAC or bypass was externally changed.
		if cur.PACURL != old.PACURL || (cur.PACEnabled && !old.PACEnabled) || (cur.AutoDiscovery && !old.AutoDiscovery) || (!reflect.DeepEqual(cur.Bypass, old.Bypass) && !reflect.DeepEqual(cur.Bypass, mergeBypass(old.Bypass))) {
			errs = append(errs, old.Name+": PAC, autodiscovery or bypass changed externally")
			continue
		}
		type field struct {
			label, set, state string
			original, current ProxySetting
			port              int
			socks             bool
		}
		fields := []field{{"web", "-setwebproxy", "-setwebproxystate", old.Web, cur.Web, hp, false}, {"secure", "-setsecurewebproxy", "-setsecurewebproxystate", old.Secure, cur.Secure, hp, false}, {"socks", "-setsocksfirewallproxy", "-setsocksfirewallproxystate", old.Socks, cur.Socks, sp, true}}
		for _, f := range fields {
			key := old.Name + "\x00" + f.label
			intent, recorded := journal.Fields[key]
			if recorded {
				if f.current == intent.After {
					if !intent.Done {
						intent.Done = true
						journal.Fields[key] = intent
						if e := persistExitJournal(journal); e != nil {
							errs = append(errs, fmt.Sprintf("%s: %s: persist completion: %v", old.Name, f.label, e))
							continue
						}
					}
					if !intent.After.Enabled {
						fmt.Fprintf(os.Stderr, "WARNING %s: %s proxy disabled; traffic may connect directly without proxy protection\n", old.Name, f.label)
					}
					continue
				}
				if intent.Done || f.current != intent.Before {
					errs = append(errs, old.Name+": "+f.label+": changed externally after handoff intent")
					continue
				}
			}
			owned := f.current.Enabled && !f.current.Auth && f.current.Server == host && f.current.Port == f.port
			if f.current == f.original && !owned {
				continue
			}
			// A disabled endpoint without a journal is not proof of our write.
			if !owned {
				errs = append(errs, old.Name+": "+f.label+": changed externally")
				continue
			}
			dest := f.original
			good := usable(dest, f.socks)
			if !good {
				if (f.socks && fallbackSOCKS) || (!f.socks && fallbackHTTP) {
					dest = fallback
					good = true
				}
			}
			if recorded {
				dest = intent.After
				good = dest.Enabled
			} else {
				if !good {
					dest = f.current
					dest.Enabled = false
				}
				intent = exitIntent{Before: f.current, After: dest}
				journal.Fields[key] = intent
				if e := persistExitJournal(journal); e != nil {
					delete(journal.Fields, key)
					errs = append(errs, fmt.Sprintf("%s: %s: persist intent: %v", old.Name, f.label, e))
					continue
				}
			}
			// Re-read immediately before each mutation; networksetup has no atomic CAS.
			fresh, e := exitSnapshot(old.Name)
			if e != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", old.Name, e))
				continue
			}
			var actual ProxySetting
			switch f.label {
			case "web":
				actual = fresh.Web
			case "secure":
				actual = fresh.Secure
			default:
				actual = fresh.Socks
			}
			if actual != f.current || fresh.PACURL != cur.PACURL || fresh.PACEnabled != cur.PACEnabled || fresh.AutoDiscovery != cur.AutoDiscovery || !reflect.DeepEqual(fresh.Bypass, cur.Bypass) {
				errs = append(errs, old.Name+": "+f.label+": changed externally")
				continue
			}
			if good {
				_, e = exitSetup(f.set, old.Name, dest.Server, strconv.Itoa(dest.Port))
			} else {
				_, e = exitSetup(f.state, old.Name, "off")
			}
			if e != nil {
				errs = append(errs, fmt.Sprintf("%s: %s: %v", old.Name, f.label, e))
				continue
			}
			verified, e := exitSnapshot(old.Name)
			if e != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", old.Name, e))
				continue
			}
			switch f.label {
			case "web":
				actual = verified.Web
			case "secure":
				actual = verified.Secure
			default:
				actual = verified.Socks
			}
			if good && actual != dest || !good && (actual.Enabled || actual.Server != host || actual.Port != f.port || actual.Auth) {
				errs = append(errs, old.Name+": "+f.label+": readback mismatch")
				continue
			}
			intent.Done = true
			journal.Fields[key] = intent
			if e := persistExitJournal(journal); e != nil {
				errs = append(errs, fmt.Sprintf("%s: %s: persist completion: %v", old.Name, f.label, e))
				continue
			}
			if !good {
				fmt.Fprintf(os.Stderr, "WARNING %s: %s proxy disabled; traffic may connect directly without proxy protection\n", old.Name, f.label)
			}
		}
		if old.PACEnabled || old.AutoDiscovery {
			// Legacy snapshots predate the admission check. Restore their exact
			// flags only when the URL and previously disabled values still match.
			for _, item := range []struct {
				needed, current bool
				command         string
			}{{old.PACEnabled, cur.PACEnabled, "-setautoproxystate"}, {old.AutoDiscovery, cur.AutoDiscovery, "-setproxyautodiscovery"}} {
				if !item.needed || item.current {
					continue
				}
				fresh, e := exitSnapshot(old.Name)
				if e != nil || fresh.PACURL != old.PACURL || fresh.PACEnabled != cur.PACEnabled || fresh.AutoDiscovery != cur.AutoDiscovery {
					errs = append(errs, old.Name+": PAC/autodiscovery changed externally or unreadable")
					continue
				}
				if _, e = exitSetup(item.command, old.Name, "on"); e != nil {
					errs = append(errs, fmt.Sprintf("%s: restore %s: %v", old.Name, item.command, e))
					continue
				}
				verified, e := exitSnapshot(old.Name)
				if e != nil || verified.PACURL != old.PACURL || (item.command == "-setautoproxystate" && !verified.PACEnabled) || (item.command == "-setproxyautodiscovery" && !verified.AutoDiscovery) {
					errs = append(errs, old.Name+": PAC/autodiscovery readback mismatch")
					continue
				}
				cur.PACEnabled = verified.PACEnabled
				cur.AutoDiscovery = verified.AutoDiscovery
			}
		}
		if !reflect.DeepEqual(cur.Bypass, old.Bypass) {
			fresh, e := exitSnapshot(old.Name)
			if e != nil || !reflect.DeepEqual(fresh.Bypass, cur.Bypass) || fresh.PACURL != cur.PACURL || fresh.PACEnabled != cur.PACEnabled || fresh.AutoDiscovery != cur.AutoDiscovery {
				errs = append(errs, old.Name+": bypass changed externally or could not be read")
			} else {
				domains := old.Bypass
				if len(domains) == 0 {
					domains = []string{"Empty"}
				}
				_, e = exitSetup(append([]string{"-setproxybypassdomains", old.Name}, domains...)...)
				if e != nil {
					errs = append(errs, fmt.Sprintf("%s: bypass: %v", old.Name, e))
				} else if verified, e := exitSnapshot(old.Name); e != nil || !reflect.DeepEqual(verified.Bypass, old.Bypass) {
					errs = append(errs, old.Name+": bypass readback mismatch")
				}
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}
