package main

// An opt-in, in-process statistics store. It does not attach itself to the proxy,
// start goroutines, or read/write files until OpenStats/Flush are called.
import (
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const statsRetention = 48 * time.Hour

var statsProcessStartedAt = time.Now().UTC()

// StatsConnection is one completed connection attempt; byte counts may instead
// be supplied separately with RecordTraffic as traffic flows. Node is an opaque,
// non-secret identifier, not an upstream URL or credentials. Empty means unknown.
type StatsConnection struct {
	At               time.Time
	Target           string // hostname, IP, or host:port, never a URL
	Node             string
	Success          bool
	Duration         time.Duration
	Upload, Download uint64 // client->target, target->client
}
type StatsHealth struct {
	At       time.Time
	Name     string // non-secret probe identifier
	Success  bool
	Duration time.Duration
}

type statsKey struct {
	Minute       int64
	Target, Node string
	Port         uint16 // zero means unknown; old snapshots also decode as unknown
}
type statsHealthKey struct {
	Minute int64
	Name   string
}
type statsCounts struct {
	Attempts, Successes, Upload, Download uint64
	Latency                               [64]uint64 // millisecond histogram; index 0 is zero, others have upper bound 2^(index-1) ms
}
type statsRankKey struct {
	Minute int64
	Target string
}
type statsPortKey struct {
	Minute int64
	Port   uint16
}
type statsRankBucket struct {
	Key    statsRankKey
	Counts statsCounts
}
type statsPortBucket struct {
	Key    statsPortKey
	Counts statsCounts
}

type statsDisk struct {
	Version           int
	Connections       []statsConnectionBucket
	Health            []statsHealthBucket
	Ranking           []statsRankBucket
	Ports             []statsPortBucket
	Top               map[string]time.Time
	History           []statsHistoryBucket
	RankMinute        int64
	AttemptsStartedAt time.Time
	DetailStartedAt   time.Time
}
type statsHistoryBucket struct {
	Target   string
	Upload   uint64
	Download uint64
}

type statsConnectionBucket struct {
	Key    statsKey
	Counts statsCounts
}
type statsHealthBucket struct {
	Key    statsHealthKey
	Counts statsCounts
}

// StatsStore is safe for concurrent use by one process. Do not open the same
// path in multiple processes: there is no interprocess locking or merging.
type StatsStore struct {
	mu                sync.Mutex
	closed            bool
	path              string
	now               func() time.Time
	connections       map[statsKey]statsCounts
	health            map[statsHealthKey]statsCounts
	ranking           map[statsRankKey]statsCounts
	history           map[string]statsCounts
	ports             map[statsPortKey]statsCounts
	top               map[string]time.Time
	rankMinute        int64
	attemptsStartedAt time.Time
	detailStartedAt   time.Time
	recentConnections map[int64]statsCounts // exact-second rolling connection samples; not persisted
	processStartedAt  time.Time
	trafficTotals     StatsTrafficTotals
}

// StatsTrafficTotals counts observed payload bytes for this process only.
// Values saturate instead of wrapping if uint64 capacity is exhausted.
type StatsTrafficTotals struct {
	Upload   uint64 `json:"upload"`
	Download uint64 `json:"download"`
}

func statsSaturatingAdd(a, b uint64) uint64 {
	if ^uint64(0)-a < b {
		return ^uint64(0)
	}
	return a + b
}

func (s *StatsStore) recordProcessTraffic(upload, download uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.trafficTotals.Upload = statsSaturatingAdd(s.trafficTotals.Upload, upload)
	s.trafficTotals.Download = statsSaturatingAdd(s.trafficTotals.Download, download)
}

func (s *StatsStore) processTraffic() (StatsTrafficTotals, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.trafficTotals, s.processStartedAt
}

// OpenStats loads a private JSON snapshot, or starts empty if it does not exist.
// It does not create a file. Corrupt or incompatible snapshots return an error.
func OpenStats(path string) (*StatsStore, error) {
	if path == "" {
		return nil, errors.New("stats path is empty")
	}
	s := &StatsStore{path: path, now: time.Now, processStartedAt: statsProcessStartedAt, connections: make(map[statsKey]statsCounts), health: make(map[statsHealthKey]statsCounts), ranking: make(map[statsRankKey]statsCounts), history: make(map[string]statsCounts), ports: make(map[statsPortKey]statsCounts), top: make(map[string]time.Time), rankMinute: -1, attemptsStartedAt: time.Now().UTC(), detailStartedAt: time.Now().UTC()}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var d statsDisk
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("decode stats: %w", err)
	}
	if d.Version != 1 && d.Version != 2 && d.Version != 3 {
		return nil, fmt.Errorf("unsupported stats version %d", d.Version)
	}
	for _, v := range d.Connections {
		if _, ok := s.connections[v.Key]; ok {
			return nil, errors.New("duplicate stats bucket")
		}
		s.connections[v.Key] = v.Counts
	}
	for _, v := range d.Health {
		if _, ok := s.health[v.Key]; ok {
			return nil, errors.New("duplicate health bucket")
		}
		s.health[v.Key] = v.Counts
	}
	if d.Version == 1 {
		// Migrate before dropping any target rows: port totals are independent of rank.
		for k, c := range s.connections {
			pk := statsPortKey{k.Minute, k.Port}
			pc := s.ports[pk]
			statsMerge(&pc, c)
			s.ports[pk] = pc
			if net.ParseIP(k.Target) == nil {
				rk := statsRankKey{k.Minute, k.Target}
				rc := s.ranking[rk]
				statsMerge(&rc, c)
				s.ranking[rk] = rc
			}
		}
	} else {
		for _, b := range d.Ranking {
			if _, ok := s.ranking[b.Key]; ok {
				return nil, errors.New("duplicate ranking bucket")
			}
			if normalized, err := statsTarget(b.Key.Target); err != nil || normalized != b.Key.Target || net.ParseIP(b.Key.Target) != nil {
				return nil, errors.New("invalid ranking target")
			}
			s.ranking[b.Key] = b.Counts
		}
		for _, b := range d.Ports {
			if _, ok := s.ports[b.Key]; ok {
				return nil, errors.New("duplicate port bucket")
			}
			s.ports[b.Key] = b.Counts
		}
		if d.Top == nil || len(d.Top) > statsTopDomains {
			return nil, errors.New("invalid top domains")
		}
		for name, started := range d.Top {
			if normalized, err := statsTarget(name); err != nil || normalized != name || net.ParseIP(name) != nil || started.IsZero() {
				return nil, errors.New("invalid top domain")
			}
		}
		s.top = d.Top
		for _, h := range d.History {
			if normalized, err := statsTarget(h.Target); err != nil || normalized != h.Target || net.ParseIP(h.Target) != nil {
				return nil, errors.New("invalid history target")
			}
			if _, ok := s.history[h.Target]; ok {
				return nil, errors.New("duplicate history target")
			}
			s.history[h.Target] = statsCounts{Upload: h.Upload, Download: h.Download}
		}
		s.rankMinute = d.RankMinute
		if d.Version == 3 {
			if d.AttemptsStartedAt.IsZero() || d.DetailStartedAt.IsZero() {
				return nil, errors.New("missing stats coverage")
			}
			s.attemptsStartedAt, s.detailStartedAt = d.AttemptsStartedAt, d.DetailStartedAt
		}
	}
	if len(s.history) == 0 {
		// First run with the all-time table: start from whatever detail is still
		// retained instead of showing zero history after the upgrade.
		for k, c := range s.connections {
			if net.ParseIP(k.Target) != nil {
				continue
			}
			h := s.history[k.Target]
			h.Upload = statsSaturatingAdd(h.Upload, c.Upload)
			h.Download = statsSaturatingAdd(h.Download, c.Download)
			s.history[k.Target] = h
		}
	}
	s.prune(s.now())
	// A snapshot does not establish continuity: a crash can lose unflushed
	// events, and collection may have been stopped between processes. Start a
	// new attempt coverage period on every open, including clean restarts.
	// Preserve traffic and historical detail, but don't mix old attempt counts
	// into the new coverage period.
	for k, c := range s.ranking {
		c.Attempts, c.Successes, c.Latency = 0, 0, [64]uint64{}
		s.ranking[k] = c
	}
	s.attemptsStartedAt = s.now().UTC()
	s.detailStartedAt = s.attemptsStartedAt
	s.refreshRanking(s.now(), true)
	return s, nil
}

