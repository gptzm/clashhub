package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// This file is the pure-Go replacement for the former gen_config.py. It builds
// the mihomo configuration, validates it with the engine, and publishes the
// config plus the node-label map atomically.

var (
	genJPRE     = regexp.MustCompile(`(?i)(日本|JP|Japan|东京|大阪|名古屋|Tokyo|Osaka|Nagoya)`)
	genInfoRE   = regexp.MustCompile(`剩余流量|套餐到期|距离下次|重置|官网|入口|流量|Expires`)
	genGeoRE    = regexp.MustCompile(`(?i)^(GEOIP|GEOSITE|IP-ASN|RULE-SET),`)
	genEmojiRE  = regexp.MustCompile(`[\x{1F000}-\x{1FAFF}\x{2600}-\x{27BF}]`)
	genPromoRE  = regexp.MustCompile(`国内官网[：:].*$|固定入口[：:].*$`)
	genNonName  = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)
	genJPNameRE = regexp.MustCompile(`(?i)^JP[-_]*`)
	genDashRE   = regexp.MustCompile(`-+`)
	genSecretRE = regexp.MustCompile(`(?m)^secret:\s*(\S+)\s*$`)
)

type genSource struct {
	ID      string
	Label   string
	Type    string
	Path    string
	Include []string
	Exclude []string
}

type genSummary struct {
	Config  string          `json:"config"`
	JP      int             `json:"jp"`
	Other   int             `json:"other"`
	Groups  []string        `json:"groups"`
	Rules   int             `json:"rules"`
	Sources []genSourceInfo `json:"sources"`
}

type genSourceInfo struct {
	ID    string `json:"id"`
	Path  string `json:"path"`
	Mtime int64  `json:"mtime"`
}

func genSanitize(name string) string {
	name = genEmojiRE.ReplaceAllString(name, "")
	name = genPromoRE.ReplaceAllString(name, "")
	name = genNonName.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if len(name) > 32 {
		name = name[:32]
	}
	if name == "" {
		return "node"
	}
	return name
}

func genCountry(name string, keywords []string) string {
	matched := genJPRE.MatchString(name)
	if keywords != nil {
		lower := strings.ToLower(name)
		matched = false
		for _, k := range keywords {
			if strings.Contains(lower, strings.ToLower(k)) {
				matched = true
				break
			}
		}
	}
	if matched {
		return "JP"
	}
	for _, item := range []struct{ pat, cc string }{
		{`(?i)香港|HK|Hong`, "HK"}, {`(?i)台湾|TW|Taiwan`, "TW"},
		{`(?i)新加坡|SG|Singapore`, "SG"}, {`(?i)韩国|KR|Korea|首尔|Seoul`, "KR"},
		{`(?i)美国|USA|US|United`, "US"}, {`(?i)马来西亚|MY|Malaysia`, "MY"},
	} {
		if regexp.MustCompile(item.pat).MatchString(name) {
			return item.cc
		}
	}
	return "XX"
}

func genRealNode(p map[string]any) bool {
	name, _ := p["name"].(string)
	server, _ := p["server"].(string)
	if name == "" || server == "" || p["port"] == nil {
		return false
	}
	typ := strings.ToLower(fmt.Sprint(p["type"]))
	if typ == "subscribe" || typ == "direct" || typ == "reject" {
		return false
	}
	return !genInfoRE.MatchString(name)
}

