package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

func userHome() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}

// Source types. Profile sources are read-only local Clash profiles.
// Subscription sources are fetched from the 玖壹 local subscription cache into
// srhop's private sources cache (see source_subscription.go). VPN sources are
// metadata only: they never produce node groups, they only appear in labels
// metadata so the runtime can attribute them.
const (
	SourceTypeProfile      = "profile"
	SourceTypeSubscription = "subscription"
	SourceTypeVPN          = "vpn"
)

// Stable identifiers for the standard source layout. These are part of the
// config contract shared with gen_config.py and the runtime: group names
// (JP-<id>), cache paths (sources/<id>.yaml) and labels metadata all key off
// them, so they must never change for an existing deployment.
const (
	sourceIDBigAirport = "big-airport"
	sourceIDKTMCloud   = "ktm-cloud"
	sourceIDOffice91   = "office91"
	sourceIDXLinker    = "xlinker"

	baseRulesSourceDefault          = sourceIDBigAirport
	subscriptionRefreshHoursDefault = 6
)

type SourceConfig struct {
	ID                string   `json:"id"`
	Label             string   `json:"label"`
	Type              string   `json:"type"`
	Path              string   `json:"path,omitempty"`
	Enabled           bool     `json:"enabled"`
	DefaultPriority   string   `json:"default_priority,omitempty"`
	DowngradePatterns []string `json:"downgrade_patterns"`
	IncludeKeywords   []string `json:"include_keywords,omitempty"`
	ExcludeKeywords   []string `json:"exclude_keywords,omitempty"`
	// URL optionally overrides the subscription address from FlClash's database.
	// It is stored in srhop's own config; FlClash is never modified.
	URL         string `json:"url,omitempty"`
	extraFields map[string]json.RawMessage
}

type Config struct {
	Office91Enabled   bool `json:"office91_enabled"`
	extraFields       map[string]json.RawMessage
	AggregateKeywords []string `json:"aggregate_keywords,omitempty"`
	HTTPListen        string   `json:"http_listen"`
	SOCKSListen       string   `json:"socks_listen"`
	PrimaryAddr       string   `json:"primary_addr"`
	FallbackAddr      string   `json:"fallback_addr"`
	// FallbackAddrs optionally lists extra external proxies tried in order.
	// FallbackAddr stays the first entry for older readers.
	FallbackAddrs            []string       `json:"fallback_addrs,omitempty"`
	Controller               string         `json:"controller"`
	EngineBin                string         `json:"engine_bin"`
	Sources                  []SourceConfig `json:"sources"`
	BaseRulesSource          string         `json:"base_rules_source"`
	SubscriptionRefreshHours int            `json:"subscription_refresh_hours"`
	// SourceProfiles is derived from Sources (enabled profile-type entries, in
	// priority order) on every load and save. It is kept in config.json so a
	// pre-sources binary can roll back without losing its source list; it is
	// never authoritative when "sources" is present.
	SourceProfiles []string `json:"source_profiles"`
	VPNService     string   `json:"vpn_service"`
	ProbeURL       string   `json:"probe_url"`
	AIProbeURL     string   `json:"ai_probe_url"`
	// AITargets selects which AI providers the AI route must serve and probe.
	AITargets        []string `json:"ai_targets,omitempty"`
	RequireJapan     bool     `json:"require_japan_egress"`
	VPNGuard         []string `json:"vpn_guard"`
	ProbeIntervalSec int      `json:"probe_interval_sec"`
	ProbeTimeoutSec  int      `json:"probe_timeout_sec"`
	PrimaryTimeoutMS int      `json:"primary_timeout_ms"`
	RecoveryInitSec  int      `json:"recovery_initial_sec"`
	RecoveryMaxSec   int      `json:"recovery_max_sec"`
	EgressCheckSec   int      `json:"egress_check_sec"`
	AutoStartSR      bool     `json:"auto_start_shadowrocket"`
	Notify           bool     `json:"notify"`
	// Per-route node selection strategy: "stability" keeps the active node
	// until it explicitly fails; "latency" re-selects whenever the active
	// node's delay exceeds the best healthy candidate by LatencyGapMS;
	// "balanced" uses the same rule with the looser BalanceGapMS threshold.
	PrimaryStrategy string `json:"primary_strategy"`
	AIStrategy      string `json:"ai_strategy"`
	LatencyGapMS    int    `json:"latency_gap_ms"`
	BalanceGapMS    int    `json:"balance_gap_ms"`
	// TunnelStallTimeoutSec kills an established tunnel that has seen no byte
	// in either direction for this long after data already flowed. Zero falls
	// back to the default.
	TunnelStallTimeoutSec int `json:"tunnel_stall_timeout_sec"`
	// TunnelFirstByteTimeoutSec kills a tunnel whose upstream never delivered
	// a byte and went silent (TLS handshake black hole after a node switch).
	TunnelFirstByteTimeoutSec int `json:"tunnel_first_byte_timeout_sec"`
}