func statsTarget(target string) (string, error) {
	target = strings.TrimSpace(target)
	if strings.ContainsAny(target, "/@?#\\ \t\r\n") || target == "" {
		return "", errors.New("target must be a hostname or IP, not a URL")
	}
	if h, _, err := net.SplitHostPort(target); err == nil {
		target = h
	} else if strings.Contains(target, ":") && net.ParseIP(target) == nil {
		return "", errors.New("invalid target")
	}
	target = strings.ToLower(strings.TrimSuffix(target, "."))
	if target == "" {
		return "", errors.New("empty target")
	}
	return target, nil
}
func statsLabel(label string) bool {
	return len(label) <= 256 && !strings.ContainsAny(label, "/@?#\\ \t\r\n")
}
func statsMinute(t time.Time) int64 { return t.Unix() / 60 }
func (s *StatsStore) eventMinute(at, now time.Time) (int64, error) {
	if at.IsZero() {
		at = now
	}
	if at.After(now) || at.Before(now.Add(-statsRetention)) {
		return 0, errors.New("stats event outside retention window")
	}
	return statsMinute(at), nil
}
func (s *StatsStore) prune(now time.Time) {
	cutoff := statsMinute(now.Add(-statsRetention))
	for k := range s.connections {
		if k.Minute < cutoff {
			delete(s.connections, k)
		}
	}
	for k := range s.ranking {
		if k.Minute < statsMinute(now)-1439 || k.Minute > statsMinute(now) {
			delete(s.ranking, k)
		}
	}
	for k := range s.ports {
		if k.Minute < cutoff {
			delete(s.ports, k)
		}
	}
	for k := range s.health {
		if k.Minute < cutoff {
			delete(s.health, k)
		}
	}
}

