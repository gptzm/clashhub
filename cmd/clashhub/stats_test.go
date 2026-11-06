package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestStatsWindowsAndHealth(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 30, 30, 0, time.UTC)
	s, err := OpenStats(filepath.Join(t.TempDir(), "stats.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }
	s.top["example.com"] = now.Add(-48 * time.Hour)
	s.rankMinute = statsMinute(now)
	for _, mins := range []int{1, 7, 30, 120, 1500} {
		err = s.RecordConnection(StatsConnection{At: now.Add(-time.Duration(mins) * time.Minute), Target: "Example.COM:443", Success: mins != 7, Duration: time.Duration(mins) * time.Millisecond, Upload: 10, Download: 20})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = s.RecordTraffic(now, "example.com", "", 3, 4); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordHealth(StatsHealth{At: now, Name: "primary", Success: true, Duration: 3 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordHealth(StatsHealth{At: now.Add(-90 * time.Minute), Name: "primary", Duration: 5 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		window   int
		attempts uint64
		hours    int
	}{{5, 1, 1}, {10, 2, 1}, {60, 3, 1}, {1440, 4, 2}} {
		rows, h, err := s.Query(tt.window)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].Attempts != tt.attempts || rows[0].Target != "example.com" || !rows[0].NodeUnknown || rows[0].Upload != tt.attempts*10+3 || rows[0].Download != tt.attempts*20+4 || len(h) != tt.hours {
			t.Fatalf("window %d: %#v %#v", tt.window, rows, h)
		}
		if h[0].Hour.Minute() != 0 || h[0].Hour.Location() != time.UTC {
			t.Fatal("health hour not UTC aligned")
		}
	}
	if _, _, err = s.Query(15); err == nil {
		t.Fatal("unsupported window accepted")
	}
}
func TestStatsP95AndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.json")
	s, err := OpenStats(path)
	if err != nil {
		t.Fatal(err)
	}
	s.top["host"] = time.Now().Add(-time.Minute)
	s.rankMinute = statsMinute(time.Now())
	s.ranking[statsRankKey{statsMinute(time.Now()), "host"}] = statsCounts{Upload: 1}
	for i := 1; i <= 20; i++ {
		if err = s.RecordConnection(StatsConnection{Target: "host", Node: "node", Success: i != 1, Duration: time.Duration(i) * time.Millisecond}); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Flush(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	s, err = OpenStats(path)
	if err != nil {
		t.Fatal(err)
	}
	rows, _, err := s.Query(5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Attempts != 20 || rows[0].Failures != 1 || rows[0].NodeUnknown || rows[0].P95MSUpper == nil || *rows[0].P95MSUpper != 32 {
		t.Fatalf("unexpected summary %#v", rows)
	}
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err = s.Flush(); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("replacement permissions", info, err)
	}
}
func TestStatsRetentionAndValidation(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 30, 30, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "stats")
	s, err := OpenStats(path)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }
	if err = s.RecordConnection(StatsConnection{At: now.Add(-48 * time.Hour), Target: "a", Duration: time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordConnection(StatsConnection{At: now.Add(-48*time.Hour - time.Second), Target: "a"}); err == nil {
		t.Fatal("old event accepted")
	}
	if err = s.RecordConnection(StatsConnection{Target: "https://user:password@host/"}); err == nil {
		t.Fatal("URL accepted")
	}
	if err = s.RecordConnection(StatsConnection{Target: "host", Node: "user:pass@host"}); err == nil {
		t.Fatal("credential-like node accepted")
	}
	if err = s.RecordConnection(StatsConnection{At: now.Add(time.Second), Target: "a"}); err == nil {
		t.Fatal("future event accepted")
	}
	now = now.Add(time.Minute)
	if err = s.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(s.connections) != 0 {
		t.Fatal("old bucket retained")
	}
	if err = os.WriteFile(path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenStats(path); err == nil {
		t.Fatal("corruption ignored")
	}
}
func TestStatsHealthWindowMinuteBoundaries(t *testing.T) {
	now := time.Date(2026, 1, 3, 12, 30, 30, 0, time.UTC)
	s, err := OpenStats(filepath.Join(t.TempDir(), "stats"))
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }
	for _, at := range []time.Time{
		time.Date(2026, 1, 3, 11, 30, 0, 0, time.UTC),  // cutoff minute, before exact cutoff
		time.Date(2026, 1, 3, 11, 29, 59, 0, time.UTC), // previous minute
		time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC),
		now,
	} {
		if err := s.RecordHealth(StatsHealth{At: at, Name: "probe", Success: true}); err != nil {
			t.Fatal(err)
		}
	}
	_, h, err := s.Query(60)
	if err != nil || len(h) != 2 || h[0].Attempts != 1 || h[1].Attempts != 2 || !h[0].Hour.Equal(now.Add(-time.Hour).Truncate(time.Hour)) {
		t.Fatalf("boundary hours: %+v, %v", h, err)
	}
}

func TestStatsHealth48Hours(t *testing.T) {
	now := time.Date(2026, 1, 3, 12, 30, 30, 0, time.UTC)
	s, err := OpenStats(filepath.Join(t.TempDir(), "stats"))
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }
	if got := s.QueryHealth48Hours(); len(got) != 0 {
		t.Fatalf("empty: %#v", got)
	}
	for _, e := range []StatsHealth{
		{At: now.Add(-48 * time.Hour), Name: "old", Success: true, Duration: time.Millisecond},
		{At: now.Add(-25 * time.Hour), Name: "primary", Duration: 3 * time.Millisecond},
		{At: now.Add(-10 * time.Minute), Name: "primary", Success: true, Duration: 5 * time.Millisecond},
		{At: now.Add(-9 * time.Minute), Name: "primary", Duration: 7 * time.Millisecond},
		{At: now, Name: "secondary", Success: true},
	} {
		if err := s.RecordHealth(e); err != nil {
			t.Fatal(err)
		}
	}
	_, short, err := s.Query(5)
	if err != nil || len(short) != 1 {
		t.Fatalf("short query: %#v %v", short, err)
	}
	got := s.QueryHealth48Hours()
	if len(got) != 4 || got[0].Name != "old" || got[1].Name != "primary" || got[2].Name != "primary" || got[3].Name != "secondary" {
		t.Fatalf("48h: %#v", got)
	}
	if got[2].Attempts != 2 || got[2].Successes != 1 || got[2].Failures != 1 || got[2].P95MSUpper == nil || *got[2].P95MSUpper != 8 {
		t.Fatalf("aggregation: %#v", got[2])
	}
	if !got[0].Hour.Equal(now.Add(-48 * time.Hour).Truncate(time.Hour)) {
		t.Fatalf("boundary: %#v", got[0])
	}
	now = now.Add(time.Minute)
	got = s.QueryHealth48Hours()
	if len(got) != 3 || got[0].Name == "old" {
		t.Fatalf("expired: %#v", got)
	}
}

func TestPortTrafficAndRollingP95(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	path := filepath.Join(t.TempDir(), "stats")
	s, err := OpenStats(path)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }
	for _, e := range []struct {
		addr     string
		up, down uint64
	}{
		{"127.0.0.1:33301", 5, 8}, {"127.0.0.1:33210", 7, 9}, {"[::1]:4567", 11, 12}, {"invalid", 13, 14},
	} {
		if err := s.RecordPortTraffic(time.Time{}, "example.org", "", selectedPort(e.addr), e.up, e.down); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.QueryPortRows(5)
	if err != nil || len(rows) != 4 || !rows[0].Unknown || rows[0].Upload != 13 || rows[1].Port != 4567 || rows[2].Port != 33210 || rows[3].Port != 33301 {
		t.Fatalf("ports: %+v %v", rows, err)
	}
	totals, _, _ := s.Query(5)
	if len(totals) != 1 || totals[0].Upload != 36 || totals[0].Download != 43 {
		t.Fatalf("totals: %+v", totals)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := OpenStats(path)
	if err != nil {
		t.Fatal(err)
	}
	reloaded.now = s.now
	rows, err = reloaded.QueryPortRows(5)
	if err != nil || len(rows) != 4 {
		t.Fatalf("reloaded: %+v %v", rows, err)
	}
	for i := 0; i < 19; i++ {
		s.RecordRecentConnection(time.Millisecond)
	}
	s.RecordRecentConnection(100 * time.Millisecond)
	p95, count := s.RecentConnectionP95()
	if count != 20 || p95 == nil || *p95 != 1 {
		t.Fatalf("p95 %v samples %d", p95, count)
	}
	now = now.Add(60 * time.Second)
	p95, count = s.RecentConnectionP95()
	if p95 != nil || count != 0 {
		t.Fatalf("expired p95 %v samples %d", p95, count)
	}
}

func TestStatsConcurrent(t *testing.T) {
	s, err := OpenStats(filepath.Join(t.TempDir(), "stats"))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if err := s.RecordTraffic(time.Time{}, "example.org", "", 1, 2); err != nil {
					t.Error(err)
				}
				_, _, _ = s.Query(5)
			}
		}()
	}
	wg.Wait()
	r, _, err := s.Query(5)
	if err != nil || len(r) != 1 || r[0].Upload != 800 || r[0].Download != 1600 || r[0].P95MSUpper != nil {
		t.Fatalf("%#v %v", r, err)
	}
}

func TestDomainRankingLifecycle(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Minute).Add(-3*time.Minute + 10*time.Second)
	path := filepath.Join(t.TempDir(), "stats")
	s, err := OpenStats(path)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }
	for i := 0; i < 25; i++ {
		if err := s.RecordPortTraffic(now, fmt.Sprintf("d%02d.example", i), "", 443, uint64(100-i), 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordPortTraffic(now, "192.0.2.1", "", 443, 99999, 0); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	rows, _, _ := s.Query(60)
	if len(s.top) != 20 || len(rows) != 25 {
		t.Fatalf("membership/detail: %v %+v", s.top, rows)
	}
	first := s.detailStartedAt
	if first.IsZero() {
		t.Fatal("missing start")
	}
	_ = s.RecordTraffic(now, "d00.example", "", 3, 4)
	_ = s.RecordTraffic(now, "d24.example", "", 1000, 0)
	now = now.Add(time.Minute)
	rows, _, _ = s.Query(60)
	if _, ok := s.top["d24.example"]; !ok {
		t.Fatal("new domain not admitted")
	}
	if _, ok := s.top["d19.example"]; ok {
		t.Fatal("old domain not evicted")
	}
	if len(rows) != 25 || !rows[0].DetailStartedAt.Equal(first) {
		t.Fatalf("details: %+v", rows)
	}
	_ = s.RecordTraffic(now, "d24.example", "", 7, 0)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := OpenStats(path)
	if err != nil {
		t.Fatal(err)
	}
	reloaded.now = s.now
	rows, _, _ = reloaded.Query(60)
	if len(rows) != 25 {
		t.Fatalf("reload: %+v", rows)
	}
	ports, _ := reloaded.QueryPortRows(60)
	var total uint64
	for _, p := range ports {
		total += p.Upload
	}
	if total != 103209 {
		t.Fatalf("ports total %d", total)
	}
	now = now.Add(24 * time.Hour)
	rows, _, _ = reloaded.Query(60)
	if len(rows) != 0 || len(reloaded.top) != 0 {
		t.Fatalf("expired %+v %v", rows, reloaded.top)
	}
}

func TestLegacyMigrationPreservesPortsAndHealth(t *testing.T) {
	now := time.Now().UTC()
	path := filepath.Join(t.TempDir(), "stats")
	d := statsDisk{Version: 1, Connections: []statsConnectionBucket{{Key: statsKey{Minute: statsMinute(now), Target: "old.example", Port: 443}, Counts: statsCounts{Upload: 42, Download: 9}}, {Key: statsKey{Minute: statsMinute(now), Target: "192.0.2.1", Port: 443}, Counts: statsCounts{Upload: 8}}}, Health: []statsHealthBucket{{Key: statsHealthKey{statsMinute(now), "node"}, Counts: statsCounts{Attempts: 1}}}}
	b, _ := json.Marshal(d)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStats(path)
	if err != nil {
		t.Fatal(err)
	}
	rows, health, _ := s.Query(60)
	ports, _ := s.QueryPortRows(60)
	if len(rows) != 1 || rows[0].Target != "old.example" || rows[0].Upload != 42 || rows[0].DetailStartedAt.Before(now) || len(health) != 1 || len(ports) != 1 || ports[0].Upload != 50 || len(s.top) != 1 {
		t.Fatalf("migration %+v %+v %+v %v", rows, health, ports, s.top)
	}
	if err = s.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenStats(path); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyMigrationSelectsTopTenAndPreservesStart(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Minute)
	path := filepath.Join(t.TempDir(), "stats")
	d := statsDisk{Version: 1}
	for i := 0; i < 12; i++ {
		d.Connections = append(d.Connections, statsConnectionBucket{Key: statsKey{Minute: statsMinute(now), Target: fmt.Sprintf("d%02d.example", i), Port: 443}, Counts: statsCounts{Upload: uint64(100 - i)}})
	}
	d.Connections = append(d.Connections, statsConnectionBucket{Key: statsKey{Minute: statsMinute(now.Add(-30 * time.Hour)), Target: "d00.example"}, Counts: statsCounts{Download: 7}})
	b, _ := json.Marshal(d)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStats(path)
	if err != nil {
		t.Fatal(err)
	}
	rows, _, _ := s.Query(2880)
	if len(rows) != 12 {
		t.Fatalf("rows=%+v", rows)
	}
	for _, r := range rows {
		if r.Target == "d00.example" && (r.Download != 7 || r.DetailStartedAt.Before(now)) {
			t.Fatalf("start %+v", r)
		}
	}
	if err = s.Flush(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStats(path)
	if err != nil {
		t.Fatal(err)
	}
	rows, _, _ = s.Query(2880)
	if len(rows) != 12 {
		t.Fatal(rows)
	}
}

func TestStatsCrashReopenNeverClaimsComplete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats")
	s, err := OpenStats(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	s.attemptsStartedAt = now.Add(-48 * time.Hour)
	if err := s.RecordConnection(StatsConnection{Target: "crash.example"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTraffic(time.Time{}, "crash.example", "", 100, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	// Simulate more unflushed activity followed by a crash.
	if err := s.RecordConnection(StatsConnection{Target: "crash.example"}); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStats(path)
	if err != nil {
		t.Fatal(err)
	}
	rank := reopened.QueryDomainRanking()
	if len(rank) != 1 || rank[0].AttemptsComplete || rank[0].Attempts != 0 || rank[0].AttemptsStartedAt.Before(now) || rank[0].Upload != 100 {
		t.Fatalf("crash recovery: %+v", rank)
	}
	// Even after a whole day, elapsed time cannot establish path coverage.
	later := time.Now().Add(25 * time.Hour)
	reopened.now = func() time.Time { return later }
	if err := reopened.RecordTraffic(time.Time{}, "crash.example", "", 1, 0); err != nil {
		t.Fatal(err)
	}
	rank = reopened.QueryDomainRanking()
	if len(rank) != 1 || rank[0].AttemptsComplete {
		t.Fatalf("elapsed time claimed coverage: %+v", rank)
	}
}

func TestRankingAttemptsCoverageAndMigration(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	path := filepath.Join(t.TempDir(), "stats")
	legacy := statsDisk{Version: 2, Top: map[string]time.Time{"a.example": now.Add(-time.Hour)}, RankMinute: statsMinute(now), Ranking: []statsRankBucket{{Key: statsRankKey{statsMinute(now), "a.example"}, Counts: statsCounts{Upload: 100, Attempts: 99}}}, Connections: []statsConnectionBucket{{Key: statsKey{Minute: statsMinute(now), Target: "a.example"}, Counts: statsCounts{Attempts: 3}}}}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStats(path)
	if err != nil {
		t.Fatal(err)
	}
	start := s.attemptsStartedAt
	now = start.Add(time.Second)
	s.now = func() time.Time { return now }
	if err := s.RecordConnection(StatsConnection{Target: "a.example", Success: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordConnection(StatsConnection{Target: "a.example"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordConnection(StatsConnection{At: start.Add(-time.Second), Target: "a.example"}); err != nil {
		t.Fatal(err)
	}
	rank := s.QueryDomainRanking()
	if len(rank) != 1 || rank[0].Attempts != 2 || rank[0].AttemptsComplete || !rank[0].AttemptsStartedAt.Equal(start) || rank[0].Upload != 100 {
		t.Fatalf("migration rank: %+v", rank)
	}
	rows, _, _ := s.Query(60)
	if len(rows) != 1 || rows[0].Attempts != 6 {
		t.Fatalf("legacy detail: %+v", rows)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStats(path)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }
	rank = s.QueryDomainRanking()
	if len(rank) != 1 || rank[0].Attempts != 0 || rank[0].AttemptsStartedAt.Before(start) || rank[0].AttemptsComplete {
		t.Fatalf("reload must restart coverage: %+v", rank)
	}
	// Elapsed time alone cannot prove that all proxy paths were observed.
	now = start.Truncate(time.Minute).Add(24*time.Hour - time.Minute)
	if err := s.RecordTraffic(now, "a.example", "", 1, 0); err != nil {
		t.Fatal(err)
	}
	s.refreshRanking(now, true)
	rank = s.QueryDomainRanking()
	if len(rank) != 1 || rank[0].AttemptsComplete {
		t.Fatalf("premature complete: %+v", rank)
	}
	now = now.Add(time.Minute)
	// Keep the domain in the traffic ranking after the old traffic expires.
	if err := s.RecordTraffic(now, "a.example", "", 1, 0); err != nil {
		t.Fatal(err)
	}
	s.refreshRanking(now, true)
	rank = s.QueryDomainRanking()
	if len(rank) != 1 || rank[0].AttemptsComplete || rank[0].Attempts != 0 {
		t.Fatalf("unverified coverage: %+v", rank)
	}
}

func TestDomainHistoryPersistsBeyondRetention(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Minute)
	path := filepath.Join(t.TempDir(), "stats")
	s, err := OpenStats(path)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }
	if err := s.RecordTraffic(now, "history.example", "", 100, 25); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTraffic(now, "192.0.2.9", "", 500, 500); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	rows := s.QueryDomainHistory()
	if len(rows) != 1 || rows[0].Target != "history.example" || rows[0].Upload != 100 || rows[0].Download != 25 {
		t.Fatalf("history: %+v", rows)
	}
	// Three days later the rolling windows are empty, but the cumulative table
	// must still report the domain.
	now = now.Add(72 * time.Hour)
	s.now = func() time.Time { return now }
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := OpenStats(path)
	if err != nil {
		t.Fatal(err)
	}
	reloaded.now = func() time.Time { return now }
	rows = reloaded.QueryDomainHistory()
	if len(rows) != 1 || rows[0].Upload != 100 || rows[0].Download != 25 {
		t.Fatalf("history after retention: %+v", rows)
	}
	if err := reloaded.RecordTraffic(now, "history.example", "", 5, 1); err != nil {
		t.Fatal(err)
	}
	rows = reloaded.QueryDomainHistory()
	if len(rows) != 1 || rows[0].Upload != 105 || rows[0].Download != 26 {
		t.Fatalf("history did not accumulate: %+v", rows)
	}
}

func TestDomainHistorySeedsFromRetainedDetailOnUpgrade(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Minute)
	path := filepath.Join(t.TempDir(), "stats")
	s, err := OpenStats(path)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }
	if err := s.RecordTraffic(now, "legacy.example", "", 40, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc, "History")
	raw, err = json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	upgraded, err := OpenStats(path)
	if err != nil {
		t.Fatal(err)
	}
	upgraded.now = func() time.Time { return now }
	rows := upgraded.QueryDomainHistory()
	if len(rows) != 1 || rows[0].Target != "legacy.example" || rows[0].Upload != 40 || rows[0].Download != 2 {
		t.Fatalf("seeded history: %+v", rows)
	}
}
