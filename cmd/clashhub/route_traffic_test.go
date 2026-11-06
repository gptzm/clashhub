package main

import "testing"

func TestAITargetClassificationFollowsConfiguredDomains(t *testing.T) {
	setAIRouteDomains([]string{"openai.com", "chatgpt.com", "anthropic.com", "claude.ai", "x.ai"})
	t.Cleanup(func() { setAIRouteDomains(aiTargetDefinitions()["openai"].Domains) })
	cases := map[string]bool{
		"api.openai.com:443":       true,
		"chatgpt.com:443":          true,
		"api.anthropic.com:443":    true,
		"claude.ai:443":            true,
		"api.x.ai:443":             true,
		"example.com:443":          false,
		"notopenai.com:443":        false,
		"openai.com.evil.test:443": false,
		"API.OPENAI.COM:443":       true,
	}
	for target, want := range cases {
		if got := isAITarget(target); got != want {
			t.Fatalf("%s: got %v want %v", target, got, want)
		}
	}
}