// Unknown object fields survive load/save migrations. Known typed fields are
// always authoritative; extension values cannot override edited settings.
func configExtras(data []byte, typed any) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	t := reflect.TypeOf(typed)
	for i := 0; i < t.NumField(); i++ {
		tag := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		if tag != "" && tag != "-" {
			delete(fields, tag)
		}
	}
	if len(fields) == 0 {
		return nil, nil
	}
	return fields, nil
}
func marshalConfigExtras(typed any, extras map[string]json.RawMessage) ([]byte, error) {
	data, err := json.Marshal(typed)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	for k, v := range extras {
		if _, exists := fields[k]; !exists {
			fields[k] = v
		}
	}
	return json.Marshal(fields)
}
func (s *SourceConfig) UnmarshalJSON(data []byte) error {
	type plain SourceConfig
	if err := json.Unmarshal(data, (*plain)(s)); err != nil {
		return err
	}
	extras, err := configExtras(data, plain{})
	s.extraFields = extras
	return err
}
func (s SourceConfig) MarshalJSON() ([]byte, error) {
	type plain SourceConfig
	return marshalConfigExtras(plain(s), s.extraFields)
}
func (c *Config) UnmarshalJSON(data []byte) error {
	type plain Config
	if err := json.Unmarshal(data, (*plain)(c)); err != nil {
		return err
	}
	extras, err := configExtras(data, plain{})
	c.extraFields = extras
	return err
}
func (c Config) MarshalJSON() ([]byte, error) {
	type plain Config
	return marshalConfigExtras(plain(c), c.extraFields)
}

// normalizeStrategy maps accepted spellings to "stability"/"latency"; an
// unknown value returns "" so callers can fall back to their default.
func normalizeStrategy(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "stability", "stable":
		return "stability"
	case "latency", "latency-first", "speed":
		return "latency"
	case "balanced", "balance", "even":
		return "balanced"
	default:
		return ""
	}
}

func defaultConfig() Config {
	return Config{
		HTTPListen:                "127.0.0.1:33333",
		SOCKSListen:               "127.0.0.1:33334",
		PrimaryAddr:               "127.0.0.1:33301",
		FallbackAddr:              "127.0.0.1:33210",
		Controller:                "127.0.0.1:19090",
		EngineBin:                 "/opt/homebrew/bin/mihomo",
		Sources:                   []SourceConfig{},
		BaseRulesSource:           "",
		SubscriptionRefreshHours:  subscriptionRefreshHoursDefault,
		SourceProfiles:            []string{},
		VPNService:                "Shadowrocket",
		ProbeURL:                  "https://www.gstatic.com/generate_204",
		AIProbeURL:                "https://api.openai.com/v1/models",
		AITargets:                 []string{"openai"},
		RequireJapan:              true,
		VPNGuard:                  []string{"Shadowrocket", "LetsVPN"},
		ProbeIntervalSec:          10,
		ProbeTimeoutSec:           5,
		PrimaryTimeoutMS:          2500,
		RecoveryInitSec:           30,
		RecoveryMaxSec:            120,
		EgressCheckSec:            60,
		AutoStartSR:               false,
		Notify:                    true,
		PrimaryStrategy:           "stability",
		AIStrategy:                "latency",
		LatencyGapMS:              latencyGapMSDefault,
		TunnelStallTimeoutSec:     tunnelStallTimeoutSecDefault,
		TunnelFirstByteTimeoutSec: tunnelFirstByteTimeoutSecDefault,
	}
}