func genMatchesSource(p map[string]any, include, exclude []string) bool {
	name := strings.ToLower(fmt.Sprint(p["name"]))
	if len(include) > 0 {
		ok := false
		for _, k := range include {
			if strings.Contains(name, strings.ToLower(k)) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	for _, k := range exclude {
		if strings.Contains(name, strings.ToLower(k)) {
			return false
		}
	}
	return true
}

func genLabelDigest(profile, name string) string {
	h := sha256.New()
	h.Write([]byte("srhop-label-v1\x00"))
	var size [8]byte
	n := uint64(len(profile))
	for i := 0; i < 8; i++ {
		size[7-i] = byte(n >> (8 * i))
	}
	h.Write(size[:])
	h.Write([]byte(profile))
	h.Write([]byte(name))
	return hex.EncodeToString(h.Sum(nil))
}

func genTake(conf map[string]any, prefix, profile string, keywords, include, exclude []string) (jp, other []any) {
	raw, _ := conf["proxies"].([]any)
	used := map[string]bool{}
	for i, item := range raw {
		p, ok := item.(map[string]any)
		if !ok || !genRealNode(p) || !genMatchesSource(p, include, exclude) {
			continue
		}
		cc := genCountry(fmt.Sprint(p["name"]), keywords)
		label := genSanitize(fmt.Sprint(p["name"]))
		label = genJPNameRE.ReplaceAllString(label, "")
		label = strings.Trim(genDashRE.ReplaceAllString(label, "-"), "-")
		base := fmt.Sprintf("%s-%s-%s", prefix, cc, label)
		if label == "" {
			base = fmt.Sprintf("%s-%s-%d", prefix, cc, i+1)
		}
		name := base
		for n := 2; used[name]; n++ {
			name = fmt.Sprintf("%s-%d", base, n)
		}
		used[name] = true
		if profile != "" {
			name += "-lbl-" + genLabelDigest(profile, fmt.Sprint(p["name"]))
		}
		clone := make(map[string]any, len(p)+1)
		for k, v := range p {
			clone[k] = v
		}
		clone["name"] = name
		if cc == "JP" {
			jp = append(jp, clone)
		} else {
			other = append(other, clone)
		}
	}
	return jp, other
}

func genLoad(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw any
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	conf, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("profile is not a mapping")
	}
	return conf, nil
}

func genExistingSecret() string {
	b, err := os.ReadFile(engineConfigPath())
	if err != nil {
		return ""
	}
	if m := genSecretRE.FindSubmatch(b); len(m) == 2 {
		return strings.Trim(string(m[1]), `"'`)
	}
	return ""
}

func genParseFallback(value string) (string, int, error) {
	host, portText, err := net.SplitHostPort(value)
	if err != nil {
		return "", 0, fmt.Errorf("fallback_addr must contain a host and port 1-65535")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 || strings.ContainsAny(host, " \t\r\n") || host == "" {
		return "", 0, fmt.Errorf("fallback_addr must contain a host and port 1-65535")
	}
	return host, port, nil
}

func genResolveSources(cfg Config, paths []string) (active []genSource, meta []map[string]string, err error) {
	entries := append([]SourceConfig(nil), cfg.Sources...)
	if len(entries) == 0 && len(cfg.SourceProfiles) > 0 {
		for _, p := range cfg.SourceProfiles {
			base := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
			id := genNonName.ReplaceAllString(strings.ToLower(base), "-")
			id = strings.Trim(id, "-")
			if id == "" {
				id = "profile"
			}
			entries = append(entries, SourceConfig{ID: id, Label: filepath.Base(p), Type: SourceTypeProfile, Path: p, Enabled: true})
		}
	}
	if paths != nil {
		allowed := map[string]bool{}
		for _, p := range paths {
			allowed[filepath.Clean(p)] = true
		}
		filtered := make([]SourceConfig, 0, len(entries)+len(paths))
		for _, e := range entries {
			if e.Type != SourceTypeProfile || allowed[filepath.Clean(e.Path)] {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}
	urls, _ := readFlClashProfileURLs(flclashDatabasePath())
	profilesDir := filepath.Join(userHome(), "Library", "Application Support", "com.follow.clash", "profiles")
	for _, e := range entries {
		if e.ID == sourceIDXLinker {
			continue
		}
		if e.ID == sourceIDOffice91 && !cfg.Office91Enabled {
			continue
		}
		if !e.Enabled {
			continue
		}
		meta = append(meta, map[string]string{"id": e.ID, "label": e.Label})
		if e.Type == SourceTypeVPN {
			continue
		}
		path := e.Path
		if e.Type == SourceTypeProfile {
			if path == "" {
				return nil, nil, fmt.Errorf("source %s: profile source requires a path", e.ID)
			}
			ident := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
			if filepath.Dir(filepath.Clean(path)) == profilesDir {
				if u, ok := urls[ident]; ok {
					digest := fmt.Sprintf("%x", sha256.Sum256([]byte(u)))
					cache := filepath.Join(sourcesCacheDir(), "flclash-"+ident+"-"+digest+".yaml")
					if _, err := os.Stat(cache); err == nil {
						path = cache
					}
				}
			}
		} else if e.Type == SourceTypeSubscription {
			path = filepath.Join(sourcesCacheDir(), e.ID+".yaml")
		} else {
			return nil, nil, fmt.Errorf("source %s: unknown type %q", e.ID, e.Type)
		}
		label := e.Label
		if label == "" {
			label = e.ID
		}
		active = append(active, genSource{ID: e.ID, Label: label, Type: e.Type, Path: path, Include: e.IncludeKeywords, Exclude: e.ExcludeKeywords})
	}
	return active, meta, nil
}

type genGroup struct {
	Name           string   `yaml:"name"`
	Type           string   `yaml:"type"`
	Proxies        []string `yaml:"proxies"`
	URL            string   `yaml:"url,omitempty"`
	ExpectedStatus int      `yaml:"expected-status,omitempty"`
	Interval       int      `yaml:"interval,omitempty"`
	Tolerance      int      `yaml:"tolerance,omitempty"`
	Timeout        int      `yaml:"timeout,omitempty"`
	Lazy           *bool    `yaml:"lazy,omitempty"`
}

func genURLTest(name string, members []any, url string, status, interval int) genGroup {
	names := make([]string, 0, len(members))
	for _, m := range members {
		if p, ok := m.(map[string]any); ok {
			names = append(names, fmt.Sprint(p["name"]))
		}
	}
	lazy := false
	return genGroup{Name: name, Type: "url-test", Proxies: names, URL: url, ExpectedStatus: status, Interval: interval, Tolerance: 50, Timeout: 5000, Lazy: &lazy}
}

type genConfigFile struct {
	MixedPort          int            `yaml:"mixed-port"`
	AllowLAN           bool           `yaml:"allow-lan"`
	BindAddress        string         `yaml:"bind-address"`
	Mode               string         `yaml:"mode"`
	LogLevel           string         `yaml:"log-level"`
	IPv6               bool           `yaml:"ipv6"`
	UnifiedDelay       bool           `yaml:"unified-delay"`
	TCPConcurrent      bool           `yaml:"tcp-concurrent"`
	ExternalController string         `yaml:"external-controller"`
	Secret             string         `yaml:"secret"`
	Profile            map[string]any `yaml:"profile"`
	Tun                map[string]any `yaml:"tun"`
	DNS                map[string]any `yaml:"dns"`
	Proxies            []any          `yaml:"proxies"`
	ProxyGroups        []genGroup     `yaml:"proxy-groups"`
	Rules              []string       `yaml:"rules"`
}

func genLabelsJSON(configHash string, meta []map[string]string, labels map[string]map[string]string) ([]byte, error) {
	sources := map[string]string{}
	for _, m := range meta {
		sources[m["id"]] = m["label"]
	}
	return json.Marshal(map[string]any{
		"config_sha256": configHash,
		"_generation":   configHash[:16],
		"_sources":      sources,
		"nodes":         labels,
	})
}

func buildEngineConfig(ctx context.Context, cfg Config, paths []string) (*genSummary, error) {
	keywords := cfg.AggregateKeywords
	if len(keywords) == 0 {
		keywords = nil
	}
	for i := range keywords {
		keywords[i] = strings.TrimSpace(keywords[i])
	}
	primary := cfg.PrimaryAddr
	if primary == "" {
		primary = "127.0.0.1:33301"
	}
	controller := cfg.Controller
	if controller == "" {
		controller = "127.0.0.1:19090"
	}
	probe := cfg.ProbeURL
	if probe == "" {
		probe = "https://www.gstatic.com/generate_204"
	}
	aiProbe := cfg.AIProbeURL
	if aiProbe == "" {
		aiProbe = "https://api.openai.com/v1/models"
	}
	enginePort := 33301
	if _, p, err := net.SplitHostPort(primary); err == nil {
		if v, err := strconv.Atoi(p); err == nil {
			enginePort = v
		}
	}
	fbHost, fbPort, err := genParseFallback(cfg.FallbackAddr)
	if err != nil {
		return nil, err
	}
	active, meta, err := genResolveSources(cfg, paths)
	if err != nil {
		return nil, err
	}
	baseID := cfg.BaseRulesSource
	if baseID == "" && len(active) > 0 {
		baseID = active[0].ID
	}
	type loadedSource struct {
		src  genSource
		conf map[string]any
	}
	var loaded []loadedSource
	for _, s := range active {
		conf, err := genLoad(s.Path)
		if err != nil {
			if s.Type == SourceTypeSubscription {
				continue
			}
			return nil, fmt.Errorf("source %s could not be read", s.ID)
		}
		loaded = append(loaded, loadedSource{s, conf})
	}
	var baseConf map[string]any
	for _, l := range loaded {
		if l.src.ID == baseID {
			baseConf = l.conf
			break
		}
	}
	if baseConf == nil {
		baseConf = map[string]any{}
		if baseID != "" {
			isSubscription := false
			for _, s := range active {
				if s.ID == baseID && s.Type == SourceTypeSubscription {
					isSubscription = true
				}
			}
			if !isSubscription {
				return nil, fmt.Errorf("base_rules_source %q is missing, disabled, or unreadable", baseID)
			}
		}
	}
	labels := map[string]map[string]string{}
	type sourceNodes struct {
		src genSource
		jp  []any
	}
	var sourcesJP []sourceNodes
	var other []any
	for _, l := range loaded {
		jp, rest := genTake(l.conf, l.src.ID, l.src.Label, keywords, l.src.Include, l.src.Exclude)
		all := append(append([]any{}, jp...), rest...)
		sourcesJP = append(sourcesJP, sourceNodes{l.src, all})
		other = append(other, rest...)
		raw, _ := l.conf["proxies"].([]any)
		var originals []map[string]any
		for _, item := range raw {
			p, ok := item.(map[string]any)
			if !ok || !genRealNode(p) || !genMatchesSource(p, l.src.Include, l.src.Exclude) {
				continue
			}
			originals = append(originals, p)
		}
		var originalJP, originalOther []map[string]any
		for _, p := range originals {
			if genCountry(fmt.Sprint(p["name"]), keywords) == "JP" {
				originalJP = append(originalJP, p)
			} else {
				originalOther = append(originalOther, p)
			}
		}
		generated := append(append([]any{}, jp...), rest...)
		originalOrder := append(append([]map[string]any{}, originalJP...), originalOther...)
		for i := 0; i < len(generated) && i < len(originalOrder); i++ {
			name := fmt.Sprint(generated[i].(map[string]any)["name"])
			labels[name] = map[string]string{"profile": l.src.Label, "name": fmt.Sprint(originalOrder[i]["name"]), "source_id": l.src.ID}
		}
	}
	var allJP []any
	for _, s := range sourcesJP {
		allJP = append(allJP, s.jp...)
	}
	proxies := append([]any{}, allJP...)
	proxies = append(proxies, map[string]any{"name": "FALLBACK-LOCAL", "type": "http", "server": fbHost, "port": fbPort})

	groups := []genGroup{}
	var chain, aiChain []string
	for _, s := range sourcesJP {
		if len(s.jp) == 0 {
			continue
		}
		groups = append(groups, genURLTest("JP-"+s.src.ID, s.jp, probe, 204, 60))
		chain = append(chain, "JP-"+s.src.ID)
	}
	if len(allJP) > 0 {
		groups = append(groups, genURLTest("JP-ALL", allJP, probe, 204, 60))
	}
	if len(other) > 0 {
		groups = append(groups, genURLTest("BACKUP-AUTO", other, probe, 204, 120))
	}
	for _, s := range sourcesJP {
		if len(s.jp) == 0 {
			continue
		}
		groups = append(groups, genURLTest("AI-JP-"+s.src.ID, s.jp, aiProbe, 401, 60))
		aiChain = append(aiChain, "AI-JP-"+s.src.ID)
	}
	if len(allJP) > 0 {
		groups = append(groups, genURLTest("AI-JP-ALL", allJP, aiProbe, 401, 60))
	}
	lazy := false
	if len(chain) > 0 {
		groups = append(groups, genGroup{Name: "BOOT-JP-ALL", Type: "fallback", Proxies: chain, URL: probe, ExpectedStatus: 204, Interval: 20, Timeout: 5000, Lazy: &lazy})
	}
	if len(aiChain) > 0 {
		groups = append(groups, genGroup{Name: "BOOT-AI-JP-ALL", Type: "fallback", Proxies: aiChain, URL: aiProbe, ExpectedStatus: 401, Interval: 20, Timeout: 5000, Lazy: &lazy})
	}
	allNames := make([]string, 0, len(allJP))
	for _, p := range allJP {
		allNames = append(allNames, fmt.Sprint(p.(map[string]any)["name"]))
	}
	primaryMembers := append([]string{}, "FALLBACK-LOCAL")
	if len(chain) > 0 {
		primaryMembers = append([]string{"BOOT-JP-ALL"}, primaryMembers...)
	}
	primaryMembers = append(primaryMembers, allNames...)
	aiMembers := append([]string{}, "FALLBACK-LOCAL")
	if len(aiChain) > 0 {
		aiMembers = append([]string{"BOOT-AI-JP-ALL"}, aiMembers...)
	}
	aiMembers = append(aiMembers, allNames...)
	groups = append(groups, genGroup{Name: "主代理", Type: "select", Proxies: primaryMembers})
	groups = append(groups, genGroup{Name: "AI主代理", Type: "select", Proxies: aiMembers})

	baseRules, _ := baseConf["rules"].([]any)
	rules := []string{}
	for _, item := range baseRules {
		s := strings.TrimSpace(fmt.Sprint(item))
		if strings.HasPrefix(strings.ToUpper(s), "MATCH,") {
			continue
		}
		s = strings.ReplaceAll(s, ",大机场 Big Airport", ",主代理")
		s = strings.ReplaceAll(s, ",自动选择", ",主代理")
		s = strings.ReplaceAll(s, ",故障转移", ",主代理")
		rules = append(rules, s)
	}
	aiRules := []string{}
	for _, d := range cfg.aiDomains() {
		aiRules = append(aiRules, "DOMAIN-SUFFIX,"+d+",AI主代理")
	}
	rules = append(append(aiRules, rules...), "MATCH,主代理")

	secret := genExistingSecret()
	if secret == "" {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		secret = hex.EncodeToString(b[:])
	}
	cfgFile := genConfigFile{
		MixedPort: enginePort, AllowLAN: false, BindAddress: "127.0.0.1", Mode: "rule",
		LogLevel: "info", IPv6: false, UnifiedDelay: true, TCPConcurrent: true,
		ExternalController: controller, Secret: secret,
		Profile: map[string]any{"store-selected": true, "store-fake-ip": false},
		Tun:     map[string]any{"enable": false},
		DNS: map[string]any{"enable": true, "ipv6": false, "enhanced-mode": "redir-host",
			"nameserver": []string{"223.5.5.5", "119.29.29.29"}},
		Proxies: proxies, ProxyGroups: groups, Rules: rules,
	}
	if _, err := os.Stat(filepath.Join(engineDataDir(), "geoip.metadb")); err != nil {
		filtered := cfgFile.Rules[:0]
		for _, r := range cfgFile.Rules {
			if !genGeoRE.MatchString(r) {
				filtered = append(filtered, r)
			}
		}
		cfgFile.Rules = filtered
	}
	out, err := yaml.Marshal(&cfgFile)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(engineDataDir(), 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(engineDataDir(), ".config-*.yaml")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	bin := engineBinary(cfg)
	cmd := exec.CommandContext(ctx, bin, "-d", engineDataDir(), "-t", "-f", tmpName)
	if output, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		_ = output
		return nil, fmt.Errorf("mihomo rejected the generated config")
	}
	sum := sha256.Sum256(out)
	hash := hex.EncodeToString(sum[:])
	labelBytes, err := genLabelsJSON(hash, meta, labels)
	if err != nil {
		return nil, err
	}
	labelTmp, err := os.CreateTemp(appSupportDir(), ".node-labels-*.json")
	if err != nil {
		return nil, err
	}
	labelTmpName := labelTmp.Name()
	defer os.Remove(labelTmpName)
	if _, err := labelTmp.Write(labelBytes); err != nil {
		labelTmp.Close()
		return nil, err
	}
	if err := labelTmp.Close(); err != nil {
		return nil, err
	}
	old, _ := os.ReadFile(engineConfigPath())
	if err := os.Rename(tmpName, engineConfigPath()); err != nil {
		return nil, err
	}
	if err := os.Rename(labelTmpName, filepath.Join(appSupportDir(), "node-labels.json")); err != nil {
		if old == nil {
			_ = os.Remove(engineConfigPath())
		} else {
			_ = writePrivateFile(engineConfigPath(), string(old))
		}
		return nil, err
	}
	summary := &genSummary{Config: engineConfigPath(), JP: len(allJP), Other: len(other), Rules: len(rules)}
	for _, g := range groups {
		summary.Groups = append(summary.Groups, g.Name)
	}
	for _, l := range loaded {
		info, _ := os.Stat(l.src.Path)
		mtime := int64(0)
		if info != nil {
			mtime = info.ModTime().Unix()
		}
		summary.Sources = append(summary.Sources, genSourceInfo{ID: l.src.ID, Path: l.src.Path, Mtime: mtime})
	}
	_ = time.Now()
	return summary, nil
}
