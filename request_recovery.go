package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

type routingAttempt struct{ cancel context.CancelFunc }

// Only setup/headers are registered: established tunnels and response streams
// retain the existing lifecycle after successful handoff.
func (a *App) beginRoutingAttempt(ctx context.Context, generation uint64) (context.Context, *routingAttempt, error) {
	a.routingGate.Lock()
	defer a.routingGate.Unlock()
	if !a.routingProbeValidLocked(generation) {
		return nil, nil, fmt.Errorf("routing mode changed")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	child, cancel := context.WithCancel(ctx)
	attempt := &routingAttempt{cancel: cancel}
	if a.routingAttempts == nil {
		a.routingAttempts = map[*routingAttempt]struct{}{}
	}
	a.routingAttempts[attempt] = struct{}{}
	return child, attempt, nil
}
func (a *App) cancelRoutingAttemptsLocked() {
	for attempt := range a.routingAttempts {
		attempt.cancel()
	}
}
func (a *App) finishRoutingAttempt(attempt *routingAttempt, generation uint64) bool {
	a.routingGate.Lock()
	defer a.routingGate.Unlock()
	delete(a.routingAttempts, attempt)
	return a.routingProbeValidLocked(generation)
}
func (a *App) routingProxyDial(ctx context.Context, addr, target string, generation uint64, timeout time.Duration) (net.Conn, string, error) {
	child, attempt, err := a.beginRoutingAttempt(ctx, generation)
	if err != nil {
		return nil, "", err
	}
	defer attempt.cancel()
	started := time.Now()
	c, err := dialProxyContext(child, addr, target, timeout)
	valid := a.finishRoutingAttempt(attempt, generation)
	if !valid || ctx.Err() != nil {
		if c != nil {
			c.Close()
		}
		c = nil
		if ctx.Err() != nil {
			err = ctx.Err()
		} else {
			err = fmt.Errorf("routing mode changed")
		}
	}
	a.stats.connection(target, started, err)
	return c, addr, err
}
func (a *App) recoveryPrimaryDial(ctx context.Context, target string, generation uint64) (net.Conn, string, error) {
	timeout := time.Duration(a.cfg.PrimaryTimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return a.routingProxyDial(ctx, a.cfg.PrimaryAddr, target, generation, timeout)
}
func dialProxyContext(ctx context.Context, addr, target string, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	deadline, _ := ctx.Deadline()
	_ = c.SetDeadline(deadline)
	if _, err = io.WriteString(c, fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)); err != nil {
		stop()
		c.Close()
		return nil, err
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		stop()
		c.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		stop()
		c.Close()
		return nil, fmt.Errorf("primary CONNECT returned HTTP %d", resp.StatusCode)
	}
	if !stop() || ctx.Err() != nil {
		c.Close()
		return nil, ctx.Err()
	}
	_ = c.SetDeadline(time.Time{})
	return &bufferedConn{Conn: c, r: br}, nil
}

// Preserve the child context until the caller closes the successful response.
type routingResponseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *routingResponseBody) Close() error { defer b.cancel(); return b.ReadCloser.Close() }
func (a *App) routingRoundTrip(r *http.Request, addr string, generation uint64) (*http.Response, error) {
	child, attempt, err := a.beginRoutingAttempt(r.Context(), generation)
	if err != nil {
		return nil, err
	}
	resp, err := a.transportFor(addr).RoundTrip(r.Clone(child))
	valid := a.finishRoutingAttempt(attempt, generation)
	if !valid || r.Context().Err() != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		resp = nil
		if r.Context().Err() != nil {
			err = r.Context().Err()
		} else {
			err = fmt.Errorf("routing mode changed")
		}
	}
	if err != nil {
		attempt.cancel()
		return nil, err
	}
	resp.Body = &routingResponseBody{resp.Body, attempt.cancel}
	return resp, nil
}
func (a *App) recoveryPrimaryRoundTrip(r *http.Request, generation uint64) (*http.Response, error) {
	return a.routingRoundTrip(r, a.cfg.PrimaryAddr, generation)
}
