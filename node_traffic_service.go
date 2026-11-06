package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// More frequent snapshots reduce missed short flows, but cannot capture
	// connections that both start and finish between snapshots or their tails.
	nodeTrafficInterval       = 2 * time.Second
	nodeTrafficMaxGap         = 15 * time.Second
	nodeTrafficMaxConnections = 20000
	nodeTrafficMaxNodes       = 2048
	nodeTrafficMaxBuckets     = 200000
	nodeTrafficMaxFile        = 32 << 20
)

type trafficBucketKey struct {
	Minute int64
	Node   string
}
type trafficDiskBucket struct {
	Minute   int64  `json:"minute"`
	Node     string `json:"node"`
	Upload   uint64 `json:"upload"`
	Download uint64 `json:"download"`
}
type trafficDisk struct {
	Version int                 `json:"version"`
	Buckets []trafficDiskBucket `json:"buckets"`
}
type trafficRow struct {
	Node          string   `json:"node"`
	Upload        uint64   `json:"upload"`
	Download      uint64   `json:"download"`
	UploadBPS     *float64 `json:"upload_bps"`
	DownloadBPS   *float64 `json:"download_bps"`
	RateAvailable bool     `json:"rate_available"`
}
type trafficResponse struct {
	UpdatedAt      *time.Time           `json:"updated_at"`
	WindowMinutes  int                  `json:"window_minutes"`
	Nodes          []trafficRow         `json:"nodes"`
	Coverage       string               `json:"coverage"`
	SamplingStatus string               `json:"sampling_status"`
	Unknown        nodeTrafficBytesJSON `json:"unknown_observed"`
	Global         nodeTrafficBytesJSON `json:"global_observed"`
}
type nodeTrafficBytesJSON struct {
	Upload   uint64 `json:"upload"`
	Download uint64 `json:"download"`
}

// The reserved keys are distinct from the hashed node identifiers. Total and
// unknown are independent measurements and cannot be subtracted reliably.
const trafficUnknown = "unknown"
const trafficGlobal = "global"

type nodeTrafficService struct {
	mu              sync.Mutex
	flushMu         sync.Mutex
	path            string
	buckets         map[trafficBucketKey]nodeTrafficBytes
	names           map[string]string // hash -> current name, never persisted
	sampler         nodeTrafficSampler
	rates           map[string]nodeTrafficBytes
	elapsed         time.Duration
	rateOK          bool
	updated         time.Time
	status          string
	cancel          context.CancelFunc
	done            chan struct{}
	client          *http.Client
	logf            func(string, ...any)
	fallbackTraffic func(uint64)
}