// refreshRanking is called with mu held. The current minute and 1439 previous
// minute buckets form the 24-hour window. Ties are resolved by domain name.
// The dashboard shows a domain TOP 20, so the hot set keeps 20 entries.
const statsTopDomains = 20

func (s *StatsStore) refreshRanking(now time.Time, force bool) {
	minute := statsMinute(now)
	if !force && s.rankMinute == minute {
		return
	}
	totals := make(map[string]uint64)
	for k, c := range s.ranking {
		if k.Minute >= minute-1439 && k.Minute <= minute {
			totals[k.Target] = statsSaturatingAdd(totals[k.Target], statsSaturatingAdd(c.Upload, c.Download))
		}
	}
	domains := make([]string, 0, len(totals))
	for name, n := range totals {
		if n > 0 {
			domains = append(domains, name)
		}
	}
	sort.Slice(domains, func(i, j int) bool {
		if totals[domains[i]] != totals[domains[j]] {
			return totals[domains[i]] > totals[domains[j]]
		}
		return domains[i] < domains[j]
	})
	if len(domains) > statsTopDomains {
		domains = domains[:statsTopDomains]
	}
	next := make(map[string]time.Time, len(domains))
	for _, name := range domains {
		if start, ok := s.top[name]; ok {
			next[name] = start
		} else {
			next[name] = now.UTC()
		}
	}
	s.top = next
	s.rankMinute = minute
}

func statsAddLatency(c *statsCounts, d time.Duration) {
	// Round up to milliseconds, then to the next power of two; P95 is an upper bound.
	ms := uint64(d / time.Millisecond)
	if d%time.Millisecond != 0 {
		ms++
	}
	i := 0
	if ms > 0 {
		i = 1 + bits.Len64(ms-1)
	}
	c.Latency[i]++
}

