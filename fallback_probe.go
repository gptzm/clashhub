package main

import (
	"context"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	fallbackProbeURL      = "https://www.gstatic.com/generate_204"
	fallbackProbeInterval = time.Minute
	fallbackProbeMaxAge   = 2 * time.Minute
	fallbackProbeTimeout  = 10 * time.Second
)

type fallbackProbeResult struct {
	Status    string     `json:"status"`
	DelayMS   *int64     `json:"delay_ms"`
	CheckedAt *time.Time `json:"checked_at"`
	ExpiresAt *time.Time `json:"expires_at"`
}

type fallbackProbe struct {
	mu     sync.Mutex
	result fallbackProbeResult
}

func (p *fallbackProbe) snapshot(now time.Time) fallbackProbeResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.result
	if r.Status == "" {
		r.Status = "pending"
	}
	if r.ExpiresAt != nil && !now.Before(*r.ExpiresAt) {
		r.Status = "expired"
	}
	return r
}

// run is the sole producer. Reads never initiate network requests, and each
// interval starts after the preceding request completes, preventing overlaps.
func (p *fallbackProbe) run(ctx context.Context, proxyAddr, target string, interval, timeout time.Duration) {
	proxyURL := &url.URL{Scheme: "http", Host: proxyAddr}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		var resp *http.Response
		if err == nil {
			resp, err = client.Do(req)
		}
		if resp != nil {
			resp.Body.Close()
		}
		if ctx.Err() != nil {
			return
		}
		checked := time.Now().UTC()
		expires := checked.Add(fallbackProbeMaxAge)
		r := fallbackProbeResult{Status: "failed", CheckedAt: &checked, ExpiresAt: &expires}
		if err == nil && resp != nil && resp.StatusCode == http.StatusNoContent {
			delay := time.Since(start).Milliseconds()
			r.Status, r.DelayMS = "ok", &delay
		}
		p.mu.Lock()
		p.result = r
		p.mu.Unlock()
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
}

func (d *nodeDiagnostics) startFallbackProbe(a *App) {
	if a == nil || a.stopCh == nil || len(a.fallbackList()) == 0 {
		return
	}
	d.mu.Lock()
	if d.fallback != nil {
		d.mu.Unlock()
		return
	}
	p := &fallbackProbe{}
	d.fallback = p
	d.mu.Unlock()
	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-a.stopCh:
				cancel()
			case <-done:
			}
		}()
		p.run(ctx, a.fallbackAddr(), fallbackProbeURL, fallbackProbeInterval, fallbackProbeTimeout)
	}()
}
