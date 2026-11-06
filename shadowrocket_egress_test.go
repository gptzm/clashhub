package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func traceServer(loc, ip string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprintf(w, "fl=1\nip=%s\nloc=%s\n", ip, loc)
	}))
}

func TestEvaluateEgressTraceSkipsEOFAndAcceptsJP(t *testing.T) {
	var eofCalls atomic.Int32
	eof := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		eofCalls.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer eof.Close()
	jp := traceServer("JP", "203.0.113.10")
	defer jp.Close()

	ip, loc, err := evaluateEgressTrace(context.Background(), &http.Client{Timeout: time.Second}, []string{eof.URL, jp.URL})
	if err != nil {
		t.Fatalf("EOF source should not hide a valid source: %v", err)
	}
	if ip != "203.0.113.10" || loc != "JP" {
		t.Fatalf("got ip=%q loc=%q", ip, loc)
	}
	if eofCalls.Load() != 1 {
		t.Fatalf("EOF endpoint calls=%d", eofCalls.Load())
	}
}

func TestEvaluateEgressTraceReturnsNonJPAndRejectsRegionConflict(t *testing.T) {
	jp := traceServer("JP", "203.0.113.11")
	defer jp.Close()
	us := traceServer("US", "203.0.113.12")
	defer us.Close()

	ip, loc, err := evaluateEgressTrace(context.Background(), &http.Client{Timeout: time.Second}, []string{jp.URL, us.URL})
	if err == nil || !strings.Contains(err.Error(), "region conflict") {
		t.Fatalf("expected explicit region conflict, ip=%q loc=%q err=%v", ip, loc, err)
	}
	if ip != "" || loc != "" {
		t.Fatalf("conflict returned a usable egress: ip=%q loc=%q", ip, loc)
	}

	ip, loc, err = evaluateEgressTrace(context.Background(), &http.Client{Timeout: time.Second}, []string{us.URL})
	if err != nil || ip != "203.0.113.12" || loc != "US" {
		t.Fatalf("expected identifiable non-JP result, ip=%q loc=%q err=%v", ip, loc, err)
	}
}

func TestEvaluateEgressTraceMalformedSourceFallsThrough(t *testing.T) {
	malformed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ip=not-an-ip\nloc=JP\n")
	}))
	defer malformed.Close()
	jp := traceServer("JP", "2001:db8::10")
	defer jp.Close()

	ip, loc, err := evaluateEgressTrace(context.Background(), &http.Client{Timeout: time.Second}, []string{malformed.URL, jp.URL})
	if err != nil {
		t.Fatalf("malformed source should be treated as unknown: %v", err)
	}
	if ip != "2001:db8::10" || loc != "JP" {
		t.Fatalf("got ip=%q loc=%q", ip, loc)
	}
}

func TestEvaluateEgressTraceDoesNotReusePreviousResult(t *testing.T) {
	var failed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failed.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = io.WriteString(w, "ip=203.0.113.13\nloc=JP\n")
	}))
	defer srv.Close()
	client := &http.Client{Timeout: time.Second}

	if _, loc, err := evaluateEgressTrace(context.Background(), client, []string{srv.URL}); err != nil || loc != "JP" {
		t.Fatalf("initial trace failed: loc=%q err=%v", loc, err)
	}
	failed.Store(true)
	ip, loc, err := evaluateEgressTrace(context.Background(), client, []string{srv.URL})
	if err == nil || ip != "" || loc != "" {
		t.Fatalf("stale trace was reused: ip=%q loc=%q err=%v", ip, loc, err)
	}
}

func TestEvaluateEgressTraceKeepsTLSVerification(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ip=203.0.113.14\nloc=JP\n")
	}))
	defer srv.Close()

	// The default client does not trust httptest's certificate. If this test ever
	// succeeds, an insecure TLS bypass has accidentally entered the verifier.
	ip, loc, err := evaluateEgressTrace(context.Background(), &http.Client{Timeout: time.Second}, []string{srv.URL})
	if err == nil || ip != "" || loc != "" {
		t.Fatalf("untrusted TLS endpoint was accepted: ip=%q loc=%q err=%v", ip, loc, err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "certificate") && !strings.Contains(strings.ToLower(err.Error()), "tls") {
		t.Fatalf("unexpected TLS error: %v", err)
	}
}