// RecordConnection records the result of a real establishment attempt. Record
// bytes either here or through RecordTraffic, not both for the same bytes.
func (s *StatsStore) RecordConnection(e StatsConnection) error {
	target, err := statsTarget(e.Target)
	if err != nil {
		return err
	}
	if !statsLabel(e.Node) {
		return errors.New("invalid node identifier")
	}
	if e.Duration < 0 {
		return errors.New("negative duration")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("stats store closed")
	}
	now := s.now()
	minute, err := s.eventMinute(e.At, now)
	if err != nil {
		return err
	}
	s.prune(now)
	s.refreshRanking(now, false)
	if net.ParseIP(target) == nil {
		k := statsKey{Minute: minute, Target: target, Node: e.Node}
		c := s.connections[k]
		c.Attempts = statsSaturatingAdd(c.Attempts, 1)
		if e.Success {
			c.Successes = statsSaturatingAdd(c.Successes, 1)
		}
		statsAddLatency(&c, e.Duration)
		s.connections[k] = c
		at := e.At
		if at.IsZero() {
			at = now
		}
		if !at.Before(s.attemptsStartedAt) && minute >= statsMinute(now)-1439 {
			rk := statsRankKey{minute, target}
			rc := s.ranking[rk]
			rc.Attempts = statsSaturatingAdd(rc.Attempts, 1)
			s.ranking[rk] = rc
		}
	}
	if e.Upload != 0 || e.Download != 0 {
		s.recordBytesLocked(minute, target, e.Node, 0, e.Upload, e.Download, e.At, now)
	}
	if e.At.IsZero() || !e.At.Before(now.Add(-60*time.Second)) {
		if s.recentConnections == nil {
			s.recentConnections = make(map[int64]statsCounts)
		}
		at := e.At
		if at.IsZero() {
			at = now
		}
		for sec := range s.recentConnections {
			if sec <= now.Unix()-60 {
				delete(s.recentConnections, sec)
			}
		}
		r := s.recentConnections[at.Unix()]
		statsAddLatency(&r, e.Duration)
		s.recentConnections[at.Unix()] = r
	}
	return nil
}

// RecordRecentConnection observes a real connection establishment without adding a target row.
func (s *StatsStore) RecordRecentConnection(duration time.Duration) {
	if duration < 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if s.recentConnections == nil {
		s.recentConnections = make(map[int64]statsCounts)
	}
	now := s.now().Unix()
	for sec := range s.recentConnections {
		if sec <= now-60 {
			delete(s.recentConnections, sec)
		}
	}
	r := s.recentConnections[now]
	statsAddLatency(&r, duration)
	s.recentConnections[now] = r
}

// RecentConnectionP95 merges raw latency histograms, not per-group percentiles.
// Second buckets approximate the rolling window to within one second.
func (s *StatsStore) RecentConnectionP95() (*uint64, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().Unix()
	var merged statsCounts
	for sec, c := range s.recentConnections {
		if sec <= now-60 {
			delete(s.recentConnections, sec)
			continue
		}
		if sec <= now {
			statsMerge(&merged, c)
		}
	}
	var samples uint64
	for _, count := range merged.Latency {
		samples += count
	}
	return statsSummary(merged).P95MSUpper, samples
}

// RecordTraffic records incremental bytes at their observation time, allowing
// long-lived connections to be represented in the appropriate query windows.
func (s *StatsStore) RecordTraffic(at time.Time, target, node string, upload, download uint64) error {
	return s.RecordPortTraffic(at, target, node, 0, upload, download)
}