func openNodeTraffic(path string, logf func(string, ...any)) (*nodeTrafficService, error) {
	s := &nodeTrafficService{path: path, buckets: make(map[trafficBucketKey]nodeTrafficBytes), names: make(map[string]string), status: "starting", logf: logf}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, nodeTrafficMaxFile+1))
	if err != nil || len(b) > nodeTrafficMaxFile {
		return nil, errors.New("node traffic snapshot too large or unreadable")
	}
	var d trafficDisk
	if json.Unmarshal(b, &d) != nil || d.Version != 1 || len(d.Buckets) > nodeTrafficMaxBuckets {
		return nil, errors.New("invalid node traffic snapshot")
	}
	now := time.Now().Unix() / 60
	for _, v := range d.Buckets {
		if v.Minute > now || v.Minute < now-2880 {
			continue
		}
		if v.Node != trafficGlobal && v.Node != trafficUnknown && (len(v.Node) != 69 || !strings.HasPrefix(v.Node, "node-") || strings.Trim(v.Node[5:], "0123456789abcdef") != "") {
			return nil, errors.New("invalid node traffic bucket")
		}
		k := trafficBucketKey{v.Minute, v.Node}
		if _, ok := s.buckets[k]; ok {
			return nil, errors.New("duplicate node traffic bucket")
		}
		s.buckets[k] = nodeTrafficBytes{v.Upload, v.Download}
	}
	return s, nil
}
func (s *nodeTrafficService) prune(now time.Time) {
	cutoff := now.Unix()/60 - 2880
	for k := range s.buckets {
		if k.Minute < cutoff || k.Minute > now.Unix()/60 {
			delete(s.buckets, k)
		}
	}
}
func (s *nodeTrafficService) add(k trafficBucketKey, v nodeTrafficBytes) bool {
	if v.Upload == 0 && v.Download == 0 {
		return true
	}
	old, exists := s.buckets[k]
	if !exists && len(s.buckets) >= nodeTrafficMaxBuckets {
		return false
	}
	s.buckets[k] = nodeTrafficBytes{statsSaturatingAdd(old.Upload, v.Upload), statsSaturatingAdd(old.Download, v.Download)}
	return true
}
func (s *nodeTrafficService) apply(epoch string, at time.Time, snap mihomoConnectionsSnapshot, names map[string]string) error {
	if len(snap.Connections) > nodeTrafficMaxConnections || len(names) > nodeTrafficMaxNodes {
		return errors.New("controller snapshot exceeds limits")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Require the complete interval to fit; no partially written observation.
	possible := len(names) + 2
	s.prune(at)
	if len(s.buckets)+possible > nodeTrafficMaxBuckets {
		s.rateOK = false
		s.status = "capacity"
		return errors.New("node traffic capacity reached")
	}
	resolve := func(chain []string) string {
		if len(chain) == 0 {
			return ""
		}
		return names[chain[0]]
	}
	o, err := s.sampler.observe(epoch, at, snap, resolve, nodeTrafficMaxGap)
	if err != nil {
		s.rateOK = false
		return err
	}
	if s.fallbackTraffic != nil {
		v := o.Known[nodeHealthName("FALLBACK-33210")]
		s.fallbackTraffic(statsSaturatingAdd(v.Upload, v.Download))
	}
	s.names = make(map[string]string, len(names))
	for name, id := range names {
		s.names[id] = name
	}
	min := at.Unix() / 60
	for id, v := range o.Known {
		s.add(trafficBucketKey{min, id}, v)
	}
	s.add(trafficBucketKey{min, trafficUnknown}, o.Unknown)
	s.add(trafficBucketKey{min, trafficGlobal}, o.Global)
	s.updated = at
	s.rateOK = o.RateAvailable
	s.elapsed = o.Elapsed
	s.rates = o.Known
	s.status = "ok"
	return nil
}
func (s *nodeTrafficService) failed() {
	s.mu.Lock()
	s.rateOK = false
	s.status = "unavailable"
	s.mu.Unlock()
}

// sourceTrafficRow is one per-source traffic row shared by /api/sources and
// /stats. The office91-port row carries the port-33210 fallback bytes and is
// never merged into a source's node rows.
type sourceTrafficRow struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Upload   uint64 `json:"upload"`
	Download uint64 `json:"download"`
}

