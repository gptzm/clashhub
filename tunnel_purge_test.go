package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func pipeTunnel(t *testing.T) (relaySide, relaySide, relaySide, relaySide) {
	t.Helper()
	clientEnd, relayClient := net.Pipe()
	upstreamEnd, relayUpstream := net.Pipe()
	t.Cleanup(func() {
		_ = clientEnd.Close()
		_ = relayClient.Close()
		_ = upstreamEnd.Close()
		_ = relayUpstream.Close()
	})
	return relayClient, clientEnd, relayUpstream, upstreamEnd
}

type relaySide = net.Conn

// waitClosed polls until a Read on c returns an error (conn was closed).
func waitClosed(t *testing.T, c net.Conn, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	errCh := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(c, make([]byte, 1))
		errCh <- err
	}()
	for {
		select {
		case err := <-errCh:
			return err != nil
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

func TestPurgeTunnelsByUpstream(t *testing.T) {
	a := &App{log: log.New(io.Discard, "", 0)}
	mine1, mine1Peer := net.Pipe()
	mine2, mine2Peer := net.Pipe()
	other, otherPeer := net.Pipe()
	defer mine1.Close()
	defer mine1Peer.Close()
	defer mine2.Close()
	defer mine2Peer.Close()
	defer other.Close()
	defer otherPeer.Close()

	a.trackTunnel("127.0.0.1:33301", mine1, mine2)
	a.trackTunnel("127.0.0.1:33210", other)

	got := a.purgeTunnelsByUpstream("127.0.0.1:33301", "test purge")
	if got != 2 {
		t.Fatalf("expected 2 purged tunnels, got %d", got)
	}
	for _, c := range []net.Conn{mine1, mine2} {
		if !waitClosed(t, c, time.Second) {
			t.Fatalf("tunnel conn was not closed by purge")
		}
	}
	if waitClosed(t, other, 200*time.Millisecond) {
		t.Fatalf("tunnel on another upstream must stay open")
	}
	// The registry must be clean afterwards.
	a.tunnelMu.Lock()
	left := len(a.tunnelConns)
	a.tunnelMu.Unlock()
	if left != 1 {
		t.Fatalf("expected 1 remaining tracked tunnel, got %d", left)
	}
}

func TestRelayFirstByteStallKill(t *testing.T) {
	a := &App{
		cfg: Config{TunnelFirstByteTimeoutSec: 1, TunnelStallTimeoutSec: 1500},
		log: log.New(io.Discard, "", 0),
	}
	relayClient, clientEnd, relayUpstream, upstreamEnd := pipeTunnel(t)
	// One client byte flows (like a TLS ClientHello), then total silence: the
	// upstream never answers, so the first-byte limit must close the pair.
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.relay(&bufferedConn{Conn: relayClient, r: bufio.NewReader(relayClient)}, &bufferedConn{Conn: relayUpstream, r: bufio.NewReader(relayUpstream)}, "127.0.0.1:33301", "chatgpt.com:443")
	}()
	if _, err := clientEnd.Write([]byte{0x16}); err != nil {
		t.Fatalf("client write: %v", err)
	}
	go func() {
		_, _ = io.Copy(io.Discard, upstreamEnd) // keep the pipe drained
	}()
	start := time.Now()
	if !waitClosed(t, clientEnd, 5*time.Second) {
		t.Fatalf("first-byte stall did not close the client side")
	}
	elapsed := time.Since(start)
	if elapsed > 3*time.Second {
		t.Fatalf("first-byte kill took too long: %s", elapsed)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("relay did not exit after stall kill")
	}
}

func TestRelayMidStreamStallKill(t *testing.T) {
	a := &App{
		cfg: Config{TunnelFirstByteTimeoutSec: 60, TunnelStallTimeoutSec: 1},
		log: log.New(io.Discard, "", 0),
	}
	relayClient, clientEnd, relayUpstream, upstreamEnd := pipeTunnel(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.relay(&bufferedConn{Conn: relayClient, r: bufio.NewReader(relayClient)}, &bufferedConn{Conn: relayUpstream, r: bufio.NewReader(relayUpstream)}, "127.0.0.1:33301", "chatgpt.com:443")
	}()
	go func() {
		_, _ = io.Copy(io.Discard, clientEnd)
	}()
	// Upstream delivers one byte (data has flowed), then goes silent forever.
	if _, err := upstreamEnd.Write([]byte("x")); err != nil {
		t.Fatalf("upstream write: %v", err)
	}
	if !waitClosed(t, clientEnd, 5*time.Second) {
		t.Fatalf("mid-stream stall did not close the client side")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("relay did not exit after stall kill")
	}
}

func TestRelayProgressPreventsKill(t *testing.T) {
	a := &App{
		cfg: Config{TunnelFirstByteTimeoutSec: 1, TunnelStallTimeoutSec: 1},
		log: log.New(io.Discard, "", 0),
	}
	relayClient, clientEnd, relayUpstream, upstreamEnd := pipeTunnel(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.relay(&bufferedConn{Conn: relayClient, r: bufio.NewReader(relayClient)}, &bufferedConn{Conn: relayUpstream, r: bufio.NewReader(relayUpstream)}, "127.0.0.1:33301", "chatgpt.com:443")
	}()
	// Keep bytes flowing in both directions: the watchdog must never fire.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		byte := []byte("y")
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = clientEnd.Write(byte)
			time.Sleep(100 * time.Millisecond)
		}
	}()
	go func() {
		_, _ = io.Copy(io.Discard, clientEnd)
	}()
	go func() {
		buf := make([]byte, 64)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := upstreamEnd.Write(buf[:1]); err != nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	go func() {
		_, _ = io.Copy(io.Discard, upstreamEnd)
	}()
	select {
	case <-done:
		t.Fatalf("watchdog killed an actively flowing tunnel")
	case <-time.After(3 * time.Second):
		// Survived 3s against 1s limits: progress tracking works.
	}
}

