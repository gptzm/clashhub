package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

func vpnStatus(service string) string {
	out, err := exec.Command("scutil", "--nc", "status", service).Output()
	if err != nil {
		return "Unknown"
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if line == "" {
		return "Unknown"
	}
	return line
}

func vpnStart(service string) error {
	if vpnStatus(service) == "Connected" {
		return nil
	}
	out, err := exec.Command("scutil", "--nc", "start", service).CombinedOutput()
	if err != nil {
		return fmt.Errorf("scutil --nc start %s: %w: %s", service, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func vpnStop(service string) error {
	if vpnStatus(service) == "Disconnected" {
		return nil
	}
	out, err := exec.Command("scutil", "--nc", "stop", service).CombinedOutput()
	if err != nil {
		return fmt.Errorf("scutil --nc stop %s: %w: %s", service, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func portOpen(addr string, timeout time.Duration) bool {
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func waitPort(addr string, wantOpen bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if portOpen(addr, 400*time.Millisecond) == wantOpen {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return portOpen(addr, 400*time.Millisecond) == wantOpen
}

func probeThrough(proxyAddr, target string, timeout time.Duration) (int, int64, error) {
	return probeThroughContext(context.Background(), proxyAddr, target, timeout)
}

// probeThroughContext is the cancelable form used by bounded health probes.
// The timeout is a total budget for the request, not an additional wait after
// the caller's context is canceled.
func probeThroughContext(ctx context.Context, proxyAddr, target string, timeout time.Duration) (int, int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	pu, err := url.Parse("http://" + proxyAddr)
	if err != nil {
		return 0, 0, err
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyURL(pu),
		DialContext:           (&net.Dialer{Timeout: timeout}).DialContext,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		DisableKeepAlives:     true,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Transport: tr,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("User-Agent", "clashhub/1.0")
	req.Header.Set("Cache-Control", "no-cache")

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, time.Since(start).Milliseconds(), err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 8192)); err != nil {
		return resp.StatusCode, time.Since(start).Milliseconds(), fmt.Errorf("read response body: %w", err)
	}
	ms := time.Since(start).Milliseconds()

	if resp.StatusCode == http.StatusProxyAuthRequired {
		return resp.StatusCode, ms, fmt.Errorf("upstream requires proxy auth")
	}
	if resp.StatusCode >= 500 {
		return resp.StatusCode, ms, fmt.Errorf("upstream returned %s", resp.Status)
	}
	return resp.StatusCode, ms, nil
}

// probeExpectedCode derives the status a configured health URL must answer.
// A 204 endpoint is strict; any other page is only required to answer 2xx.
// Requiring 204 for an ordinary page marks every healthy probe as a failure.
func probeExpectedCode(target string) int {
	if strings.Contains(strings.ToLower(target), "generate_204") {
		return http.StatusNoContent
	}
	return 0
}

// nodeProbeExpectedCode is the exact code handed to mihomo's /delay endpoint,
// which needs one concrete expectation rather than "any 2xx".
func nodeProbeExpectedCode(target string) int {
	if code := probeExpectedCode(target); code != 0 {
		return code
	}
	return http.StatusOK
}

// independentProbeTargets are additional ordinary targets used to tell "this
// node cannot reach one site" apart from "the whole route is down". Nodes
// differ: a Japanese node may reach Google but not gstatic, or vice versa.
var independentProbeTargets = []string{
	"https://www.apple.com/library/test/success.html",
	"https://www.google.com",
	"https://www.cloudflare.com/cdn-cgi/trace",
}

// ordinaryProbeTargets returns the configured target first, then independent
// fallbacks, without duplicates.
func ordinaryProbeTargets(primary string) []string {
	out := []string{primary}
	seen := map[string]bool{primary: true}
	for _, t := range independentProbeTargets {
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

func probeStatus(proxyAddr, target string, want int, timeout time.Duration) (int, int64, error) {
	return probeStatusContext(context.Background(), proxyAddr, target, want, timeout)
}

// probeStatusContext preserves probeStatus' status validation while allowing a
// caller to cancel the whole probe attempt (including proxy connect, TLS and
// response-body reads).
func probeStatusContext(ctx context.Context, proxyAddr, target string, want int, timeout time.Duration) (int, int64, error) {
	code, ms, err := probeThroughContext(ctx, proxyAddr, target, timeout)
	if err != nil {
		return code, ms, err
	}
	if want == 0 {
		if code < 200 || code > 299 {
			return code, ms, fmt.Errorf("expected HTTP 2xx, got %d", code)
		}
		return code, ms, nil
	}
	if code != want {
		return code, ms, fmt.Errorf("expected HTTP %d, got %d", want, code)
	}
	return code, ms, nil
}

func connectedVPNs() []string {
	out, err := exec.Command("scutil", "--nc", "list").Output()
	if err != nil {
		return nil
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "(Connected)") {
			continue
		}
		if m := vpnNameRe.FindStringSubmatch(line); len(m) == 2 {
			names = append(names, m[1])
		}
	}
	return names
}

// egressTraceTargets are independent HTTPS names which expose the same
// Cloudflare /cdn-cgi/trace format. The IP URL is deliberately kept as an
// HTTPS URL: the default transport must validate its certificate, just as it
// does for the named endpoints. A certificate or network failure is an
// unknown result and does not turn into a guessed location.
var egressTraceTargets = []string{
	"https://www.cloudflare.com/cdn-cgi/trace",
	"https://cloudflare.com/cdn-cgi/trace",
	"https://one.one.one.one/cdn-cgi/trace",
	"https://1.1.1.1/cdn-cgi/trace",
}

// egressTraceResult is one endpoint's independently observed trace. An error
// means that endpoint provided no usable geographic evidence and the caller
// should continue with the other endpoints.
type egressTraceResult struct {
	target string
	ip     string
	loc    string
	err    error
}

// egressThrough preserves the existing primary egress API. It verifies all
// configured trace endpoints within one timeout budget so a single Cloudflare
// hostname's transient EOF cannot make the whole route look non-JP.
func egressThrough(proxyAddr string, timeout time.Duration) (string, string, error) {
	return egressThroughContext(context.Background(), proxyAddr, timeout)
}

// egressThroughContext is the cancelable form of egressThrough. One shared
// context deadline covers all trace endpoints, so a recovery attempt cannot
// spend the full timeout once per endpoint.
func egressThroughContext(ctx context.Context, proxyAddr string, timeout time.Duration) (string, string, error) {
	return egressThroughContextTargets(ctx, proxyAddr, timeout, egressTraceTargets)
}

func egressThroughContextTargets(ctx context.Context, proxyAddr string, timeout time.Duration, targets []string) (string, string, error) {
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	if ctx == nil {
		ctx = context.Background()
	}
	pu, err := url.Parse("http://" + proxyAddr)
	if err != nil {
		return "", "", err
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyURL(pu),
		DialContext:           (&net.Dialer{Timeout: timeout}).DialContext,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		DisableKeepAlives:     true,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Transport: tr,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return evaluateEgressTrace(ctx, client, targets)
}

// evaluateEgressTrace is separated from proxy construction so endpoint
// failures, malformed trace bodies, and region conflicts can be tested without
// weakening the production TLS configuration. All endpoints are queried before
// deciding: a valid non-JP result is never hidden by a later JP result.
func evaluateEgressTrace(ctx context.Context, client *http.Client, targets []string) (string, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if client == nil {
		return "", "", fmt.Errorf("egress trace client unavailable")
	}
	// Production callers pass a deadline shared by all endpoints. Keep the
	// testable helper bounded as well when a caller supplies a background
	// context and a client without its own timeout; every goroutine below must
	// observe the same cancellation rather than outliving this call.
	if _, ok := ctx.Deadline(); !ok {
		budget := 8 * time.Second
		if client.Timeout > 0 {
			budget = client.Timeout
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}
	if len(targets) == 0 {
		return "", "", fmt.Errorf("no egress trace endpoints configured")
	}

	unique := make([]string, 0, len(targets))
	seen := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		target = strings.TrimSpace(target)
		if target == "" {
			continue
		}
		if _, ok := seen[target]; ok {
			continue
		}
		seen[target] = struct{}{}
		unique = append(unique, target)
	}
	if len(unique) == 0 {
		return "", "", fmt.Errorf("no egress trace endpoints configured")
	}

	results := make(chan egressTraceResult, len(unique))
	for _, target := range unique {
		target := target
		go func() { results <- probeEgressTrace(ctx, client, target) }()
	}
	ordered := make([]egressTraceResult, len(unique))
	byTarget := make(map[string]egressTraceResult, len(unique))
	for range unique {
		result := <-results
		byTarget[result.target] = result
	}
	for i, target := range unique {
		ordered[i] = byTarget[target]
	}

	var jp []egressTraceResult
	var nonJP []egressTraceResult
	var unknown []egressTraceResult
	for _, result := range ordered {
		switch {
		case result.err != nil:
			unknown = append(unknown, result)
		case strings.EqualFold(result.loc, "JP"):
			jp = append(jp, result)
		default:
			nonJP = append(nonJP, result)
		}
	}

	// A valid non-JP observation is explicit evidence and must never be
	// converted into JP success by another endpoint. A standalone non-JP
	// result is returned to the compatibility callers (doctor and egressCheck),
	// which can display it or apply RequireJapan policy themselves. A JP/non-JP
	// disagreement fails closed instead of making a geographic guess.
	if len(nonJP) > 0 {
		locations := make([]string, 0, len(nonJP))
		for _, result := range nonJP {
			locations = append(locations, result.loc)
		}
		if len(jp) > 0 {
			return "", "", fmt.Errorf("egress region conflict: JP and %s", strings.Join(locations, ", "))
		}
		return nonJP[0].ip, nonJP[0].loc, nil
	}
	if len(jp) > 0 {
		return jp[0].ip, "JP", nil
	}

	// Do not retain or reuse a previous answer. Every endpoint was attempted in
	// this call; if all results are unknown, the route remains unverified.
	errs := make([]string, 0, len(unknown))
	for _, result := range unknown {
		errs = append(errs, result.target+": "+result.err.Error())
	}
	if len(errs) == 0 {
		return "", "", fmt.Errorf("no valid egress trace result")
	}
	return "", "", fmt.Errorf("all egress trace endpoints unavailable: %s", strings.Join(errs, "; "))
}

func probeEgressTrace(ctx context.Context, client *http.Client, target string) egressTraceResult {
	result := egressTraceResult{target: target}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		result.err = fmt.Errorf("request invalid: %w", err)
		return result
	}
	req.Header.Set("User-Agent", "clashhub/1.0")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := client.Do(req)
	if err != nil {
		result.err = err
		return result
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		result.err = fmt.Errorf("trace returned HTTP %d", resp.StatusCode)
		return result
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if err != nil {
		result.err = fmt.Errorf("read trace: %w", err)
		return result
	}
	fields := make(map[string]string)
	for _, line := range strings.Split(string(body), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		fields[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	ip := fields["ip"]
	loc := strings.ToUpper(fields["loc"])
	if net.ParseIP(ip) == nil {
		result.err = fmt.Errorf("trace has invalid ip")
		return result
	}
	if !validTraceLocation(loc) {
		result.err = fmt.Errorf("trace has invalid loc")
		return result
	}
	result.ip, result.loc = ip, loc
	return result
}

func validTraceLocation(loc string) bool {
	if len(loc) != 2 {
		return false
	}
	return loc[0] >= 'A' && loc[0] <= 'Z' && loc[1] >= 'A' && loc[1] <= 'Z'
}

// dialViaHTTPProxy opens a CONNECT tunnel through a local HTTP proxy.
func dialViaHTTPProxy(proxyAddr, target string, timeout time.Duration) (net.Conn, error) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	c, err := net.DialTimeout("tcp", proxyAddr, timeout)
	if err != nil {
		return nil, fmt.Errorf("dial upstream %s: %w", proxyAddr, err)
	}
	_ = c.SetDeadline(time.Now().Add(timeout))
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Connection: Keep-Alive\r\n\r\n", target, target)
	if _, err := io.WriteString(c, req); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("write CONNECT: %w", err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("read CONNECT response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = c.Close()
		return nil, fmt.Errorf("upstream %s CONNECT %s: %s", proxyAddr, target, resp.Status)
	}
	// Do not close resp.Body: for CONNECT 200 it may wrap the connection.
	_ = c.SetDeadline(time.Time{})
	return &bufferedConn{Conn: c, r: br}, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *bufferedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	// A transport without half-close must be fully closed to unblock the peer.
	return c.Conn.Close()
}