// trafficBySource aggregates sampled node bytes by configured source.
// perSource maps source id to node bytes; portNode carries the engine's
// FALLBACK node bytes (the port fallback); everything that cannot be mapped
// to a current node name and a bound source stays unknown. History for nodes
// that no longer exist is preserved on disk but reported as unknown.
// currentRate returns the last observed per-node rate, if sampling is healthy.
func (s *nodeTrafficService) currentRate(name string) (nodeTrafficBytes, bool) {
	if s == nil || name == "" {
		return nodeTrafficBytes{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.rateOK || s.elapsed <= 0 {
		return nodeTrafficBytes{}, false
	}
	for id, n := range s.names {
		if n == name {
			return s.rates[id], true
		}
	}
	return nodeTrafficBytes{}, false
}

func (s *nodeTrafficService) trafficBySource(idx sourceIndex, minutes int, now time.Time) (perSource map[string]nodeTrafficBytes, portNode, unknown nodeTrafficBytes) {
	perSource = map[string]nodeTrafficBytes{}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(now)
	cutoff := now.Unix()/60 - int64(minutes)
	add := func(dst *nodeTrafficBytes, v nodeTrafficBytes) {
		dst.Upload = statsSaturatingAdd(dst.Upload, v.Upload)
		dst.Download = statsSaturatingAdd(dst.Download, v.Download)
	}
	for k, v := range s.buckets {
		if k.Minute < cutoff || k.Node == trafficGlobal || k.Node == trafficUnknown {
			continue
		}
		name := s.names[k.Node]
		if name == "FALLBACK-33210" {
			add(&portNode, v)
			continue
		}
		if id := idx.sourceID(name); id != "" {
			cur := perSource[id]
			add(&cur, v)
			perSource[id] = cur
			continue
		}
		add(&unknown, v)
	}
	return perSource, portNode, unknown
}

func (s *nodeTrafficService) query(minutes int, now time.Time) trafficResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(now)
	r := trafficResponse{WindowMinutes: minutes, Nodes: []trafficRow{}, Coverage: "partial", SamplingStatus: s.status}
	if !s.updated.IsZero() {
		at := s.updated.UTC()
		r.UpdatedAt = &at
		if now.Sub(s.updated) > nodeTrafficMaxGap || now.Before(s.updated) {
			r.SamplingStatus = "unavailable"
		}
	}
	grouped := map[string]nodeTrafficBytes{}
	cutoff := now.Unix()/60 - int64(minutes)
	for k, v := range s.buckets {
		if k.Minute < cutoff {
			continue
		}
		x := grouped[k.Node]
		grouped[k.Node] = nodeTrafficBytes{statsSaturatingAdd(x.Upload, v.Upload), statsSaturatingAdd(x.Download, v.Download)}
	}
	global := grouped[trafficGlobal]
	unknown := grouped[trafficUnknown]
	r.Global = nodeTrafficBytesJSON{global.Upload, global.Download}
	r.Unknown = nodeTrafficBytesJSON{unknown.Upload, unknown.Download}
	delete(grouped, trafficGlobal)
	delete(grouped, trafficUnknown)
	for id := range s.names {
		if _, ok := grouped[id]; !ok {
			grouped[id] = nodeTrafficBytes{}
		}
	}
	for id, v := range grouped {
		name := s.names[id]
		if name == "" {
			// A stored hash is not a current controller node name. Keep the
			// history on disk but never expose it as a selectable node.
			continue
		}
		row := trafficRow{Node: name, Upload: v.Upload, Download: v.Download}
		if s.rateOK && r.SamplingStatus == "ok" && s.elapsed > 0 { // Removed or unrecognized nodes cannot be claimed to have zero current rate.
			if _, ok := s.names[id]; ok {
				rate := s.rates[id]
				up, down := float64(rate.Upload)/s.elapsed.Seconds(), float64(rate.Download)/s.elapsed.Seconds()
				row.UploadBPS = &up
				row.DownloadBPS = &down
				row.RateAvailable = true
			}
		}
		r.Nodes = append(r.Nodes, row)
	}
	sort.Slice(r.Nodes, func(i, j int) bool { return r.Nodes[i].Node < r.Nodes[j].Node })
	return r
}
func (s *nodeTrafficService) flush() error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.mu.Lock()
	s.prune(time.Now())
	d := trafficDisk{Version: 1, Buckets: make([]trafficDiskBucket, 0, len(s.buckets))}
	for k, v := range s.buckets {
		d.Buckets = append(d.Buckets, trafficDiskBucket{k.Minute, k.Node, v.Upload, v.Download})
	}
	s.mu.Unlock()
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if len(b) > nodeTrafficMaxFile {
		return errors.New("node traffic snapshot too large")
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".node-traffic-*")
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
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, s.path)
}

