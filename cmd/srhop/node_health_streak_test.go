package main

import (
	"testing"
	"time"
)

func TestPersistentUncertainRemainsUnknown(t *testing.T) {
	p := &nodeProber{uncertainStreak: map[string]int{}}
	now := time.Now()
	uncertain := probeResult{uncertain: true, at: now}
	failed := probeResult{failed: true, at: now}
	ok := probeResult{at: now, delay: 100}

	if p.deadForSelection("gstatic", "n", uncertain) {
		t.Fatal("first uncertain result must not count as dead")
	}
	p.recordUncertain("gstatic", "n", uncertain)
	if p.deadForSelection("gstatic", "n", uncertain) {
		t.Fatal("one uncertain sweep must not count as dead")
	}
	p.recordUncertain("gstatic", "n", uncertain)
	if p.deadForSelection("gstatic", "n", uncertain) {
		t.Fatal("repeated uncertainty is not evidence of node failure")
	}
	p.recordUncertain("gstatic", "n", ok)
	if p.deadForSelection("gstatic", "n", ok) {
		t.Fatal("success must reset the streak")
	}
	p.recordUncertain("gstatic", "n", failed)
	if !p.deadForSelection("gstatic", "n", failed) {
		t.Fatal("explicit failure is dead regardless of streak")
	}
	p.recordUncertain("gstatic", "n", failed)
	if p.deadForSelection("gstatic", "n", ok) {
		t.Fatal("fresh success must never be dead")
	}
	// Streaks are per kind|node.
	p.recordUncertain("gstatic", "m", uncertain)
	p.recordUncertain("ai-chatgpt", "n", uncertain)
	if p.deadForSelection("ai", "m", uncertain) {
		t.Fatal("other kinds must not share streaks")
	}
	// Stale uncertain results are never dead.
	old := probeResult{uncertain: true, at: now.Add(-2 * nodeResultMaxAge)}
	if p.deadForSelection("gstatic", "n", old) {
		t.Fatal("stale results must not count as dead")
	}
}