func strategyInput(strategy string, activeDelay, rivalDelay int64) selectNodeChoiceInput {
	return selectNodeChoiceInput{
		Candidates: []string{"A", "B"},
		Healthy:    func(string) bool { return true },
		Delay: func(n string) int64 {
			if n == "A" {
				return activeDelay
			}
			return rivalDelay
		},
		Failed:       func(string) bool { return false },
		Active:       "A",
		Previous:     "",
		Strategy:     strategy,
		LatencyGapMS: 150,
	}
}

func TestSelectNodeChoiceStabilityKeepsSlowActive(t *testing.T) {
	in := strategyInput("stability", 900, 100)
	in.Index = sourceIndex{} // selectionActive() false
	if got := selectNodeChoice(in); got != "A" {
		t.Fatalf("stability must keep a healthy active node despite latency, got %q", got)
	}
}

func TestSelectNodeChoiceLatencySwitchesBeyondGap(t *testing.T) {
	in := strategyInput("latency", 900, 100)
	in.Index = sourceIndex{}
	if got := selectNodeChoice(in); got != "B" {
		t.Fatalf("latency must chase the best node beyond the gap, got %q", got)
	}
}

func TestSelectNodeChoiceLatencyKeepsWithinGap(t *testing.T) {
	in := strategyInput("latency", 200, 100)
	in.Index = sourceIndex{}
	if got := selectNodeChoice(in); got != "A" {
		t.Fatalf("latency must keep the active node within the gap, got %q", got)
	}
}

func TestSelectNodeChoiceLegacyTolerance(t *testing.T) {
	// Legacy behavior: slow-but-under-500ms active sticks, over 500ms switches.
	slow := strategyInput("", 900, 100)
	slow.Index = sourceIndex{}
	if got := selectNodeChoice(slow); got != "B" {
		t.Fatalf("legacy strategy must switch beyond 500ms, got %q", got)
	}
	tolerable := strategyInput("", 400, 100)
	tolerable.Index = sourceIndex{}
	if got := selectNodeChoice(tolerable); got != "A" {
		t.Fatalf("legacy strategy must keep active within 500ms, got %q", got)
	}
}

func TestStrategyForKind(t *testing.T) {
	a := &App{}
	a.setStrategyValues("stability", "latency", latencyGapMSDefault, balanceGapMSDefault)
	if got := a.strategyForKind("general"); got != "stability" {
		t.Fatalf("general strategy = %q, want stability", got)
	}
	if got := a.strategyForKind("ai"); got != "latency" {
		t.Fatalf("ai strategy = %q, want latency", got)
	}
	var nilApp *App
	if got := nilApp.strategyForKind("ai"); got != "" {
		t.Fatalf("nil app strategy = %q, want legacy empty", got)
	}
}

