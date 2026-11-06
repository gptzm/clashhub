package main

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Route traffic separates AI-service-bound bytes from everything else at the
// single place every request passes through: srhop's own proxy listeners. It
// therefore stays meaningful when both routes share one upstream port or node.
//
// The AI side follows the configured ai_targets selection: only the official
// domains of the selected providers are counted as AI traffic, so selecting
// claude or grok does not silently keep measuring OpenAI.
var routeTraffic struct {
	normalUp, normalDown atomic.Uint64
	aiUp, aiDown         atomic.Uint64
	mu                   sync.RWMutex
	normalUpBPS          float64
	normalDownBPS        float64
	aiUpBPS              float64
	aiDownBPS            float64
	updated              time.Time
	lastNormalUp         uint64
	lastNormalDown       uint64
	lastAIUp             uint64
	lastAIDown           uint64
	lastAt               time.Time
}

var aiRouteDomains atomic.Value // []string

func init() {
	// Until the daemon applies its config, keep the historical OpenAI default.
	setAIRouteDomains(aiTargetDefinitions()["openai"].Domains)
}

func setAIRouteDomains(domains []string) {
	aiRouteDomains.Store(append([]string(nil), domains...))
}

func isAITarget(target string) bool {
	host := target
	if h, _, err := net.SplitHostPort(target); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" {
		return false
	}
	domains, _ := aiRouteDomains.Load().([]string)
	for _, suffix := range domains {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

func recordRouteTraffic(target string, upload bool, n uint64) {
	if n == 0 {
		return
	}
	if isAITarget(target) {
		if upload {
			routeTraffic.aiUp.Add(n)
		} else {
			routeTraffic.aiDown.Add(n)
		}
		return
	}
	if upload {
		routeTraffic.normalUp.Add(n)
	} else {
		routeTraffic.normalDown.Add(n)
	}
}

// routeTrafficLoop turns the monotonic counters into per-second rates.
func (a *App) routeTrafficLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-a.stopCh:
			return
		case now := <-ticker.C:
			normalUp := routeTraffic.normalUp.Load()
			normalDown := routeTraffic.normalDown.Load()
			aiUp := routeTraffic.aiUp.Load()
			aiDown := routeTraffic.aiDown.Load()
			routeTraffic.mu.Lock()
			if !routeTraffic.lastAt.IsZero() && now.After(routeTraffic.lastAt) &&
				normalUp >= routeTraffic.lastNormalUp && normalDown >= routeTraffic.lastNormalDown &&
				aiUp >= routeTraffic.lastAIUp && aiDown >= routeTraffic.lastAIDown {
				secs := now.Sub(routeTraffic.lastAt).Seconds()
				routeTraffic.normalUpBPS = float64(normalUp-routeTraffic.lastNormalUp) / secs
				routeTraffic.normalDownBPS = float64(normalDown-routeTraffic.lastNormalDown) / secs
				routeTraffic.aiUpBPS = float64(aiUp-routeTraffic.lastAIUp) / secs
				routeTraffic.aiDownBPS = float64(aiDown-routeTraffic.lastAIDown) / secs
			}
			routeTraffic.lastNormalUp, routeTraffic.lastNormalDown = normalUp, normalDown
			routeTraffic.lastAIUp, routeTraffic.lastAIDown = aiUp, aiDown
			routeTraffic.lastAt = now
			routeTraffic.updated = now
			routeTraffic.mu.Unlock()
		}
	}
}

type routeTrafficRates struct {
	UploadBPS   float64 `json:"upload_bps"`
	DownloadBPS float64 `json:"download_bps"`
}

func (a *App) serveRouteTraffic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	routeTraffic.mu.RLock()
	payload := struct {
		Normal    routeTrafficRates `json:"normal"`
		AI        routeTrafficRates `json:"ai"`
		UpdatedAt time.Time         `json:"updated_at"`
	}{routeTrafficRates{routeTraffic.normalUpBPS, routeTraffic.normalDownBPS},
		routeTrafficRates{routeTraffic.aiUpBPS, routeTraffic.aiDownBPS},
		routeTraffic.updated}
	routeTraffic.mu.RUnlock()
	_ = json.NewEncoder(w).Encode(payload)
}
