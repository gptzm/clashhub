package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Routing mode is independent of the primary/fallback health state.
func routingModePath() string { return filepath.Join(appSupportDir(), "routing-mode.json") }
func validRoutingMode(mode string) bool {
	return mode == "rule" || mode == "global" || mode == "direct"
}
func readRoutingMode() (string, error) {
	b, err := os.ReadFile(routingModePath())
	if os.IsNotExist(err) {
		return "rule", nil
	}
	if err != nil {
		return "", err
	}
	var v struct {
		Mode string `json:"mode"`
	}
	if json.Unmarshal(b, &v) != nil || !validRoutingMode(v.Mode) {
		return "", fmt.Errorf("invalid routing mode preference")
	}
	return v.Mode, nil
}
func saveRoutingMode(mode string) error {
	return saveNodeSelection(routingModePath(), map[string]string{"mode": mode})
}

// A mode commit cancels setup and pending headers without waiting for network
// IO. Already handed-off tunnels and response streams remain open; older
// requests cannot start a new proxy attempt after this commit.
func (a *App) commitRoutingMode(mode, previous string) error {
	a.routingGate.Lock()
	defer a.routingGate.Unlock()
	if err := saveRoutingMode(mode); err != nil {
		return err
	}
	if mode != previous {
		a.routingGeneration++
		a.cancelRoutingAttemptsLocked()
		a.setMode(ModeProbing, "routing mode changed; primary health unverified")
		a.mu.Lock()
		a.probeEnd = time.Now().Add(20 * time.Second)
		a.primaryFails = 0
		a.probeMS = 0
		a.egress = ""
		a.egressOK = false
		a.lastEgress = time.Time{}
		a.mu.Unlock()
		_ = a.persist()
	}
	return nil
}

