package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// decodeClashYAML parses a Clash profile into generic maps so unknown proxy
// fields survive round-tripping unchanged.
func decodeClashYAML(body []byte) (map[string]any, error) {
	var raw any
	if err := yaml.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse")
	}
	conf, ok := raw.(map[string]any)
	if !ok || conf == nil {
		return nil, fmt.Errorf("not-clash")
	}
	return conf, nil
}

func usableClashProxies(conf map[string]any) ([]any, error) {
	raw, ok := conf["proxies"].([]any)
	if !ok {
		return nil, fmt.Errorf("not-clash")
	}
	kept := make([]any, 0, len(raw))
	for _, item := range raw {
		p, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := p["name"].(string)
		server, _ := p["server"].(string)
		if name == "" || server == "" || p["port"] == nil {
			continue
		}
		typ := strings.ToLower(fmt.Sprint(p["type"]))
		switch typ {
		case "subscribe", "direct", "reject":
			continue
		}
		kept = append(kept, p)
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("empty")
	}
	return kept, nil
}

func clashRules(conf map[string]any) []string {
	raw, ok := conf["rules"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if rule, ok := item.(string); ok {
			out = append(out, rule)
		}
	}
	return out
}

// storeClashNodes keeps only usable proxies and string rules in the private
// node cache, matching the previous parser's contract.
func storeClashNodesYAML(body []byte, dst string) (proxies, rules int, err error) {
	conf, err := decodeClashYAML(body)
	if err != nil {
		return 0, 0, err
	}
	kept, err := usableClashProxies(conf)
	if err != nil {
		return 0, 0, err
	}
	ruleList := clashRules(conf)
	out, err := yaml.Marshal(map[string]any{"proxies": kept, "rules": ruleList})
	if err != nil {
		return 0, 0, fmt.Errorf("store")
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return 0, 0, err
	}
	if err := writePrivateFile(dst, string(out)); err != nil {
		return 0, 0, fmt.Errorf("store")
	}
	return len(kept), len(ruleList), nil
}

// validateClashPayload reports usable node and rule counts without writing.
func validateClashPayload(body []byte) (proxies, rules int, err error) {
	conf, err := decodeClashYAML(body)
	if err != nil {
		return 0, 0, err
	}
	kept, err := usableClashProxies(conf)
	if err != nil {
		return 0, 0, err
	}
	return len(kept), len(clashRules(conf)), nil
}