// RecordPortTraffic records bytes once, with the directly selected upstream port.
func (s *StatsStore) RecordPortTraffic(at time.Time, target, node string, port uint16, upload, download uint64) error {
	target, err := statsTarget(target)
	if err != nil {
		return err
	}
	if !statsLabel(node) {
		return errors.New("invalid node identifier")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("stats store closed")
	}
	now := s.now()
	minute, err := s.eventMinute(at, now)
	if err != nil {
		return err
	}
	s.prune(now)
	s.recordBytesLocked(minute, target, node, port, upload, download, at, now)
	return nil
}
func (s *StatsStore) recordBytesLocked(minute int64, target, node string, port uint16, upload, download uint64, at, now time.Time) {
	pk := statsPortKey{minute, port}
	pc := s.ports[pk]
	pc.Upload = statsSaturatingAdd(pc.Upload, upload)
	pc.Download = statsSaturatingAdd(pc.Download, download)
	s.ports[pk] = pc
	if net.ParseIP(target) != nil {
		s.refreshRanking(now, false)
		return
	}
	history := s.history[target]
	history.Upload = statsSaturatingAdd(history.Upload, upload)
	history.Download = statsSaturatingAdd(history.Download, download)
	s.history[target] = history
	if minute >= statsMinute(now)-1439 {
		rk := statsRankKey{minute, target}
		rc := s.ranking[rk]
		rc.Upload = statsSaturatingAdd(rc.Upload, upload)
		rc.Download = statsSaturatingAdd(rc.Download, download)
		s.ranking[rk] = rc
	}
	s.refreshRanking(now, false)
	k := statsKey{minute, target, node, port}
	c := s.connections[k]
	c.Upload = statsSaturatingAdd(c.Upload, upload)
	c.Download = statsSaturatingAdd(c.Download, download)
	s.connections[k] = c
}
func (s *StatsStore) RecordHealth(e StatsHealth) error {
	if e.Name == "" || !statsLabel(e.Name) || e.Duration < 0 {
		return errors.New("invalid health event")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("stats store closed")
	}
	now := s.now()
	minute, err := s.eventMinute(e.At, now)
	if err != nil {
		return err
	}
	s.prune(now)
	k := statsHealthKey{minute, e.Name}
	c := s.health[k]
	c.Attempts++
	if e.Success {
		c.Successes++
	}
	statsAddLatency(&c, e.Duration)
	s.health[k] = c
	return nil
}

type StatsSummary struct {
	Attempts, Successes, Failures, Upload, Download uint64
	// P95MSUpper is the histogram's inclusive upper bound; nil means no samples.
	P95MSUpper *uint64
}
type StatsRow struct {
	Target, Node    string
	NodeUnknown     bool
	DetailStartedAt time.Time `json:"detail_started_at"`
	StatsSummary
}

// StatsPortRow aggregates payload bytes by the port srhop directly selected.
type StatsPortRow struct {
	Port     uint16 `json:"port"`
	Unknown  bool   `json:"unknown"`
	Upload   uint64 `json:"upload"`
	Download uint64 `json:"download"`
}

func statsValidWindow(minutes int) bool {
	return minutes == 5 || minutes == 10 || minutes == 60 || minutes == 1440 || minutes == 2880
}

func (s *StatsStore) QueryPortRows(minutes int) ([]StatsPortRow, error) {
	if !statsValidWindow(minutes) {
		return nil, errors.New("unsupported stats window")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.prune(now)
	cutoff, end := statsMinute(now.Add(-time.Duration(minutes)*time.Minute)), statsMinute(now)
	grouped := make(map[uint16]StatsPortRow)
	for k, c := range s.ports {
		if k.Minute < cutoff || k.Minute > end || (c.Upload == 0 && c.Download == 0) {
			continue
		}
		r := grouped[k.Port]
		r.Port, r.Unknown = k.Port, k.Port == 0
		r.Upload += c.Upload
		r.Download += c.Download
		grouped[k.Port] = r
	}
	rows := make([]StatsPortRow, 0, len(grouped))
	for _, r := range grouped {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Port < rows[j].Port })
	return rows, nil
}

// StatsSeriesPoint is one fixed-width bucket of observed proxy payload.
type StatsSeriesPoint struct {
	Start    time.Time `json:"start"`
	Upload   uint64    `json:"upload"`
	Download uint64    `json:"download"`
}