func (a *App) directRouting() bool {
	mode, err := readRoutingMode()
	return err == nil && mode == "direct"
}
func (a *App) routingController() (*nodeProber, error) { return a.selectionProber() }
func controllerRoutingMode(ctx context.Context, p *nodeProber) (string, error) {
	var v struct {
		Mode string `json:"mode"`
	}
	if err := p.get(ctx, "/configs", nil, &v, p.timeout); err != nil {
		return "", err
	}
	v.Mode = strings.ToLower(v.Mode)
	if !validRoutingMode(v.Mode) {
		return "", fmt.Errorf("invalid controller routing mode")
	}
	return v.Mode, nil
}
func setControllerRoutingMode(ctx context.Context, p *nodeProber, mode string) error {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"mode": mode})
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, p.base+"/configs", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("controller returned HTTP %d", resp.StatusCode)
	}
	return nil
}
func configureGlobal(ctx context.Context, p *nodeProber) error {
	snap, err := p.snapshot(ctx)
	if err != nil {
		return err
	}
	group, ok := snap.Proxies["GLOBAL"]
	if !ok || !strings.EqualFold(group.Type, "selector") || !containsNode(group.All, "主代理") {
		return fmt.Errorf("GLOBAL selector unavailable")
	}
	if group.Now != "主代理" {
		if err := p.selectNode(ctx, "GLOBAL", "主代理"); err != nil {
			return err
		}
	}
	snap, err = p.snapshot(context.Background())
	if err != nil {
		return err
	}
	if snap.Proxies["GLOBAL"].Now != "主代理" {
		return fmt.Errorf("GLOBAL selector readback mismatch")
	}
	return nil
}
func (a *App) restoreRoutingModeLocked() error {
	desired, err := readRoutingMode()
	if err != nil {
		return err
	}
	p, err := a.routingController()
	if err != nil {
		return err
	}
	defer p.client.CloseIdleConnections()
	if desired == "global" {
		if err := configureGlobal(context.Background(), p); err != nil {
			return err
		}
	}
	actual, err := controllerRoutingMode(context.Background(), p)
	if err != nil {
		return err
	}
	if actual != desired {
		if err = setControllerRoutingMode(context.Background(), p, desired); err != nil {
			return err
		}
	}
	actual, err = controllerRoutingMode(context.Background(), p)
	if err != nil {
		return err
	}
	if actual != desired {
		return fmt.Errorf("routing mode readback mismatch")
	}
	return nil
}
func (a *App) serveRoutingMode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Mode string `json:"mode"`
	}
	if r.Method == http.MethodPut {
		if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
			http.Error(w, "JSON required", 415)
			return
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		dec.DisallowUnknownFields()
		if dec.Decode(&req) != nil || dec.Decode(&struct{}{}) != io.EOF || !validRoutingMode(req.Mode) {
			http.Error(w, "invalid request", 400)
			return
		}
	}
	a.engineMu.Lock()
	defer a.engineMu.Unlock()
	if a.shuttingDown.Load() {
		http.Error(w, "shutting down", 503)
		return
	}
	desired, err := readRoutingMode()
	if err != nil {
		http.Error(w, "preference read failed", 500)
		return
	}
	p, err := a.routingController()
	if err != nil {
		http.Error(w, "controller unavailable", 502)
		return
	}
	defer p.client.CloseIdleConnections()
	actual, err := controllerRoutingMode(r.Context(), p)
	if r.Method == http.MethodGet {
		warning := ""
		if err != nil {
			actual = ""
			warning = "controller_unavailable"
		} else if actual != desired {
			warning = "mode_not_active"
		}
		if actual == "global" {
			snap, e := p.snapshot(r.Context())
			if e != nil || snap.Proxies["GLOBAL"].Now != "主代理" {
				warning = "global_selector_not_active"
			}
		}
		health, _ := a.snapshotMode()
		fallback := desired != "direct" && health == ModeFallback
		_ = json.NewEncoder(w).Encode(map[string]any{"desired_mode": desired, "actual_mode": actual, "warning": warning, "health_mode": health, "fallback_uncontrolled": fallback})
		return
	}
	if err != nil {
		http.Error(w, "controller unavailable", 502)
		return
	}
	previous := actual
	previousGlobal := ""
	if req.Mode == "global" {
		snap, e := p.snapshot(r.Context())
		if e != nil {
			http.Error(w, "controller unavailable", 502)
			return
		}
		previousGlobal = snap.Proxies["GLOBAL"].Now
		if previousGlobal == "" {
			http.Error(w, "GLOBAL selector unavailable", 409)
			return
		}
	}
	// A failed or timed-out controller mutation can still change the route.
	// Invalidate scans and published health before any potential write.
	if req.Mode != desired || req.Mode != actual || (req.Mode == "global" && previousGlobal != "主代理") {
		a.advanceEngineGenerationLocked()
		a.invalidateAIHealth()
		_ = a.persist()
		select {
		case a.nodeScanWake <- struct{}{}:
		default:
		}
	}
	failure := func(reason string) {
		rollback := "not_attempted"
		selectorRestored := previousGlobal == ""
		if previousGlobal != "" {
			if p.selectNode(context.Background(), "GLOBAL", previousGlobal) == nil {
				snap, e := p.snapshot(context.Background())
				selectorRestored = e == nil && snap.Proxies["GLOBAL"].Now == previousGlobal
			}
		}
		if setControllerRoutingMode(context.Background(), p, previous) == nil {
			rollback = "requested"
		}
		now, e := controllerRoutingMode(context.Background(), p)
		if e != nil {
			now = ""
		} else if now == previous && selectorRestored {
			rollback = "confirmed"
		}
		w.WriteHeader(502)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": reason, "desired_mode": desired, "actual_mode": now, "preference_changed": false, "rollback": rollback, "warning": "controller state may change after a timed-out request; read back again"})
	}
	if req.Mode == "global" {
		if err := configureGlobal(r.Context(), p); err != nil {
			failure("global_selector_failed")
			return
		}
	}
	if actual != req.Mode {
		if err := setControllerRoutingMode(r.Context(), p, req.Mode); err != nil {
			failure("mode_update_failed")
			return
		}
	}
	actual, err = controllerRoutingMode(context.Background(), p)
	if err != nil || actual != req.Mode {
		failure("mode_readback_failed")
		return
	}
	if err = a.commitRoutingMode(req.Mode, desired); err != nil {
		failure("preference_write_failed")
		return
	}
	health, _ := a.snapshotMode()
	_ = json.NewEncoder(w).Encode(map[string]any{"desired_mode": req.Mode, "actual_mode": actual, "warning": "", "health_mode": health, "fallback_uncontrolled": req.Mode != "direct" && health == ModeFallback})
}
