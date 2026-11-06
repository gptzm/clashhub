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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// statsIntegration owns the store and its loopback-only HTTP endpoint.
type statsIntegration struct {
	store                *StatsStore
	server               *http.Server
	stop                 chan struct{}
	done                 chan struct{}
	logf                 func(string, ...any)
	diagnostics          *nodeDiagnostics
	nodeTraffic          *nodeTrafficService
	lifecycleLock        *os.File
	sourceMu             sync.Mutex // serializes /api/sources config rewrites
	registerSubscription func(context.Context, SourceConfig, bool) bool
	sourceApplyMu        sync.RWMutex
	sourceApply          func(context.Context, func() error) error
}

// The stable lock file is never removed; hold it until final snapshot is durable.
func lockStatsLifecycle(path string) (*os.File, error) {
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open stats lock: %w", err)
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("stats already in use: %w", err)
	}
	return lock, nil
}

func startStatsIntegration(logf func(string, ...any)) (*statsIntegration, error) {
	path := filepath.Join(appSupportDir(), "stats.json")
	lock, err := lockStatsLifecycle(path)
	if err != nil {
		return nil, err
	}
	store, err := OpenStats(path)
	if err != nil {
		lock.Close()
		return nil, fmt.Errorf("open stats: %w", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:33335")
	if err != nil {
		lock.Close()
		return nil, fmt.Errorf("listen stats: %w", err)
	}
	s := &statsIntegration{store: store, stop: make(chan struct{}), done: make(chan struct{}), logf: logf, lifecycleLock: lock, diagnostics: newNodeDiagnostics()}
	s.nodeTraffic, err = openNodeTraffic(filepath.Join(appSupportDir(), "node-traffic.json"), logf)
	if err != nil {
		logf("node traffic unavailable; existing history was not modified")
	}
	s.diagnostics.traffic = s.nodeTraffic
	s.server = &http.Server{Handler: http.HandlerFunc(s.serveHTTP), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := s.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.logf("stats server: %v", err)
		}
	}()
	go func() {
		defer close(s.done)
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-t.C:
				if err := s.store.Flush(); err != nil {
					s.logf("flush stats: %v", err)
				}
			}
		}
	}()
	return s, nil
}

func (s *statsIntegration) close() {
	if s == nil {
		return
	}
	_ = s.server.Close()
	close(s.stop)
	<-s.done
	s.nodeTraffic.close()
	if err := s.store.closeAndFlush(); err != nil {
		s.logf("flush stats at shutdown: %v", err)
	}
	if s.lifecycleLock != nil {
		_ = s.lifecycleLock.Close()
	}
}