func TestTunnelConfigDefaults(t *testing.T) {
	cfg := defaultConfig()
	if cfg.PrimaryStrategy != "stability" || cfg.AIStrategy != "latency" {
		t.Fatalf("strategy defaults wrong: primary=%q ai=%q", cfg.PrimaryStrategy, cfg.AIStrategy)
	}
	if cfg.LatencyGapMS != latencyGapMSDefault || cfg.TunnelStallTimeoutSec != tunnelStallTimeoutSecDefault || cfg.TunnelFirstByteTimeoutSec != tunnelFirstByteTimeoutSecDefault {
		t.Fatalf("tunnel timeout defaults wrong: %+v", cfg)
	}
	// progressReader must refresh activity and mark upstream data once.
	var last, fromUp atomic.Int64
	pr := &progressReader{reader: bytes.NewReader([]byte("abcd")), at: &last, fromUpstream: &fromUp, upstream: true}
	_, _ = io.Copy(io.Discard, pr)
	if last.Load() == 0 || fromUp.Load() == 0 {
		t.Fatalf("progressReader did not record progress: last=%d fromUp=%d", last.Load(), fromUp.Load())
	}
}

func TestSelectNodeChoiceBalancedKeepsWithinGap(t *testing.T) {
	// Balanced: a 400ms trailing gap is within the 500ms balance gap — keep.
	in := strategyInput("balanced", 500, 100)
	in.Index = sourceIndex{}
	in.BalanceGapMS = 500
	if got := selectNodeChoice(in); got != "A" {
		t.Fatalf("balanced must keep active within the balance gap, got %q", got)
	}
}

func TestSelectNodeChoiceBalancedSwitchesBeyondGap(t *testing.T) {
	// Balanced: a 600ms trailing gap exceeds the 500ms balance gap — switch.
	in := strategyInput("balanced", 700, 100)
	in.Index = sourceIndex{}
	in.BalanceGapMS = 500
	if got := selectNodeChoice(in); got != "B" {
		t.Fatalf("balanced must switch beyond the balance gap, got %q", got)
	}
}

func TestNormalizeStrategyBalanced(t *testing.T) {
	for _, v := range []string{"balanced", "balance", "even", "Balanced"} {
		if got := normalizeStrategy(v); got != "balanced" {
			t.Fatalf("normalizeStrategy(%q) = %q, want balanced", v, got)
		}
		if normalizeStrategy("nonsense") != "" {
			t.Fatalf("unknown strategy must normalize to empty")
		}
	}
}

func TestPersistStrategy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	seed := map[string]any{"http_listen": "127.0.0.1:33333", "primary_strategy": "stability", "latency_gap_ms": 150}
	b, _ := json.Marshal(seed)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := persistStrategy(path, "balanced", "balanced", 300, 500); err != nil {
		t.Fatalf("persistStrategy: %v", err)
	}
	var doc map[string]any
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(raw, &doc) != nil {
		t.Fatalf("corrupted config: %s", raw)
	}
	if doc["primary_strategy"] != "balanced" || doc["ai_strategy"] != "balanced" {
		t.Fatalf("strategies not persisted: %v", doc)
	}
	if doc["latency_gap_ms"].(float64) != 300 || doc["balance_gap_ms"].(float64) != 500 {
		t.Fatalf("gaps not persisted: %v", doc)
	}
	if doc["http_listen"] != "127.0.0.1:33333" {
		t.Fatalf("other keys must be preserved: %v", doc)
	}
}

func TestSetStrategyValuesFallbacks(t *testing.T) {
	a := &App{}
	a.setStrategyValues("nonsense", "", 0, 0)
	primary, ai, latencyGap, balanceGap := a.strategySnapshot()
	if primary != "stability" || ai != "latency" {
		t.Fatalf("empty/invalid strategies must fall back to defaults, got primary=%q ai=%q", primary, ai)
	}
	if latencyGap != latencyGapMSDefault || balanceGap != balanceGapMSDefault {
		t.Fatalf("gaps must fall back to defaults: %d %d", latencyGap, balanceGap)
	}
}
