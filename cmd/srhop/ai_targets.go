package main

import "strings"

// Supported AI route targets. Each maps to official endpoints that answer
// deterministically without credentials, so a probe separates "route down"
// from "endpoint alive".
var aiTargetOrder = []string{"openai", "claude", "gemini", "grok"}

func validAITarget(v string) bool {
	for _, t := range aiTargetOrder {
		if t == v {
			return true
		}
	}
	return false
}

// normalizeAITargets filters to supported values, preserves the user's order,
// de-duplicates, and falls back to openai when nothing valid remains.
func normalizeAITargets(list []string) []string {
	out := make([]string, 0, len(list))
	seen := map[string]bool{}
	for _, v := range list {
		v = strings.ToLower(strings.TrimSpace(v))
		if !validAITarget(v) || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) == 0 {
		return []string{"openai"}
	}
	return out
}

// aiProbeAttempt is one concrete mihomo /delay request.
type aiProbeAttempt struct {
	URL      string
	Expected int
}

// aiTargetDef describes how to prove one AI provider reachable through a node.
// Checks is a list of required probes; every check must pass for the target to
// count as healthy. Each check lists acceptable codes tried in order (first
// success satisfies that check): Anthropic answers 401 from supported regions
// and 403 for flagged exits, so the claude endpoint accepts both.
type aiTargetDef struct {
	Domains []string
	Checks  [][]aiProbeAttempt
}

func aiTargetDefinitions() map[string]aiTargetDef {
	return map[string]aiTargetDef{
		"openai": {Domains: []string{"openai.com", "chatgpt.com", "oaistatic.com", "oaiusercontent.com", "openaiapi-site.azureedge.net"},
			Checks: [][]aiProbeAttempt{
				{{"https://api.openai.com/v1/models", 401}},
				{{"https://chatgpt.com/robots.txt", 200}},
			}},
		"claude": {Domains: []string{"anthropic.com", "claude.ai", "claude.com"},
			Checks: [][]aiProbeAttempt{
				{{"https://api.anthropic.com/v1/models", 401}, {"https://api.anthropic.com/v1/models", 403}},
			}},
		"gemini": {Domains: []string{"generativelanguage.googleapis.com", "gemini.google.com", "aistudio.google.com", "ai.google.dev"},
			Checks: [][]aiProbeAttempt{
				{{"https://generativelanguage.googleapis.com/v1beta/models", 403}},
			}},
		"grok": {Domains: []string{"x.ai", "grok.com"},
			Checks: [][]aiProbeAttempt{
				// / answers 200/421/502 variably through exits; /v1/models is a
				// stable 401 without credentials.
				{{"https://api.x.ai/v1/models", 401}},
			}},
	}
}

// aiTargets returns the configured selection (normalized).
func (cfg Config) aiTargets() []string {
	return normalizeAITargets(cfg.AITargets)
}

// aiProbeSet lists, per selected target, the required checks to run.
func (cfg Config) aiProbeSet() map[string][][]aiProbeAttempt {
	defs := aiTargetDefinitions()
	out := make(map[string][][]aiProbeAttempt, len(cfg.AITargets))
	for _, t := range cfg.aiTargets() {
		if def, ok := defs[t]; ok {
			out[t] = def.Checks
		}
	}
	return out
}

// aiDomains is the union of routing suffixes across selected targets.
func (cfg Config) aiDomains() []string {
	defs := aiTargetDefinitions()
	var out []string
	seen := map[string]bool{}
	for _, t := range cfg.aiTargets() {
		for _, d := range defs[t].Domains {
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	return out
}

// aiRouteTitle renders the selection for the UI, e.g. "openai、claude".
func aiRouteTitle(targets []string) string {
	t := normalizeAITargets(targets)
	return strings.Join(t, "、")
}

// aiProbeSetFor builds the check map for an explicit target list; used by
// probers that run without an App (tests, standalone scanners).
func aiProbeSetFor(targets []string) map[string][][]aiProbeAttempt {
	defs := aiTargetDefinitions()
	out := make(map[string][][]aiProbeAttempt, len(targets))
	for _, t := range normalizeAITargets(targets) {
		if def, ok := defs[t]; ok {
			out[t] = def.Checks
		}
	}
	return out
}
