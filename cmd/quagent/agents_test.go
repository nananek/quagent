package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nananek/quagent/internal/config"
)

func TestClaudeTheme(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	cfg := &config.Config{}
	if got := claudeTheme(cfg); got != "" {
		t.Fatalf("host の設定が無いのに %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"theme": "light-daltonized", "model": "x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := claudeTheme(cfg); got != "light-daltonized" {
		t.Fatalf("host のテーマを使っていない: %q", got)
	}
	cfg.Claude.Theme = "dark"
	if got := claudeTheme(cfg); got != "dark" {
		t.Fatalf("config のテーマを優先していない: %q", got)
	}
}

func TestAgyColorScheme(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGY_CONFIG_DIR", dir)
	cfg := &config.Config{}
	if got := agyColorScheme(cfg); got != "" {
		t.Fatalf("host の設定が無いのに %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"colorScheme": "tokyo night", "trustedWorkspaces": ["/work"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := agyColorScheme(cfg); got != "tokyo night" {
		t.Fatalf("host のカラースキームを使っていない: %q", got)
	}
	cfg.Agy.ColorScheme = "solarized dark"
	if got := agyColorScheme(cfg); got != "solarized dark" {
		t.Fatalf("config のカラースキームを優先していない: %q", got)
	}
}

func TestAgentNames(t *testing.T) {
	names := agentNames()
	for _, want := range []string{"opencode", "claude", "agy", "codex"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("agentNames() に %q が含まれていない: %v", want, names)
		}
	}
}

func TestSetupAgyRequiresGeminiProvider(t *testing.T) {
	cfg := &config.Config{}
	err := setupAgy(vmGuest{}, cfg, []string{"anthropic", "openai"}, "test-token")
	if err == nil {
		t.Fatal("providers に gemini が無いのを通した")
	}
}

func TestSetupCodexRequiresOpenAIProvider(t *testing.T) {
	cfg := &config.Config{}
	err := setupCodex(vmGuest{}, cfg, []string{"anthropic", "gemini"}, "test-token")
	if err == nil {
		t.Fatal("providers に openai が無いのを通した")
	}
}

func TestGuestMCPURL(t *testing.T) {
	if got := guestMCPURL(); got != "http://quagent.host:7070/mcp" {
		t.Errorf("guestMCPURL() = %q, want 'http://quagent.host:7070/mcp'", got)
	}
}

func TestSetupClaudeErrors(t *testing.T) {
	// 1. anthropic provider が無い
	cfg := &config.Config{}
	if err := setupClaude(vmGuest{}, cfg, []string{"openai"}, "token"); err == nil {
		t.Fatal("expected error when anthropic provider is missing")
	}

	// 2. 不正な subscription プラン
	cfgSub := &config.Config{
		Claude: config.Claude{Subscription: "ultra-premium"},
	}
	if err := setupClaude(vmGuest{}, cfgSub, []string{"anthropic"}, "token"); err == nil {
		t.Fatal("expected error for invalid subscription plan")
	}

	// 3. subscription なのに header / prefix が既定以外
	cfgHeader := &config.Config{
		Claude: config.Claude{Subscription: "pro"},
		Providers: map[string]config.Provider{
			"anthropic": {Header: "X-Api-Key"},
		},
	}
	if err := setupClaude(vmGuest{}, cfgHeader, []string{"anthropic"}, "token"); err == nil {
		t.Fatal("expected error when custom header is used with subscription")
	}
}

func TestCappedBuffer(t *testing.T) {
	b := &cappedBuffer{max: 10}
	n, err := b.Write([]byte("12345"))
	if err != nil || n != 5 {
		t.Fatalf("first write: n=%d, err=%v", n, err)
	}
	if b.String() != "12345" {
		t.Errorf("content = %q, want '12345'", b.String())
	}

	// 上限 (10) を超える書き込み
	n, err = b.Write([]byte("67890EXTRA"))
	if err != nil || n != 10 { // n は渡されたスライスの長さ (10バイト)
		t.Fatalf("second write: n=%d, err=%v", n, err)
	}
	if b.String() != "1234567890" {
		t.Errorf("content = %q, want '1234567890'", b.String())
	}

	// 既に上限に達しているときの書き込み
	n, err = b.Write([]byte("MORE"))
	if err != nil || n != 4 {
		t.Fatalf("third write: n=%d, err=%v", n, err)
	}
	if b.String() != "1234567890" {
		t.Errorf("content = %q, want '1234567890'", b.String())
	}
}

func TestBuildOpencodeConfig(t *testing.T) {
	cfg := &config.Config{
		Opencode: config.Opencode{Model: "opencode-go/deepseek-v4.1-flash"},
	}
	token := "test-token"
	conf := buildOpencodeConfig(cfg, []string{"opencode", "opencode-go"}, token)

	if conf["model"] != "opencode-go/deepseek-v4.1-flash" {
		t.Errorf("model = %v, want opencode-go/deepseek-v4.1-flash", conf["model"])
	}

	// v2 providers
	v2, ok := conf["providers"].(map[string]any)
	if !ok {
		t.Fatalf("providers (v2) is missing or not a map: %v", conf["providers"])
	}
	for _, id := range []string{"opencode", "opencode-go"} {
		entry, ok := v2[id].(map[string]any)
		if !ok {
			t.Fatalf("providers[%q] missing", id)
		}
		settings, ok := entry["settings"].(map[string]any)
		if !ok {
			t.Fatalf("providers[%q].settings missing", id)
		}
		wantURL := "http://quagent.host:7070/llm/" + id
		if settings["baseURL"] != wantURL {
			t.Errorf("providers[%q].settings.baseURL = %v, want %v", id, settings["baseURL"], wantURL)
		}
		if settings["apiKey"] != token {
			t.Errorf("providers[%q].settings.apiKey = %v, want %v", id, settings["apiKey"], token)
		}
	}

	// v1 provider
	v1, ok := conf["provider"].(map[string]any)
	if !ok {
		t.Fatalf("provider (v1) is missing or not a map: %v", conf["provider"])
	}
	for _, id := range []string{"opencode", "opencode-go"} {
		entry, ok := v1[id].(map[string]any)
		if !ok {
			t.Fatalf("provider[%q] missing", id)
		}
		options, ok := entry["options"].(map[string]any)
		if !ok {
			t.Fatalf("provider[%q].options missing", id)
		}
		wantURL := "http://quagent.host:7070/llm/" + id
		if options["baseURL"] != wantURL {
			t.Errorf("provider[%q].options.baseURL = %v, want %v", id, options["baseURL"], wantURL)
		}
		if options["apiKey"] != token {
			t.Errorf("provider[%q].options.apiKey = %v, want %v", id, options["apiKey"], token)
		}
	}
}

