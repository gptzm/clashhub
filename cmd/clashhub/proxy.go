package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

const maxBufferedBody = 8 << 20

var hopHeaders = map[string]bool{
	"Connection":          true,
	"Proxy-Connection":    true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

func (a *App) startHTTPProxy() error {
	ln, err := net.Listen("tcp", a.cfg.HTTPListen)
	if err != nil {
		return fmt.Errorf("listen http %s: %w", a.cfg.HTTPListen, err)
	}
	if err := captureRemove(); err != nil {
		ln.Close()
		return fmt.Errorf("clear stale capture: %w", err)
	}
	api, err := a.nodeAPIHandler()
	if err != nil {
		ln.Close()
		return err
	}
	api, err = a.captureAPI(api)
	if err != nil {
		ln.Close()
		return err
	}
	a.httpSrv = &http.Server{
		Handler:           api,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		err := a.httpSrv.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.logf("http proxy server stopped: %v", err)
		}
	}()
	return nil
}

func (a *App) startSOCKSProxy() error {
	host, _, err := net.SplitHostPort(a.cfg.SOCKSListen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return fmt.Errorf("listen socks %q: literal loopback address required", a.cfg.SOCKSListen)
	}
	ln, err := net.Listen("tcp", a.cfg.SOCKSListen)
	if err != nil {
		return fmt.Errorf("listen socks %s: %w", a.cfg.SOCKSListen, err)
	}
	a.socksLn = ln
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if !a.beginProxyWork() {
				_ = c.Close()
				continue
			}
			if !a.trackProxySockets(c) {
				a.proxyWork.Done()
				continue
			}
			go func() { defer a.proxyWork.Done(); defer a.untrackProxySockets(c); a.handleSOCKS(c) }()
		}
	}()
	return nil
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !a.beginProxyWork() {
		http.Error(w, "clashhub is shutting down", http.StatusServiceUnavailable)
		return
	}
	defer a.proxyWork.Done()
	if a.shuttingDown.Load() {
		http.Error(w, "clashhub is shutting down", http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodConnect {
		a.handleConnect(w, r)
		return
	}
	if isUpgrade(r) {
		a.handleUpgrade(w, r)
		return
	}
	a.handleHTTP(w, r)
}

func isUpgrade(r *http.Request) bool {
	return r.Header.Get("Upgrade") != "" ||
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

func (a *App) handleConnect(w http.ResponseWriter, r *http.Request) {
	target := r.Host
	if a.domainBlocked(hostOnly(target)) {
		http.Error(w, "clashhub: domain blocked", http.StatusForbidden)
		return
	}
	if !strings.Contains(target, ":") {
		target = net.JoinHostPort(target, "443")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	started := time.Now()
	gen, port, capture := a.capture.match(target)
	conn, upstream, err := a.dialForTarget(ctx, target)
	if err != nil {
		if capture {
			a.capture.record(gen, captureEvent{Time: started, Kind: "connection", Port: port, Result: "error", DurationMS: time.Since(started).Milliseconds()})
		}
		a.logf("CONNECT %s failed via %s: %v", target, upstream, err)
		http.Error(w, "clashhub: "+err.Error(), http.StatusBadGateway)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = conn.Close()
		http.Error(w, "clashhub: hijacking unsupported", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hj.Hijack()
	if err != nil {
		_ = conn.Close()
		a.logf("hijack for %s failed: %v", target, err)
		return
	}
	if !a.trackProxySockets(client, conn) {
		return
	}
	defer a.untrackProxySockets(client, conn)
	defer client.Close()
	defer conn.Close()
	_ = client.SetDeadline(time.Time{})
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		_ = client.Close()
		_ = conn.Close()
		return
	}
	if buffered.Reader.Buffered() > 0 {
		client = &bufferedConn{Conn: client, r: buffered.Reader}
	}
	a.relay(client, conn, upstream, target)
}

func (a *App) waitForMode(ctx context.Context) Mode {
	for {
		mode, probeEnd := a.snapshotMode()
		if a.directRouting() {
			return mode
		}
		if mode == ModeProbing {
			if time.Now().Before(probeEnd) {
				select {
				case <-ctx.Done():
					return mode
				case <-time.After(150 * time.Millisecond):
				}
				continue
			}
			return mode
		}
		return mode
	}
}

func (a *App) fallbackRoundTrip(r *http.Request, generation uint64) (*http.Response, error) {
	return a.routingRoundTrip(r, a.fallbackAddr(), generation)
}

func (a *App) handleHTTP(w http.ResponseWriter, r *http.Request) {
	gen, port, capture := a.capture.match(r.URL.Host)
	if capture {
		started := time.Now()
		if port == "" {
			if r.URL.Scheme == "https" {
				port = "443"
			} else {
				port = "80"
			}
		}
		cw := &captureResponseWriter{ResponseWriter: w}
		w = cw
		var upload atomic.Int64
		if r.Body != nil {
			r.Body = &captureReadCloser{ReadCloser: r.Body, count: &upload}
		}
		defer func() {
			status := cw.status
			if status == 0 {
				status = 200
			}
			result := "completed"
			if status >= 500 {
				result = "error"
			}
			path := r.URL.EscapedPath()
			if path == "" {
				path = "/"
			}
			if len(path) > 2048 {
				path = ""
			}
			a.capture.record(gen, captureEvent{Time: started, Kind: "http", Port: port, Result: result, DurationMS: time.Since(started).Milliseconds(), Upload: upload.Load(), Download: cw.bytes, Method: captureMethod(r.Method), Path: path, Status: status})
		}()
	}
	if !r.URL.IsAbs() {
		http.Error(w, "clashhub: absolute-form request required", http.StatusBadRequest)
		return
	}
	if a.domainBlocked(r.URL.Hostname()) {
		http.Error(w, "clashhub: domain blocked", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	bodyBytes, streamBody, retryable, err := readRequestBody(r)
	if err != nil {
		http.Error(w, "clashhub: read request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	makeReq := func(upstream string) *http.Request {
		out := r.Clone(ctx)
		out.RequestURI = ""
		out.Header = r.Header.Clone()
		removeHopByHop(out.Header)
		if retryable {
			out.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		} else {
			out.Body = streamBody
		}
		out.Body = statsReader{reader: out.Body, stats: a.stats, target: r.URL.Host, port: selectedPort(upstream), onBytes: a.fallbackTrafficCallback(upstream)}
		out.GetBody = nil
		out.Close = false
		// The destination authority is also the only authority sent upstream.
		out.Host = r.URL.Host
		return out
	}

	mode := ModePrimary
	routingMode, requestToken, routingErr := a.snapshotRoutingState()
	generation := requestToken.routing
	if routingErr != nil {
		http.Error(w, "clashhub: routing preference unavailable", http.StatusServiceUnavailable)
		return
	}
	direct := routingMode == "direct"
	if !direct {
		mode = a.waitForMode(ctx)
		routingMode, requestToken, routingErr = a.snapshotRoutingState()
		generation = requestToken.routing
		if routingErr != nil {
			http.Error(w, "clashhub: routing preference unavailable", http.StatusServiceUnavailable)
			return
		}
		direct = routingMode == "direct"
	}
	if ctx.Err() != nil {
		http.Error(w, "clashhub: request canceled", http.StatusGatewayTimeout)
		return
	}

	if !direct && mode != ModePrimary && mode != ModeFallback {
		http.Error(w, "clashhub: routing not ready", http.StatusServiceUnavailable)
		return
	}
	fallbackAddr := a.fallbackAddr()
	first := fallbackAddr
	if direct {
		first = "direct"
	} else if mode == ModePrimary {
		first = a.cfg.PrimaryAddr
	}
	var resp *http.Response
	if first == fallbackAddr && first != "direct" {
		resp, err = a.fallbackRoundTrip(makeReq(first), generation)
	} else if !direct {
		resp, err = a.routingRoundTrip(makeReq(first), first, generation)
	} else {
		resp, err = a.transportFor(first).RoundTrip(makeReq(first))
	}
	if err == nil {
		a.writeResponse(w, resp, r.URL.Host, first, ctx, requestToken)
		return
	}
	a.logf("HTTP %s via %s failed: %v", r.URL.Host, first, err)
	if !direct && ctx.Err() == nil {
		// This is only a health signal. The node-health loop owns selector
		// changes, and retry policy below remains limited to replay-safe methods.
		a.scheduleFailureScan(requestToken)
	}
	// A transport failure may follow server processing. Only explicitly safe
	// methods with replayable bodies may try the other configured route once.
	if !direct && retryable && retrySafeMethod(r.Method) && ctx.Err() == nil &&
		(first == fallbackAddr || a.allowFallbackRetry(ctx, r.URL.Host, requestToken)) {
		other := fallbackAddr
		if first == fallbackAddr {
			other = a.cfg.PrimaryAddr
		}
		resp, err = a.routingRoundTrip(makeReq(other), other, generation)
		if err == nil {
			a.writeResponse(w, resp, r.URL.Host, other, ctx, requestToken)
			return
		}
		if ctx.Err() == nil {
			a.scheduleFailureScan(requestToken)
		}
	}
	http.Error(w, "clashhub: "+err.Error(), http.StatusBadGateway)
}
func retrySafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

func readRequestBody(r *http.Request) ([]byte, io.ReadCloser, bool, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, http.NoBody, true, nil
	}
	if r.ContentLength >= 0 && r.ContentLength <= maxBufferedBody {
		b, err := io.ReadAll(io.LimitReader(r.Body, maxBufferedBody+1))
		if err != nil {
			return nil, nil, false, err
		}
		if len(b) <= maxBufferedBody {
			return b, nil, true, nil
		}
		// Oversized: prepend what we read to the remaining stream.
		return nil, io.NopCloser(io.MultiReader(bytes.NewReader(b), r.Body)), false, nil
	}
	return nil, r.Body, false, nil
}

func (a *App) writeResponse(w http.ResponseWriter, resp *http.Response, target, upstream string, ctx context.Context, token probeToken) {
	defer resp.Body.Close()
	removeHopByHop(resp.Header)
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	reader := &upstreamBodyReader{reader: resp.Body}
	_, copyErr := io.Copy(statsWriter{writer: w, stats: a.stats, target: target, port: selectedPort(upstream), onBytes: a.fallbackTrafficCallback(upstream)}, reader)
	if upstream != "direct" && ctx.Err() == nil && isUpstreamBodyFailure(reader.err) && errors.Is(copyErr, reader.err) {
		a.scheduleFailureScan(token)
	}
	if copyErr != nil {
		// Headers are already committed. Abort the HTTP stream rather than
		// allowing a truncated body to look like a complete response.
		panic(http.ErrAbortHandler)
	}
}

func removeHopByHop(h http.Header) {
	for _, conn := range h.Values("Connection") {
		for _, token := range strings.Split(conn, ",") {
			token = strings.TrimSpace(token)
			if token != "" {
				h.Del(token)
			}
		}
	}
	for k := range h {
		if hopHeaders[http.CanonicalHeaderKey(k)] {
			h.Del(k)
		}
	}
}

func (a *App) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	if r.URL.Scheme != "http" {
		http.Error(w, "clashhub: only plain ws upgrade is supported without CONNECT", http.StatusBadGateway)
		return
	}
	target := r.URL.Host
	if a.domainBlocked(r.URL.Hostname()) {
		http.Error(w, "clashhub: domain blocked", http.StatusForbidden)
		return
	}
	if !strings.Contains(target, ":") {
		target = net.JoinHostPort(target, "80")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, upstream, err := a.dialForTarget(ctx, target)
	if err != nil {
		a.logf("upgrade %s failed via %s: %v", target, upstream, err)
		http.Error(w, "clashhub: "+err.Error(), http.StatusBadGateway)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = conn.Close()
		http.Error(w, "clashhub: hijacking unsupported", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hj.Hijack()
	if err != nil {
		_ = conn.Close()
		return
	}

	if !a.trackProxySockets(client, conn) {
		return
	}
	defer a.untrackProxySockets(client, conn)
	defer client.Close()
	defer conn.Close()
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\n", r.Method, r.URL.RequestURI())
	fmt.Fprintf(&b, "Host: %s\r\n", r.URL.Host)
	for k, vv := range r.Header {
		if strings.EqualFold(k, "Proxy-Connection") || strings.EqualFold(k, "Proxy-Authorization") {
			continue
		}
		for _, v := range vv {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	b.WriteString("\r\n")
	if _, err := conn.Write(b.Bytes()); err != nil {
		_ = client.Close()
		_ = conn.Close()
		return
	}
	_ = client.SetDeadline(time.Time{})
	if buffered.Reader.Buffered() > 0 {
		client = &bufferedConn{Conn: client, r: buffered.Reader}
	}
	a.relay(client, conn, upstream, target)
}

func (a *App) handleSOCKS(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))

	br := bufio.NewReader(c)
	ver, err := br.ReadByte()
	if err != nil || ver != 0x05 {
		return
	}
	nmethods, err := br.ReadByte()
	if err != nil {
		return
	}
	methods := make([]byte, int(nmethods))
	if _, err := io.ReadFull(br, methods); err != nil {
		return
	}
	if !bytes.Contains(methods, []byte{0x00}) {
		_, _ = c.Write([]byte{0x05, 0xff})
		return
	}
	if _, err := c.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	hdr := make([]byte, 4)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return
	}
	if hdr[0] != 0x05 {
		return
	}
	if hdr[1] != 0x01 {
		_, _ = c.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}

	var host string
	switch hdr[3] {
	case 0x01:
		b := make([]byte, 4)
		if _, err := io.ReadFull(br, b); err != nil {
			return
		}
		host = net.IP(b).String()
	case 0x03:
		l, err := br.ReadByte()
		if err != nil {
			return
		}
		b := make([]byte, int(l))
		if _, err := io.ReadFull(br, b); err != nil {
			return
		}
		host = string(b)
	case 0x04:
		b := make([]byte, 16)
		if _, err := io.ReadFull(br, b); err != nil {
			return
		}
		host = net.IP(b).String()
	default:
		_, _ = c.Write([]byte{0x05, 0x08, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(br, pb); err != nil {
		return
	}
	if a.domainBlocked(host) {
		_, _ = c.Write([]byte{0x05, 0x02, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	port := int(binary.BigEndian.Uint16(pb))
	target := net.JoinHostPort(host, fmt.Sprintf("%d", port))

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	started := time.Now()
	gen, capturePort, capture := a.capture.match(target)
	conn, upstream, err := a.dialForTarget(ctx, target)
	if err != nil {
		if capture {
			a.capture.record(gen, captureEvent{Time: started, Kind: "connection", Port: capturePort, Result: "error", DurationMS: time.Since(started).Milliseconds()})
		}
		a.logf("SOCKS %s failed via %s: %v", target, upstream, err)
		_, _ = c.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	if _, err := c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		_ = conn.Close()
		return
	}
	_ = c.SetDeadline(time.Time{})

	var client net.Conn = c
	if br.Buffered() > 0 {
		client = &bufferedConn{Conn: c, r: br}
	}
	a.relay(client, conn, upstream, target)
}
