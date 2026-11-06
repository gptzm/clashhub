package main

import (
	"testing"
	"time"
)

func TestNodeTrafficSamplerMissesClosedShortFlow(t *testing.T) {
	var s nodeTrafficSampler
	now := time.Now()
	resolve := func([]string) string { return "fallback" }
	if _, err := s.observe("engine", now, mihomoConnectionsSnapshot{}, resolve, nodeTrafficMaxGap); err != nil {
		t.Fatal(err)
	}
	o, err := s.observe("engine", now.Add(nodeTrafficInterval), mihomoConnectionsSnapshot{UploadTotal: 300, DownloadTotal: 700}, resolve, nodeTrafficMaxGap)
	if err != nil || len(o.Known) != 0 || o.Global != (nodeTrafficBytes{300, 700}) {
		t.Fatalf("unexpected short-flow attribution: %+v, %v", o, err)
	}
}

func TestNodeTrafficSampler(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	resolve := func(c []string) string {
		if len(c) > 0 && c[0] == "node" {
			return c[0]
		}
		return ""
	}
	connection := func(id string, start time.Time, node string, up, down uint64) mihomoConnectionSample {
		return mihomoConnectionSample{ID: id, Start: start, Chains: []string{node}, Upload: up, Download: down}
	}
	var s nodeTrafficSampler
	sample := func(at time.Time, snap mihomoConnectionsSnapshot) nodeTrafficObservation {
		t.Helper()
		o, e := s.observe("process-1", at, snap, resolve, 2*time.Second)
		if e != nil {
			t.Fatal(e)
		}
		return o
	}
	first := sample(t0, mihomoConnectionsSnapshot{UploadTotal: 100, DownloadTotal: 100, Connections: []mihomoConnectionSample{connection("a", t0.Add(-time.Minute), "node", 50, 60)}})
	if first.RateAvailable || len(first.Known) != 0 {
		t.Fatal(first)
	}
	second := sample(t0.Add(time.Second), mihomoConnectionsSnapshot{UploadTotal: 150, DownloadTotal: 180, Connections: []mihomoConnectionSample{connection("a", t0.Add(-time.Minute), "node", 65, 70), connection("b", t0.Add(time.Millisecond), "node", 7, 9), connection("c", t0.Add(time.Millisecond), "DIRECT", 3, 4)}})
	if second.Known["node"] != (nodeTrafficBytes{22, 19}) || second.Unknown != (nodeTrafficBytes{3, 4}) || second.Global != (nodeTrafficBytes{50, 80}) || !second.RateAvailable {
		t.Fatal(second)
	}
	third := sample(t0.Add(2*time.Second), mihomoConnectionsSnapshot{UploadTotal: 170, DownloadTotal: 190, Connections: []mihomoConnectionSample{connection("a", t0.Add(-time.Minute), "node", 70, 72)}})
	if third.Known["node"] != (nodeTrafficBytes{5, 2}) || third.Global != (nodeTrafficBytes{20, 10}) {
		t.Fatal(third)
	}
	// Closed b's unseen tail and completely unobserved flows are represented only
	// in aggregate, not invented as a per-node count.
	fourth := sample(t0.Add(10*time.Second), mihomoConnectionsSnapshot{UploadTotal: 175, DownloadTotal: 200, Connections: []mihomoConnectionSample{connection("a", t0.Add(-time.Minute), "DIRECT", 75, 80)}})
	if fourth.RateAvailable || fourth.Unknown != (nodeTrafficBytes{5, 8}) {
		t.Fatal(fourth)
	}
	reset := sample(t0.Add(11*time.Second), mihomoConnectionsSnapshot{UploadTotal: 1, DownloadTotal: 1})
	if reset.RateAvailable || len(reset.Known) != 0 {
		t.Fatal(reset)
	}
}

func TestNodeTrafficSamplerRejectsBadSamplesWithoutAdvancing(t *testing.T) {
	var s nodeTrafficSampler
	now := time.Now()
	resolve := func([]string) string { return "n" }
	if _, err := s.observe("e", now, mihomoConnectionsSnapshot{}, resolve, time.Second); err != nil {
		t.Fatal(err)
	}
	c := mihomoConnectionSample{ID: "same", Start: now}
	if _, err := s.observe("e", now.Add(time.Second), mihomoConnectionsSnapshot{Connections: []mihomoConnectionSample{c, c}}, resolve, time.Second); err == nil {
		t.Fatal("duplicate accepted")
	}
	if !s.at.Equal(now) {
		t.Fatal("bad sample advanced cursor")
	}
	if _, err := s.observe("e", now, mihomoConnectionsSnapshot{}, resolve, time.Second); err == nil {
		t.Fatal("non-increasing time accepted")
	}
	o, err := s.observe("new", now.Add(time.Second), mihomoConnectionsSnapshot{UploadTotal: 999, Connections: []mihomoConnectionSample{c}}, resolve, time.Second)
	if err != nil || o.RateAvailable || len(o.Known) != 0 {
		t.Fatal(o, err)
	}
}
