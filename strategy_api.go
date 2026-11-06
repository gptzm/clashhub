package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
)

type strategyPayload struct {
	PrimaryStrategy string `json:"primary_strategy"`
	AIStrategy      string `json:"ai_strategy"`
	LatencyGapMS    int    `json:"latency_gap_ms"`
	BalanceGapMS    int    `json:"balance_gap_ms"`
}

const (
	minGapMS = 10
	maxGapMS = 10000
)

// serveStrategy exposes the per-route node selection strategy. PUT validates,
// persists the four keys into the config document, and applies the values to
// the running App so sweeps pick them up without a restart.
func (a *App) serveStrategy(w http.ResponseWriter, r *http.Request) {
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
		var req strategyPayload
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		dec.DisallowUnknownFields()
		if dec.Decode(&req) != nil || dec.Decode(&struct{}{}) != io.EOF {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		primary := normalizeStrategy(req.PrimaryStrategy)
		ai := normalizeStrategy(req.AIStrategy)
		if primary == "" || ai == "" {
			http.Error(w, "strategy must be stability, balanced or latency", http.StatusBadRequest)
			return
		}
		if !validGapMS(req.LatencyGapMS) || !validGapMS(req.BalanceGapMS) {
			http.Error(w, "gap must be an integer between 10 and 10000", http.StatusBadRequest)
			return
		}
		if err := persistStrategy(configPath(), primary, ai, req.LatencyGapMS, req.BalanceGapMS); err != nil {
			a.logf("strategy persist failed: %v", err)
			http.Error(w, "config write failed", http.StatusInternalServerError)
			return
		}
		a.setStrategyValues(primary, ai, req.LatencyGapMS, req.BalanceGapMS)
		// Keep cfg aligned so a later restart and full config saves agree.
		a.cfg.PrimaryStrategy, a.cfg.AIStrategy = primary, ai
		a.cfg.LatencyGapMS, a.cfg.BalanceGapMS = req.LatencyGapMS, req.BalanceGapMS
	}
	primary, ai, latencyGap, balanceGap := a.strategySnapshot()
	_ = json.NewEncoder(w).Encode(strategyPayload{
		PrimaryStrategy: primary,
		AIStrategy:      ai,
		LatencyGapMS:    latencyGap,
		BalanceGapMS:    balanceGap,
	})
}

func validGapMS(v int) bool {
	return v >= minGapMS && v <= maxGapMS
}

var errInvalidStrategyConfig = errors.New("invalid strategy config document")

// persistStrategy writes the four strategy keys while preserving every other
// key and value in the config document.
func persistStrategy(path, primary, ai string, latencyGap, balanceGap int) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(b, &doc) != nil || len(doc) == 0 {
		return errInvalidStrategyConfig
	}
	set := func(key string, v any) error {
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		doc[key] = raw
		return nil
	}
	if err := set("primary_strategy", primary); err != nil {
		return err
	}
	if err := set("ai_strategy", ai); err != nil {
		return err
	}
	if err := set("latency_gap_ms", latencyGap); err != nil {
		return err
	}
	if err := set("balance_gap_ms", balanceGap); err != nil {
		return err
	}
	out, err := json.MarshalIndent(doc, "", " ")
	if err != nil {
		return err
	}
	return atomicWriteFile(path, out, 0o600)
}