func (s *statsIntegration) serveHTTP(w http.ResponseWriter, r *http.Request) {
	// Reject browser cross-origin access, including requests which need no preflight,
	// and DNS rebinding via arbitrary Host headers.
	host, port, err := net.SplitHostPort(r.Host)
	remote, _, remoteErr := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(remote)
	if err != nil || host != "127.0.0.1" || port != "33335" || remoteErr != nil || ip == nil || !ip.IsLoopback() || r.URL.IsAbs() || r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.URL.Path == "/api/sources" {
		if r.URL.IsAbs() || r.Header.Get("Sec-Fetch-Site") != "" || !authorizeNodeDiagnostics(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		s.serveSources(w, r)
		return
	}
	if r.URL.Path == "/api/sources/subscriptions" {
		if r.URL.IsAbs() || r.Header.Get("Sec-Fetch-Site") != "" || !authorizeNodeDiagnostics(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		s.serveSubscriptionSources(w, r)
		return
	}
	if r.URL.Path == "/api/sources/update" {
		if r.URL.IsAbs() || r.Header.Get("Sec-Fetch-Site") != "" || !authorizeNodeDiagnostics(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		s.serveSourcesUpdate(w, r)
		return
	}
	if r.URL.Path == "/api/office91-module" {
		if r.URL.IsAbs() || r.Header.Get("Sec-Fetch-Site") != "" || !authorizeNodeDiagnostics(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		s.serveOffice91Module(w, r)
		return
	}
	if r.URL.Path == "/api/node-health" || r.URL.Path == "/api/node-health/probe" || r.URL.Path == "/api/node-traffic" {
		if r.URL.IsAbs() || r.Header.Get("Sec-Fetch-Site") != "" || !authorizeNodeDiagnostics(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/api/node-traffic" {
			s.serveNodeTraffic(w, r)
		} else {
			s.serveNodeDiagnostics(w, r)
		}
		return
	}
	if r.URL.Path != "/stats" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	minutes := 60
	if v := r.URL.Query().Get("minutes"); v != "" {
		minutes, err = strconv.Atoi(v)
		if err != nil {
			http.Error(w, "invalid minutes", http.StatusBadRequest)
			return
		}
	}
	rows, windowHours, err := s.store.Query(minutes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	hours := s.store.QueryHealth48Hours()
	portRows, _ := s.store.QueryPortRows(minutes)
	series, _ := s.store.QueryTrafficSeries(minutes, 10)
	p95, samples := s.store.RecentConnectionP95()
	totals, started := s.store.processTraffic()
	sources := s.buildSourcesReport(minutes)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		Rows              []StatsRow         `json:"rows"`
		PortRows          []StatsPortRow     `json:"port_rows"`
		DomainRanking     []StatsDomainRank  `json:"domain_ranking"`
		DomainHistory     []StatsDomainRank  `json:"domain_history"`
		HealthHours       []StatsHealthHour  `json:"health_hours"`
		WindowHealthHours []StatsHealthHour  `json:"window_health_hours"`
		P95               *uint64            `json:"p95_60s_ms_upper"`
		Samples           uint64             `json:"p95_60s_samples"`
		UpdatedAt         time.Time          `json:"updated_at"`
		TrafficTotals     StatsTrafficTotals `json:"traffic_totals"`
		ProcessStartedAt  time.Time          `json:"process_started_at"`
		SourceRows        []sourceTrafficRow `json:"source_rows"`
		TrafficSeries     []StatsSeriesPoint `json:"traffic_series"`
	}{rows, portRows, s.store.QueryDomainRanking(), s.store.QueryDomainHistory(), hours, windowHours, p95, samples, time.Now().UTC(), totals, started, sourceRowsWithUnknown(sources), series})
}

// sourceRowsWithUnknown appends the unknown row so /stats carries the same
// row schema as /api/sources traffic rows plus the unknown remainder.
func sourceRowsWithUnknown(report sourcesReport) []sourceTrafficRow {
	rows := make([]sourceTrafficRow, 0, len(report.Traffic.Rows)+1)
	rows = append(rows, report.Traffic.Rows...)
	rows = append(rows, sourceTrafficRow{ID: sourceUnknown, Label: sourceUnknown, Upload: report.Traffic.UnknownUpload, Download: report.Traffic.UnknownDownload})
	return rows
}

type sourceStatusRow struct {
	ID                  string   `json:"id"`
	Label               string   `json:"label"`
	Type                string   `json:"type"`
	Enabled             bool     `json:"enabled"`
	NodeCount           int      `json:"node_count"`
	Status              string   `json:"status"`
	LastError           string   `json:"last_error"`
	CanEditSubscription bool     `json:"can_edit_subscription"`
	IncludeKeywords     []string `json:"include_keywords"`
	ExcludeKeywords     []string `json:"exclude_keywords"`
}

type sourcesTrafficJSON struct {
	Rows            []sourceTrafficRow `json:"rows"`
	UnknownUpload   uint64             `json:"unknown_upload"`
	UnknownDownload uint64             `json:"unknown_download"`
	Note            string             `json:"note"`
}

type sourcesReport struct {
	Sources         []sourceStatusRow  `json:"sources"`
	Traffic         sourcesTrafficJSON `json:"traffic"`
	BaseRulesSource string             `json:"base_rules_source"`
	PriorityActive  bool               `json:"priority_active"`
	PriorityStatus  string             `json:"priority_status"`
}

// buildSourcesReport assembles the per-source view: manifest rows in priority
// order, node traffic aggregated by source id, and the office91-port row with
// the 33210 fallback bytes (stats port row plus the engine's fallback node).
// Attribution requires node-labels.json to be bound to the current engine
// config; otherwise node traffic is reported as unknown.
func (s *statsIntegration) buildSourcesReport(minutes int) sourcesReport {
	idx := loadSourceIndex()
	report := sourcesReport{
		Sources:         []sourceStatusRow{},
		Traffic:         sourcesTrafficJSON{Rows: []sourceTrafficRow{}, Note: "observed-partial"},
		BaseRulesSource: idx.baseRules,
		PriorityActive:  idx.priorityActive(),
		PriorityStatus:  idx.reason,
	}
	nodeCount := map[string]int{}
	if idx.bound {
		for _, id := range idx.nodeSource {
			nodeCount[id]++
		}
	}
	var perSource map[string]nodeTrafficBytes
	var portNode, unknown nodeTrafficBytes
	if s.nodeTraffic != nil {
		perSource, portNode, unknown = s.nodeTraffic.trafficBySource(idx, minutes, time.Now())
	}
	var portBytes nodeTrafficBytes
	if s.store != nil {
		if rows, err := s.store.QueryPortRows(minutes); err == nil {
			for _, r := range rows {
				if r.Port == idx.fallbackPort {
					portBytes.Upload, portBytes.Download = r.Upload, r.Download
					break
				}
			}
		}
	}
	for _, d := range idx.defs {
		v := perSource[d.ID]
		status := sourceStatusOK
		lastError := ""
		switch {
		case !d.Enabled:
			status = "disabled"
		case d.Type == SourceTypeSubscription:
			// A subscription counts as available when its bound cache holds
			// nodes, even if the latest refresh failed (cache-based); the
			// refresh error is still surfaced via last_error.
			if nodeCount[d.ID] > 0 {
				if st, ok := subscriptionStatusSnapshot(d.ID); ok && st.Status == "error" {
					lastError = st.Error
				}
			} else if st, ok := subscriptionStatusSnapshot(d.ID); ok {
				switch {
				case st.Status == "ok":
					status = "empty"
				case st.Status == "unknown":
					status = sourceUnknown
				default:
					status = "unavailable"
					lastError = st.Error
				}
			} else {
				status = sourceUnknown
			}
		case !idx.bound:
			status = sourceUnknown
		case nodeCount[d.ID] == 0:
			status = "empty"
		}
		report.Sources = append(report.Sources, sourceStatusRow{
			ID: d.ID, Label: idx.label(d.ID), Type: d.Type, Enabled: d.Enabled,
			NodeCount: nodeCount[d.ID], Status: status, LastError: lastError,
			CanEditSubscription: d.Type == SourceTypeSubscription && strings.HasPrefix(d.ID, customSubscriptionSourcePrefix),
			IncludeKeywords:     append([]string{}, d.IncludeKeywords...), ExcludeKeywords: append([]string{}, d.ExcludeKeywords...),
		})
		report.Traffic.Rows = append(report.Traffic.Rows, sourceTrafficRow{ID: d.ID, Label: idx.label(d.ID), Upload: v.Upload, Download: v.Download})
	}
	portLabel := idx.label(sourceOffice91)
	if portLabel == "" {
		portLabel = sourceOffice91
	}
	report.Traffic.Rows = append(report.Traffic.Rows, sourceTrafficRow{
		ID:       sourcePortRowID,
		Label:    portLabel + sourcePortNote,
		Upload:   statsSaturatingAdd(portBytes.Upload, portNode.Upload),
		Download: statsSaturatingAdd(portBytes.Download, portNode.Download),
	})
	report.Traffic.UnknownUpload = unknown.Upload
	report.Traffic.UnknownDownload = unknown.Download
	return report
}

type sourcesUpdateRequest struct {
	Sources []sourceUpdateEntry `json:"sources"`
}

func (s *statsIntegration) serveSources(w http.ResponseWriter, r *http.Request) {
	minutes := 60
	if raw := r.URL.Query().Get("minutes"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || !statsValidWindow(n) {
			http.Error(w, "invalid minutes", http.StatusBadRequest)
			return
		}
		minutes = n
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Method == http.MethodPut {
		if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
			http.Error(w, "JSON required", http.StatusUnsupportedMediaType)
			return
		}
		var req sourcesUpdateRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
		dec.DisallowUnknownFields()
		if dec.Decode(&req) != nil || dec.Decode(&struct{}{}) != io.EOF || len(req.Sources) == 0 {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if _, ok := loadNodeSources(); !ok {
			http.Error(w, "sources not configured", http.StatusConflict)
			return
		}
		s.sourceMu.Lock()
		s.sourceApplyMu.RLock()
		apply := s.sourceApply
		s.sourceApplyMu.RUnlock()
		var err error
		if apply == nil {
			s.sourceMu.Unlock()
			http.Error(w, "source runtime unavailable; changes not applied", http.StatusServiceUnavailable)
			return
		} else {
			err = apply(r.Context(), func() error {
				return persistSourcesOrder(configPath(), req.Sources)
			})
		}
		s.sourceMu.Unlock()
		if err != nil {
			if errors.Is(err, errSourcesUpdateRejected) {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			http.Error(w, "config write failed", http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(s.buildSourcesReport(minutes))
}

func (s *statsIntegration) connection(target string, start time.Time, err error) {
	if s == nil {
		return
	}
	if e := s.store.RecordConnection(StatsConnection{Target: target, Success: err == nil, Duration: time.Since(start)}); e != nil {
		s.logf("record connection stats: %v", e)
	}
}

func (s *statsIntegration) health(name string, start time.Time, err error) {
	if s == nil {
		return
	}
	if e := s.store.RecordHealth(StatsHealth{Name: name, Success: err == nil, Duration: time.Since(start)}); e != nil {
		s.logf("record health stats: %v", e)
	}
}

func selectedPort(addr string) uint16 {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseUint(p, 10, 16)
	if err != nil {
		return 0
	}
	return uint16(n)
}

type statsReader struct {
	reader  io.ReadCloser
	stats   *statsIntegration
	target  string
	port    uint16
	onBytes func(uint64)
}

func (r statsReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		recordRouteTraffic(r.target, false, uint64(n))
	}
	if n > 0 && r.onBytes != nil {
		r.onBytes(uint64(n))
	}
	if n > 0 && r.stats != nil {
		r.stats.store.recordProcessTraffic(uint64(n), 0)
		if e := r.stats.store.RecordPortTraffic(time.Time{}, r.target, "", r.port, uint64(n), 0); e != nil {
			r.stats.logf("record traffic stats: %v", e)
		}
	}
	return n, err
}

func (r statsReader) Close() error { return r.reader.Close() }

// statsWriter counts bytes actually accepted by the destination, on each write.
// This makes ongoing tunnels visible without waiting for them to close.
type statsWriter struct {
	writer  io.Writer
	stats   *statsIntegration
	target  string
	port    uint16
	upload  bool
	onBytes func(uint64)
}

func (w statsWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if n > 0 {
		recordRouteTraffic(w.target, w.upload, uint64(n))
	}
	if n > 0 && w.onBytes != nil {
		w.onBytes(uint64(n))
	}
	if n > 0 && w.stats != nil {
		var up, down uint64
		if w.upload {
			up = uint64(n)
		} else {
			down = uint64(n)
		}
		w.stats.store.recordProcessTraffic(up, down)
		if e := w.stats.store.RecordPortTraffic(time.Time{}, w.target, "", w.port, up, down); e != nil {
			w.stats.logf("record traffic stats: %v", e)
		}
	}
	return n, err
}