// QueryTrafficSeries aggregates every observed port bucket into fixed windows.
// Missing buckets are returned as zero so the client can draw a continuous line.
func (s *StatsStore) QueryTrafficSeries(minutes, bucket int) ([]StatsSeriesPoint, error) {
	if !statsValidWindow(minutes) || bucket <= 0 || bucket > minutes {
		return nil, errors.New("unsupported series window")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.prune(now)
	cutoff, end := statsMinute(now.Add(-time.Duration(minutes)*time.Minute)), statsMinute(now)
	start := cutoff - cutoff%int64(bucket)
	acc := make(map[int64]*StatsSeriesPoint)
	for k, c := range s.ports {
		if k.Minute < cutoff || k.Minute > end || (c.Upload == 0 && c.Download == 0) {
			continue
		}
		b := k.Minute - k.Minute%int64(bucket)
		p := acc[b]
		if p == nil {
			p = &StatsSeriesPoint{}
			acc[b] = p
		}
		p.Upload += c.Upload
		p.Download += c.Download
	}
	out := make([]StatsSeriesPoint, 0, (end-start)/int64(bucket)+1)
	for m := start; m <= end; m += int64(bucket) {
		point := StatsSeriesPoint{Start: time.Unix(m*60, 0).UTC()}
		if v := acc[m]; v != nil {
			point.Upload, point.Download = v.Upload, v.Download
		}
		out = append(out, point)
	}
	return out, nil
}

type StatsHealthHour struct {
	Hour time.Time
	Name string
	StatsSummary
}

func statsMerge(a *statsCounts, b statsCounts) {
	a.Attempts += b.Attempts
	a.Successes += b.Successes
	a.Upload += b.Upload
	a.Download += b.Download
	for i, v := range b.Latency {
		a.Latency[i] += v
	}
}
func statsSummary(c statsCounts) StatsSummary {
	r := StatsSummary{Attempts: c.Attempts, Successes: c.Successes, Upload: c.Upload, Download: c.Download}
	if c.Attempts >= c.Successes {
		r.Failures = c.Attempts - c.Successes
	}
	var total uint64
	for _, n := range c.Latency {
		total += n
	}
	if total > 0 {
		rank := total - total/20
		var n uint64
		for i, v := range c.Latency {
			n += v
			if n >= rank {
				var upper uint64
				if i > 0 {
					upper = uint64(1) << uint(i-1)
				}
				r.P95MSUpper = &upper
				break
			}
		}
	}
	return r
}

// StatsDomainRank reports the independently counted 24-hour traffic and the
// start of the current uninterrupted detail collection period.
type StatsDomainRank struct {
	Target            string    `json:"target"`
	Upload            uint64    `json:"upload"`
	Download          uint64    `json:"download"`
	DetailStartedAt   time.Time `json:"detail_started_at"`
	Attempts          uint64    `json:"attempts"`
	AttemptsStartedAt time.Time `json:"attempts_started_at"`
	AttemptsComplete  bool      `json:"attempts_complete"`
}

func (s *StatsStore) QueryDomainRanking() []StatsDomainRank {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.prune(now)
	s.refreshRanking(now, false)
	totals := make(map[string]StatsDomainRank, len(s.top))
	for target := range s.top {
		// Coverage of every proxy establishment path is not yet guaranteed by
		// the store itself. A time interval alone cannot prove completeness.
		totals[target] = StatsDomainRank{Target: target, DetailStartedAt: s.detailStartedAt, AttemptsStartedAt: s.attemptsStartedAt, AttemptsComplete: false}
	}
	for k, c := range s.ranking {
		r, ok := totals[k.Target]
		if ok && k.Minute <= statsMinute(now) {
			r.Attempts = statsSaturatingAdd(r.Attempts, c.Attempts)
			r.Upload = statsSaturatingAdd(r.Upload, c.Upload)
			r.Download = statsSaturatingAdd(r.Download, c.Download)
			totals[k.Target] = r
		}
	}
	rows := make([]StatsDomainRank, 0, len(totals))
	for _, r := range totals {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := statsSaturatingAdd(rows[i].Upload, rows[i].Download), statsSaturatingAdd(rows[j].Upload, rows[j].Download)
		if a != b {
			return a > b
		}
		return rows[i].Target < rows[j].Target
	})
	return rows
}

// QueryDomainHistory returns cumulative per-domain bytes since the all-time
// table was introduced. Entries are never pruned with the rolling windows.
func (s *StatsStore) QueryDomainHistory() []StatsDomainRank {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := make([]StatsDomainRank, 0, len(s.history))
	for target, c := range s.history {
		if c.Upload == 0 && c.Download == 0 {
			continue
		}
		rows = append(rows, StatsDomainRank{Target: target, Upload: c.Upload, Download: c.Download})
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := statsSaturatingAdd(rows[i].Upload, rows[i].Download), statsSaturatingAdd(rows[j].Upload, rows[j].Download)
		if a != b {
			return a > b
		}
		return rows[i].Target < rows[j].Target
	})
	return rows
}