// Only recognized leaf outbound types are attributed. The first chain entry is
// the actual outbound, not a selector's current choice.
func trafficProxyNames(raw map[string]proxyInfo) (map[string]string, error) {
	if len(raw) > nodeTrafficMaxNodes*4 {
		return nil, errors.New("too many proxies")
	}
	names := map[string]string{}
	for name, p := range raw {
		if name == "" || len(name) > 512 || len(p.All) > 0 {
			continue
		}
		switch strings.ToLower(p.Type) {
		case "ss", "shadowsocks", "ssr", "shadowsocksr", "vmess", "vless", "trojan", "hysteria", "hysteria2", "tuic", "wireguard", "http", "socks5", "snell", "ssh", "anytls":
			names[name] = nodeHealthName(name)
		}
	}
	if len(names) > nodeTrafficMaxNodes {
		return nil, errors.New("too many nodes")
	}
	return names, nil
}
func trafficGet(ctx context.Context, client *http.Client, base, secret, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return errors.New("invalid controller request")
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("controller unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return errors.New("controller rejected request")
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20+1))
	if err != nil || len(b) > 8<<20 || json.Unmarshal(b, out) != nil {
		return errors.New("invalid controller response")
	}
	return nil
}
func (s *nodeTrafficService) start(a *App) error {
	host, _, err := net.SplitHostPort(a.cfg.Controller)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		s.failed()
		return errors.New("controller must be literal loopback")
	}
	s.fallbackTraffic = a.recordFallbackTraffic
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan struct{})
	s.client = &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil, MaxIdleConns: 2, IdleConnTimeout: 30 * time.Second}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	go s.run(ctx, a, "http://"+a.cfg.Controller)
	return nil
}
func (s *nodeTrafficService) run(ctx context.Context, a *App, base string) {
	defer close(s.done)
	defer s.client.CloseIdleConnections()
	ticker := time.NewTicker(nodeTrafficInterval)
	defer ticker.Stop()
	flush := time.NewTicker(time.Minute)
	defer flush.Stop()
	lastFailure := false
	flushFailure := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-flush.C:
			if err := s.flush(); err != nil && s.logf != nil && !flushFailure {
				s.logf("node traffic flush failed")
				flushFailure = true
			} else if err == nil {
				flushFailure = false
			}
		case <-ticker.C:
			err := s.poll(ctx, a, base)
			if err != nil {
				s.failed()
				if !lastFailure && s.logf != nil {
					s.logf("node traffic sampling unavailable")
				}
				lastFailure = true
			} else if lastFailure {
				if s.logf != nil {
					s.logf("node traffic sampling resumed")
				}
				lastFailure = false
			}
		}
	}
}
func (s *nodeTrafficService) poll(ctx context.Context, a *App, base string) error {
	a.engineMu.Lock()
	p := a.ownedEngineLocked()
	generation := a.engineGeneration
	if p == nil {
		a.engineMu.Unlock()
		return errors.New("engine unavailable")
	}
	select {
	case <-p.done:
		a.engineMu.Unlock()
		return errors.New("engine unavailable")
	default:
	}
	a.engineMu.Unlock()
	var proxies struct {
		Proxies map[string]proxyInfo `json:"proxies"`
	}
	if err := trafficGet(ctx, s.client, base, engineSecret(), "/proxies", &proxies); err != nil {
		return err
	}
	if proxies.Proxies == nil {
		return errors.New("missing proxies")
	}
	names, err := trafficProxyNames(proxies.Proxies)
	if err != nil {
		return err
	}
	// Use the request start, not completion, as the new-connection boundary.
	at := time.Now()
	var snap mihomoConnectionsSnapshot
	if err = trafficGet(ctx, s.client, base, engineSecret(), "/connections", &snap); err != nil {
		return err
	}
	a.engineMu.Lock()
	defer a.engineMu.Unlock()
	if a.engineGeneration != generation || a.ownedEngineLocked() != p {
		return errors.New("engine changed")
	}
	select {
	case <-p.done:
		return errors.New("engine stopped")
	default:
	}
	return s.apply(fmt.Sprintf("%p:%d", p, generation), at, snap, names)
}
func (s *nodeTrafficService) close() {
	if s == nil {
		return
	}
	if s.cancel != nil {
		s.cancel()
		<-s.done
	}
	if err := s.flush(); err != nil && s.logf != nil {
		s.logf("node traffic flush failed")
	}
}
func (s *statsIntegration) serveNodeTraffic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.nodeTraffic == nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	minutes := 60
	if raw := r.URL.Query().Get("minutes"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 2880 {
			http.Error(w, "invalid minutes", http.StatusBadRequest)
			return
		}
		minutes = n
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(s.nodeTraffic.query(minutes, time.Now()))
}