const (
	latencyGapMSDefault          = 150
	balanceGapMSDefault          = 500
	tunnelStallTimeoutSecDefault = 1500 // 25 min: above the observed slowest legit silent prefill
	// tunnelFirstByteTimeoutSecDefault: TLS handshake + origin ServerHello
	// should complete within seconds; silence beyond a minute is a dead path.
	tunnelFirstByteTimeoutSecDefault = 60
)

func appSupportDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, "Library", "Application Support", "srhop")
}

func configPath() string { return filepath.Join(appSupportDir(), "config.json") }
func statePath() string  { return filepath.Join(appSupportDir(), "state.json") }
func lockPath() string   { return filepath.Join(appSupportDir(), "srhop.lock") }

func logPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(appSupportDir(), "srhop.log")
	}
	return filepath.Join(home, "Library", "Logs", "srhop.log")
}

var sourceIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// derivedSourceProfiles returns the rollback-compatible profile path list:
// enabled profile-type sources only, in sources priority order.
func derivedSourceProfiles(sources []SourceConfig) []string {
	out := []string{}
	for _, s := range sources {
		if s.Enabled && s.Type == SourceTypeProfile && s.Path != "" {
			out = append(out, s.Path)
		}
	}
	return out
}

// validateSources enforces the stable-id contract: ids must be stable
// identifiers, unique (case-insensitively), types must be known, profile
// sources need a path, and base_rules_source must reference a usable source.
func validateSources(sources []SourceConfig, baseRules string) error {
	seen := map[string]bool{}
	for i, s := range sources {
		if s.DefaultPriority != "" && !validPriorityLevel(s.DefaultPriority) {
			return fmt.Errorf("source %s: invalid default_priority", s.ID)
		}
		if s.URL != "" && !validSubscriptionURL(s.URL) {
			return fmt.Errorf("source %s: invalid url", s.ID)
		}
		if !sourceIDPattern.MatchString(s.ID) {
			return fmt.Errorf("source %d: invalid id %q (must be [a-z0-9][a-z0-9-]{0,63})", i, s.ID)
		}
		if seen[s.ID] {
			return fmt.Errorf("duplicate source id %q", s.ID)
		}
		if seen[strings.ToLower(s.ID)] && s.ID != strings.ToLower(s.ID) {
			return fmt.Errorf("duplicate source id %q (case-insensitive match)", s.ID)
		}
		seen[s.ID] = true
		seen[strings.ToLower(s.ID)] = true
		switch s.Type {
		case SourceTypeProfile:
			if s.Path == "" {
				return fmt.Errorf("source %s: profile source requires a path", s.ID)
			}
		case SourceTypeSubscription, SourceTypeVPN:
		default:
			return fmt.Errorf("source %s: unknown type %q", s.ID, s.Type)
		}
		if strings.TrimSpace(s.Label) == "" {
			return fmt.Errorf("source %s: label must not be empty", s.ID)
		}
	}
	if baseRules != "" {
		var src *SourceConfig
		for i := range sources {
			if sources[i].ID == baseRules {
				src = &sources[i]
				break
			}
		}
		if src == nil {
			return fmt.Errorf("base_rules_source %q does not match any source id", baseRules)
		}
		if src.Type == SourceTypeVPN {
			return fmt.Errorf("base_rules_source %q is a vpn source and has no rules", baseRules)
		}
		if !src.Enabled {
			return fmt.Errorf("base_rules_source %q is disabled", baseRules)
		}
	}
	return nil
}

// defaultBaseRulesSource picks the rules base when the config does not name
// one: the standard big-airport source when present, else the first enabled
// profile source, else the first enabled subscription source.
func defaultBaseRulesSource(sources []SourceConfig) string {
	for _, s := range sources {
		if s.ID == baseRulesSourceDefault && s.Enabled && s.Type != SourceTypeVPN {
			return baseRulesSourceDefault
		}
	}
	for _, s := range sources {
		if s.Enabled && s.Type == SourceTypeProfile {
			return s.ID
		}
	}
	for _, s := range sources {
		if s.Enabled && s.Type == SourceTypeSubscription {
			return s.ID
		}
	}
	return ""
}

var legacyIDStrip = regexp.MustCompile(`[^a-z0-9-]+`)

