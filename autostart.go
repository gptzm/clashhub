package main

import (
	"fmt"
	"time"
)

// autoProxyAllowed is intentionally stricter than interactive on: unattended
// startup must not displace a third-party proxy, PAC, or discovery setting.
func autoProxyAllowed(services []ServiceSnapshot, cfg Config) error {
	host, httpPort, err := splitListen(cfg.HTTPListen)
	if err != nil {
		return fmt.Errorf("invalid HTTP listener: %w", err)
	}
	_, socksPort, err := splitListen(cfg.SOCKSListen)
	if err != nil {
		return fmt.Errorf("invalid SOCKS listener: %w", err)
	}
	if host != "127.0.0.1" {
		return fmt.Errorf("automatic startup requires a 127.0.0.1 listener")
	}
	if err := validateRoutingConfig(cfg); err != nil {
		return err
	}
	fallbackHost, fallbackPort, err := splitListen(cfg.FallbackAddr)
	if err != nil {
		return fmt.Errorf("invalid fallback address: %w", err)
	}
	for _, s := range services {
		if s.PACEnabled || s.AutoDiscovery {
			return fmt.Errorf("%s: PAC or proxy autodiscovery enabled", s.Name)
		}
		for _, item := range []struct {
			name    string
			p       ProxySetting
			ownPort int
		}{
			{"HTTP", s.Web, httpPort}, {"HTTPS", s.Secure, httpPort}, {"SOCKS", s.Socks, socksPort},
		} {
			p := item.p
			if p.Auth {
				return fmt.Errorf("%s: %s authenticated proxy", s.Name, item.name)
			}
			if p.Enabled && !((p.Server == "127.0.0.1" && p.Port == item.ownPort) || (p.Server == fallbackHost && p.Port == fallbackPort)) {
				return fmt.Errorf("%s: %s proxy %s:%d is not permitted", s.Name, item.name, p.Server, p.Port)
			}
		}
	}
	return nil
}

// Only inspection errors are retried; an actual conflict stops immediately.
func autoSnapshotWithRetry(cfg Config, attempts int, delay time.Duration) ([]ServiceSnapshot, error) {
	return autoSnapshotRetry(cfg, attempts, delay, snapshotAllServices, time.Sleep)
}

func autoSnapshotRetry(cfg Config, attempts int, delay time.Duration, snapshot func() ([]ServiceSnapshot, error), sleep func(time.Duration)) ([]ServiceSnapshot, error) {
	var err error
	for i := 0; i < attempts; i++ {
		var services []ServiceSnapshot
		services, err = snapshot()
		if err == nil {
			if err = autoProxyAllowed(services, cfg); err != nil {
				return nil, err
			}
			return services, nil
		}
		if i+1 < attempts {
			sleep(delay)
		}
	}
	return nil, fmt.Errorf("network service inspection failed after %d attempts: %w", attempts, err)
}