func TestEvaluateEgressTraceHonorsSharedCancellation(t *testing.T) {
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer blocked.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	ip, loc, err := evaluateEgressTrace(ctx, &http.Client{}, []string{blocked.URL})
	if err == nil || ip != "" || loc != "" {
		t.Fatalf("canceled trace returned success: ip=%q loc=%q err=%v", ip, loc, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("shared cancellation took too long: %s", elapsed)
	}
}

func TestProbeStatusContextCancellation(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer proxy.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	code, _, err := probeStatusContext(ctx, strings.TrimPrefix(proxy.URL, "http://"), "http://target.test/", http.StatusOK, 2*time.Second)
	if err == nil || code != 0 {
		t.Fatalf("canceled status probe returned code=%d err=%v", code, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("status cancellation took too long: %s", elapsed)
	}
}

func TestEgressThroughContextCancellationSharesBudget(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer proxy.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	ip, loc, err := egressThroughContextTargets(ctx, strings.TrimPrefix(proxy.URL, "http://"), 2*time.Second, []string{
		"http://trace-one.test/cdn-cgi/trace",
		"http://trace-two.test/cdn-cgi/trace",
	})
	if err == nil || ip != "" || loc != "" {
		t.Fatalf("canceled egress returned success: ip=%q loc=%q err=%v", ip, loc, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("egress cancellation took too long: %s", elapsed)
	}
}

func TestProbeStatusRejectsResponseBodyReadError(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 8\r\nConnection: close\r\n\r\nshort")
		_ = conn.Close()
	}))
	defer proxy.Close()

	code, _, err := probeStatusContext(context.Background(), strings.TrimPrefix(proxy.URL, "http://"), "http://body-error.test/", http.StatusOK, time.Second)
	if code != http.StatusOK || err == nil || !strings.Contains(err.Error(), "read response body") {
		t.Fatalf("body read error was not surfaced: code=%d err=%v", code, err)
	}
}

func TestProbeAndEgressClientsDoNotFollowRedirects(t *testing.T) {
	var probeFinalCalls, egressFinalCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Host {
		case "redirect.test":
			w.Header().Set("Location", "http://probe-final.test/")
			w.WriteHeader(http.StatusFound)
		case "probe-final.test":
			probeFinalCalls.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case "redirect-trace.test":
			w.Header().Set("Location", "http://egress-final.test/cdn-cgi/trace")
			w.WriteHeader(http.StatusFound)
		case "egress-final.test":
			egressFinalCalls.Add(1)
			_, _ = io.WriteString(w, "ip=203.0.113.20\nloc=JP\n")
		default:
			http.Error(w, "unexpected target", http.StatusBadGateway)
		}
	}))
	defer proxy.Close()
	proxyAddr := strings.TrimPrefix(proxy.URL, "http://")

	code, _, err := probeStatusContext(context.Background(), proxyAddr, "http://redirect.test/", http.StatusNoContent, time.Second)
	if code != http.StatusFound || err == nil || !strings.Contains(err.Error(), "expected HTTP 204") {
		t.Fatalf("probe redirect was not rejected as original status: code=%d err=%v", code, err)
	}
	if probeFinalCalls.Load() != 0 {
		t.Fatal("probe client followed redirect")
	}

	ip, loc, err := egressThroughContextTargets(context.Background(), proxyAddr, time.Second, []string{"http://redirect-trace.test/cdn-cgi/trace"})
	if err == nil || ip != "" || loc != "" || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("egress redirect was not rejected: ip=%q loc=%q err=%v", ip, loc, err)
	}
	if egressFinalCalls.Load() != 0 {
		t.Fatal("egress client followed redirect")
	}
}