// legacySourceID derives a stable id for a pre-sources profile path so that
// repeated migrations of the same config always produce the same id.
func legacySourceID(path string, taken map[string]bool) string {
	base := strings.ToLower(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	id := strings.Trim(legacyIDStrip.ReplaceAllString(base, "-"), "-")
	if id == "" || !sourceIDPattern.MatchString(id) {
		id = "profile"
	}
	for n := 2; taken[id]; n++ {
		id = fmt.Sprintf("%s-%d", id, n)
	}
	taken[id] = true
	return id
}

// migrateSourceProfiles converts the legacy source_profiles path list into the
// sources layout. Other paths get deterministic ids derived from their file
// name so repeated migrations stay stable. A fresh install has neither list and
// therefore starts with no sources.
func migrateSourceProfiles(paths []string) []SourceConfig {
	taken := map[string]bool{sourceIDOffice91: true, sourceIDXLinker: true}
	sources := make([]SourceConfig, 0, len(paths)+2)
	seenPath := map[string]bool{}
	for _, p := range paths {
		if p == "" || seenPath[p] {
			continue
		}
		seenPath[p] = true
		sources = append(sources, SourceConfig{
			ID:      legacySourceID(p, taken),
			Label:   filepath.Base(p),
			Type:    SourceTypeProfile,
			Path:    p,
			Enabled: true,
		})
	}
	return sources
}

// saveConfig writes config.json atomically. The derived source_profiles list
// is recomputed from Sources so the rollback copy can never drift, no matter
// which call site persists the config.
func saveConfig(cfg Config) error {
	if err := validateRoutingConfig(cfg); err != nil {
		return err
	}
	if err := os.MkdirAll(appSupportDir(), 0o755); err != nil {
		return err
	}
	cfg.SourceProfiles = derivedSourceProfiles(cfg.Sources)
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(appSupportDir(), ".config-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(append(out, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Rename(name, configPath())
}

func validateRoutingConfig(cfg Config) error {
	if cfg.AggregateKeywords != nil {
		if len(cfg.AggregateKeywords) == 0 {
			return fmt.Errorf("aggregate_keywords must not be empty")
		}
		for _, keyword := range cfg.AggregateKeywords {
			if strings.TrimSpace(keyword) == "" {
				return fmt.Errorf("aggregate_keywords contains an empty keyword")
			}
		}
	}
	if err := validateFallbackAddr(cfg.FallbackAddr); err != nil {
		return fmt.Errorf("fallback_addr: %w", err)
	}
	for _, addr := range cfg.FallbackAddrs {
		if err := validateFallbackAddr(addr); err != nil {
			return fmt.Errorf("fallback_addrs: %w", err)
		}
	}
	return nil
}

func validateFallbackAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" || strings.ContainsAny(host, " \t\r\n") {
		return fmt.Errorf("must be host:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("port must be 1-65535")
	}
	return nil
}

// normalizeFallbackAddrs de-duplicates the list and keeps FallbackAddr first.
func normalizeFallbackAddrs(cfg *Config) {
	list := make([]string, 0, len(cfg.FallbackAddrs)+1)
	seen := map[string]bool{}
	for _, addr := range append([]string{cfg.FallbackAddr}, cfg.FallbackAddrs...) {
		addr = strings.TrimSpace(addr)
		if addr == "" || seen[addr] {
			continue
		}
		seen[addr] = true
		list = append(list, addr)
	}
	if len(list) == 0 {
		list = []string{cfg.FallbackAddr}
	}
	cfg.FallbackAddr = list[0]
	cfg.FallbackAddrs = list
}

func loadConfig() (Config, error) {
	cfg := defaultConfig()
	if err := os.MkdirAll(appSupportDir(), 0o755); err != nil {
		return cfg, err
	}
	// Pick up FlClash profiles added while srhop was not running. Failures here
	// must not block startup: the previously configured sources stay valid.
	_, _ = reconcileDiscoveredSources(configPath())
	b, err := os.ReadFile(configPath())
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, saveConfig(cfg)
		}
		return cfg, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", configPath(), err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", configPath(), err)
	}
	normalizeFallbackAddrs(&cfg)
	cfg.AITargets = normalizeAITargets(cfg.AITargets)
	if err := validateRoutingConfig(cfg); err != nil {
		return cfg, err
	}
	persist := false
	if rawSources, hasSources := raw["sources"]; hasSources && string(rawSources) != "null" {
		// Decode into a fresh slice: unmarshalling into the default-valued
		// slice would merge element-wise and leak default paths into entries
		// that deliberately omit "path".
		var sources []SourceConfig
		if err := json.Unmarshal(rawSources, &sources); err != nil {
			return cfg, fmt.Errorf("parse %s: %w", configPath(), err)
		}
		cfg.Sources = sources
	} else {
		// Legacy config: migrate source_profiles into the sources layout and
		// write it back so both representations exist from then on.
		cfg.Sources = migrateSourceProfiles(cfg.SourceProfiles)
		persist = true
	}
	filtered := make([]SourceConfig, 0, len(cfg.Sources))
	for _, source := range cfg.Sources {
		if source.ID == sourceIDXLinker {
			persist = true
			continue
		}
		filtered = append(filtered, source)
	}
	cfg.Sources = filtered
	if _, hasBase := raw["base_rules_source"]; !hasBase {
		// Absent key: derive from the (possibly just migrated) sources instead
		// of trusting the default value for a differently configured file.
		cfg.BaseRulesSource = defaultBaseRulesSource(cfg.Sources)
		persist = true
	}
	if cfg.SubscriptionRefreshHours <= 0 {
		cfg.SubscriptionRefreshHours = subscriptionRefreshHoursDefault
		persist = true
	}
	if err := validateSources(cfg.Sources, cfg.BaseRulesSource); err != nil {
		return cfg, err
	}
	cfg.SourceProfiles = derivedSourceProfiles(cfg.Sources)
	// Keep the rollback copy honest: if the file's source_profiles is missing
	// or disagrees with the derived value, rewrite it once.
	var fileProfiles []string
	if err := json.Unmarshal(raw["source_profiles"], &fileProfiles); err != nil || !reflect.DeepEqual(fileProfiles, cfg.SourceProfiles) {
		persist = true
	}
	if cfg.ProbeIntervalSec <= 0 {
		cfg.ProbeIntervalSec = 5
	}
	if cfg.ProbeTimeoutSec <= 0 {
		cfg.ProbeTimeoutSec = 5
	}
	if cfg.PrimaryTimeoutMS <= 0 {
		cfg.PrimaryTimeoutMS = 2500
	}
	if cfg.PrimaryStrategy = normalizeStrategy(cfg.PrimaryStrategy); cfg.PrimaryStrategy == "" {
		cfg.PrimaryStrategy = "stability"
		persist = true
	}
	if cfg.AIStrategy = normalizeStrategy(cfg.AIStrategy); cfg.AIStrategy == "" {
		cfg.AIStrategy = "latency"
		persist = true
	}
	if cfg.LatencyGapMS <= 0 {
		cfg.LatencyGapMS = latencyGapMSDefault
		persist = true
	}
	if cfg.BalanceGapMS <= 0 {
		cfg.BalanceGapMS = balanceGapMSDefault
		persist = true
	}
	if cfg.TunnelStallTimeoutSec <= 0 {
		cfg.TunnelStallTimeoutSec = tunnelStallTimeoutSecDefault
	}
	if cfg.TunnelFirstByteTimeoutSec <= 0 {
		cfg.TunnelFirstByteTimeoutSec = tunnelFirstByteTimeoutSecDefault
	}
	// Missing keys keep defaultConfig() values after unmarshal, so normalize
	// alone cannot detect a pre-strategy config file. Persist once when any
	// new key is absent to make the migration visible.
	for _, key := range []string{"primary_strategy", "ai_strategy", "latency_gap_ms", "balance_gap_ms", "tunnel_stall_timeout_sec", "tunnel_first_byte_timeout_sec"} {
		if _, ok := raw[key]; !ok {
			persist = true
			break
		}
	}
	if cfg.RecoveryInitSec <= 0 {
		cfg.RecoveryInitSec = 60
	}
	if cfg.RecoveryMaxSec < cfg.RecoveryInitSec {
		cfg.RecoveryMaxSec = cfg.RecoveryInitSec
	}
	if cfg.EgressCheckSec <= 0 {
		cfg.EgressCheckSec = 300
	}
	if persist {
		// Best effort: the in-memory config is already migrated, and the next
		// successful save re-derives the rollback copy anyway.
		_ = saveConfig(cfg)
	}
	return cfg, nil
}