// Query returns per-target/node totals for 5, 10, 60, 1440 or 2880 minutes and
// health totals grouped by UTC hour. Minute buckets intersecting the window
// are included whole, so boundaries can include up to 59 seconds extra.
func (s *StatsStore) Query(minutes int) ([]StatsRow, []StatsHealthHour, error) {
	if !statsValidWindow(minutes) {
		return nil, nil, errors.New("unsupported stats window")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.prune(now)
	s.refreshRanking(now, false)
	cutoff := statsMinute(now.Add(-time.Duration(minutes) * time.Minute))
	end := statsMinute(now)
	type dimension struct{ Target, Node string }
	grouped := map[dimension]statsCounts{}
	for k, c := range s.connections {
		if k.Minute >= cutoff && k.Minute <= end && net.ParseIP(k.Target) == nil {
			d := dimension{k.Target, k.Node}
			a := grouped[d]
			statsMerge(&a, c)
			grouped[d] = a
		}
	}
	rows := make([]StatsRow, 0, len(grouped))
	for d, c := range grouped {
		rows = append(rows, StatsRow{Target: d.Target, Node: d.Node, NodeUnknown: d.Node == "", DetailStartedAt: s.detailStartedAt, StatsSummary: statsSummary(c)})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Target != rows[j].Target {
			return rows[i].Target < rows[j].Target
		}
		return rows[i].Node < rows[j].Node
	})
	return rows, s.queryHealthLocked(cutoff, end), nil
}

// QueryHealth48Hours returns recorded health samples for the last 48 hours,
// grouped by UTC hour and probe name. Boundary minute buckets are included
// whole; partial first and last hours are not filled with synthetic samples.
func (s *StatsStore) QueryHealth48Hours() []StatsHealthHour {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.prune(now)
	return s.queryHealthLocked(statsMinute(now.Add(-statsRetention)), statsMinute(now))
}

// queryHealthLocked requires s.mu to be held.
func (s *StatsStore) queryHealthLocked(cutoff, end int64) []StatsHealthHour {
	type hourKey struct {
		Hour int64
		Name string
	}
	hours := map[hourKey]statsCounts{}
	for k, c := range s.health {
		if k.Minute >= cutoff && k.Minute <= end {
			h := hourKey{time.Unix(k.Minute*60, 0).UTC().Truncate(time.Hour).Unix(), k.Name}
			a := hours[h]
			statsMerge(&a, c)
			hours[h] = a
		}
	}
	health := make([]StatsHealthHour, 0, len(hours))
	for h, c := range hours {
		health = append(health, StatsHealthHour{Hour: time.Unix(h.Hour, 0).UTC(), Name: h.Name, StatsSummary: statsSummary(c)})
	}
	sort.Slice(health, func(i, j int) bool {
		if !health[i].Hour.Equal(health[j].Hour) {
			return health[i].Hour.Before(health[j].Hour)
		}
		return health[i].Name < health[j].Name
	})
	return health
}

// Flush atomically replaces the snapshot with mode 0600. Call periodically and
// at shutdown; unflushed events are lost on crash. Parent directory must exist.
// Errors are returned to the caller; no credentials should be passed as labels.
func (s *StatsStore) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("stats store closed")
	}
	return s.flushLocked()
}

// closeAndFlush seals the store under the same lock as all mutations.
func (s *StatsStore) closeAndFlush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("stats store closed")
	}
	s.closed = true
	return s.flushLocked()
}
func (s *StatsStore) flushLocked() error {
	now := s.now()
	s.prune(now)
	s.refreshRanking(now, false)
	d := statsDisk{Version: 3, Top: s.top, RankMinute: s.rankMinute, AttemptsStartedAt: s.attemptsStartedAt, DetailStartedAt: s.detailStartedAt}
	for k, c := range s.ranking {
		d.Ranking = append(d.Ranking, statsRankBucket{k, c})
	}
	for k, c := range s.ports {
		d.Ports = append(d.Ports, statsPortBucket{k, c})
	}
	for k, c := range s.connections {
		d.Connections = append(d.Connections, statsConnectionBucket{k, c})
	}
	for k, c := range s.health {
		d.Health = append(d.Health, statsHealthBucket{k, c})
	}
	for target, c := range s.history {
		d.History = append(d.History, statsHistoryBucket{target, c.Upload, c.Download})
	}
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".srhop-stats-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(name, s.path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
