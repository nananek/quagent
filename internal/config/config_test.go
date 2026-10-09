package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPath(t *testing.T) {
	p := Path()
	if !strings.HasSuffix(p, filepath.Join("quagent", "config.json")) {
		t.Errorf("Path() = %q, expected to end with quagent/config.json", p)
	}
}

func TestLoad(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	// 1. ファイルが存在しない場合は空の Config を返す
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load non-existent file: unexpected error: %v", err)
	}
	if cfg == nil || len(cfg.Providers) != 0 {
		t.Fatalf("expected empty Config, got %+v", cfg)
	}

	// 2. 正常な JSON
	cfgFile := filepath.Join(tmp, "quagent", "config.json")
	if err := os.MkdirAll(filepath.Dir(cfgFile), 0o755); err != nil {
		t.Fatal(err)
	}
	validJSON := `{
		"providers": {
			"anthropic": {
				"upstream": "https://api.anthropic.com",
				"secret_env": "ANTHROPIC_API_KEY"
			}
		},
		"opencode": {
			"model": "anthropic/claude-3-5-sonnet"
		},
		"claude": {
			"model": "claude-3-5-sonnet",
			"subscription": "pro"
		},
		"agy": {
			"model": "gemini-2.0-flash",
			"subscription": true
		},
		"clipboard": {
			"method": "tmux"
		},
		"qemu_sandbox": {
			"enabled": false
		}
	}`
	if err := os.WriteFile(cfgFile, []byte(validJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load valid file failed: %v", err)
	}
	if p, ok := cfg.Providers["anthropic"]; !ok || p.SecretEnv != "ANTHROPIC_API_KEY" {
		t.Errorf("expected provider anthropic, got %+v", cfg.Providers)
	}
	if cfg.Claude.Subscription != "pro" {
		t.Errorf("expected subscription pro, got %q", cfg.Claude.Subscription)
	}
	if !cfg.Agy.Subscription {
		t.Errorf("expected agy subscription true")
	}
	if cfg.QemuSandbox == nil || cfg.QemuSandbox.Enabled == nil || *cfg.QemuSandbox.Enabled != false {
		t.Errorf("expected qemu_sandbox.enabled false, got %+v", cfg.QemuSandbox)
	}

	// 3. 不正な JSON
	if err := os.WriteFile(cfgFile, []byte("{invalid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Load()
	if err == nil {
		t.Fatal("expected error on invalid json, got nil")
	}
}

func TestProviderHeaderNameAndPrefix(t *testing.T) {
	// デフォルト: HeaderName="Authorization", HeaderPrefix="Bearer "
	p1 := Provider{}
	if got := p1.HeaderName(); got != "Authorization" {
		t.Errorf("HeaderName() = %q, want Authorization", got)
	}
	if got := p1.HeaderPrefix(); got != "Bearer " {
		t.Errorf("HeaderPrefix() = %q, want 'Bearer '", got)
	}

	// 小文字 authorization
	p2 := Provider{Header: "authorization"}
	if got := p2.HeaderPrefix(); got != "Bearer " {
		t.Errorf("HeaderPrefix() for 'authorization' = %q, want 'Bearer '", got)
	}

	// 独自ヘッダ名
	p3 := Provider{Header: "X-Api-Key"}
	if got := p3.HeaderName(); got != "X-Api-Key" {
		t.Errorf("HeaderName() = %q, want X-Api-Key", got)
	}
	if got := p3.HeaderPrefix(); got != "" {
		t.Errorf("HeaderPrefix() = %q, want ''", got)
	}

	// 明示的な Prefix 指定
	customPrefix := "Token "
	p4 := Provider{Header: "Authorization", Prefix: &customPrefix}
	if got := p4.HeaderPrefix(); got != "Token " {
		t.Errorf("HeaderPrefix() = %q, want 'Token '", got)
	}

	emptyPrefix := ""
	p5 := Provider{Header: "Authorization", Prefix: &emptyPrefix}
	if got := p5.HeaderPrefix(); got != "" {
		t.Errorf("HeaderPrefix() = %q, want ''", got)
	}
}

func TestProviderSecret(t *testing.T) {
	// 1. 何も指定しない場合
	pEmpty := Provider{}
	if _, err := pEmpty.Secret(); err == nil {
		t.Error("expected error for empty secret config")
	}

	// 2. SecretEnv
	t.Setenv("TEST_QUAGENT_SECRET", "my-env-secret")
	pEnv := Provider{SecretEnv: "TEST_QUAGENT_SECRET"}
	sec, err := pEnv.Secret()
	if err != nil || sec != "my-env-secret" {
		t.Errorf("Secret() = %q, %v; want my-env-secret", sec, err)
	}

	// SecretEnv 未設定
	pEnvMissing := Provider{SecretEnv: "TEST_QUAGENT_MISSING_NONEXISTENT"}
	if _, err := pEnvMissing.Secret(); err == nil {
		t.Error("expected error for missing env var")
	}

	// SecretEnv 空
	t.Setenv("TEST_QUAGENT_EMPTY", "   ")
	pEnvEmpty := Provider{SecretEnv: "TEST_QUAGENT_EMPTY"}
	if _, err := pEnvEmpty.Secret(); err == nil {
		t.Error("expected error for whitespace env var")
	}

	// 3. SecretFile
	tmp := t.TempDir()
	secFile := filepath.Join(tmp, "secret.txt")
	if err := os.WriteFile(secFile, []byte("  my-file-secret \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pFile := Provider{SecretFile: secFile}
	sec, err = pFile.Secret()
	if err != nil || sec != "my-file-secret" {
		t.Errorf("Secret() = %q, %v; want my-file-secret", sec, err)
	}

	// SecretFile 存在しない
	pMissingFile := Provider{SecretFile: filepath.Join(tmp, "no-such-file")}
	if _, err := pMissingFile.Secret(); err == nil {
		t.Error("expected error for non-existent secret file")
	}

	// SecretFile 空
	emptySecFile := filepath.Join(tmp, "empty.txt")
	if err := os.WriteFile(emptySecFile, []byte(" \n\t"), 0o600); err != nil {
		t.Fatal(err)
	}
	pEmptyFile := Provider{SecretFile: emptySecFile}
	if _, err := pEmptyFile.Secret(); err == nil {
		t.Error("expected error for empty secret file")
	}

	// 4. SecretCommand
	pCmd := Provider{SecretCommand: []string{"echo", "my-cmd-secret"}}
	sec, err = pCmd.Secret()
	if err != nil || sec != "my-cmd-secret" {
		t.Errorf("Secret() = %q, %v; want my-cmd-secret", sec, err)
	}

	// SecretCommand 失敗
	pCmdFail := Provider{SecretCommand: []string{"false"}}
	if _, err := pCmdFail.Secret(); err == nil {
		t.Error("expected error for failing command")
	}

	// SecretCommand 出力空
	pCmdEmpty := Provider{SecretCommand: []string{"echo", "   "}}
	if _, err := pCmdEmpty.Secret(); err == nil {
		t.Error("expected error for empty output command")
	}
}

func TestToolServer(t *testing.T) {
	// 認証なし
	tsNoAuth := ToolServer{URL: "http://localhost:8080"}
	if tsNoAuth.NeedsAuth() {
		t.Error("expected NeedsAuth() == false")
	}
	authVal, err := tsNoAuth.AuthValue()
	if err != nil || authVal != "" {
		t.Errorf("AuthValue() = %q, %v; want ''", authVal, err)
	}
	if tsNoAuth.HeaderName() != "Authorization" {
		t.Errorf("HeaderName() = %q, want Authorization", tsNoAuth.HeaderName())
	}

	// 認証あり (SecretEnv)
	t.Setenv("TEST_TS_SECRET", "ts-token")
	tsAuth := ToolServer{
		URL:       "http://localhost:8080",
		Header:    "Authorization",
		SecretEnv: "TEST_TS_SECRET",
	}
	if !tsAuth.NeedsAuth() {
		t.Error("expected NeedsAuth() == true")
	}
	authVal, err = tsAuth.AuthValue()
	if err != nil || authVal != "Bearer ts-token" {
		t.Errorf("AuthValue() = %q, %v; want 'Bearer ts-token'", authVal, err)
	}

	// 認証失敗
	tsAuthFail := ToolServer{
		URL:       "http://localhost:8080",
		SecretEnv: "TEST_TS_MISSING_VAR",
	}
	if _, err := tsAuthFail.AuthValue(); err == nil {
		t.Error("expected error from AuthValue() with missing env")
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	if got := expandHome("~/sub/file.txt"); got != filepath.Join(home, "sub/file.txt") {
		t.Errorf("expandHome(~/...) = %q, want %q", got, filepath.Join(home, "sub/file.txt"))
	}
	if got := expandHome("/absolute/path"); got != "/absolute/path" {
		t.Errorf("expandHome(/...) = %q, want /absolute/path", got)
	}
	if got := expandHome("relative/path"); got != "relative/path" {
		t.Errorf("expandHome(relative) = %q, want relative/path", got)
	}
}

func TestResourcePolicy(t *testing.T) {
	// 1. デフォルト値の検証
	def := DefaultResourcePolicy()
	if def.Enabled == nil || !*def.Enabled {
		t.Errorf("expected Enabled true, got %v", def.Enabled)
	}
	if def.MinFreeDiskGiB != 10 {
		t.Errorf("expected MinFreeDiskGiB 10, got %d", def.MinFreeDiskGiB)
	}
	if def.HostSafetyFreeGiB != 2 {
		t.Errorf("expected HostSafetyFreeGiB 2, got %d", def.HostSafetyFreeGiB)
	}
	if def.DiskWarnPercent != 80 {
		t.Errorf("expected DiskWarnPercent 80, got %d", def.DiskWarnPercent)
	}
	if def.DiskStopPercent != 95 {
		t.Errorf("expected DiskStopPercent 95, got %d", def.DiskStopPercent)
	}
	if def.Nice != 10 {
		t.Errorf("expected Nice 10, got %d", def.Nice)
	}
	if def.OOMScoreAdj != 500 {
		t.Errorf("expected OOMScoreAdj 500, got %d", def.OOMScoreAdj)
	}
	if def.MaxLogSizeMiB != 50 {
		t.Errorf("expected MaxLogSizeMiB 50, got %d", def.MaxLogSizeMiB)
	}

	// 2. nil Config での補完
	var nilCfg *Config
	resNil := nilCfg.ResourcePolicyOrDefault()
	if resNil.MinFreeDiskGiB != 10 {
		t.Errorf("expected 10 from nil config, got %d", resNil.MinFreeDiskGiB)
	}

	// 3. 一部指定時の補完
	f := false
	customCfg := &Config{
		Resources: &ResourcePolicy{
			Enabled:        &f,
			MinFreeDiskGiB: 20,
			Nice:           15,
		},
	}
	resCustom := customCfg.ResourcePolicyOrDefault()
	if resCustom.Enabled == nil || *resCustom.Enabled != false {
		t.Errorf("expected Enabled false, got %v", resCustom.Enabled)
	}
	if resCustom.MinFreeDiskGiB != 20 {
		t.Errorf("expected MinFreeDiskGiB 20, got %d", resCustom.MinFreeDiskGiB)
	}
	if resCustom.HostSafetyFreeGiB != 2 {
		t.Errorf("expected HostSafetyFreeGiB default 2, got %d", resCustom.HostSafetyFreeGiB)
	}
	if resCustom.Nice != 15 {
		t.Errorf("expected Nice 15, got %d", resCustom.Nice)
	}
	if resCustom.OOMScoreAdj != 500 {
		t.Errorf("expected OOMScoreAdj default 500, got %d", resCustom.OOMScoreAdj)
	}
}

func TestContentGuardPolicy(t *testing.T) {
	var nilPolicy *ContentGuardPolicy
	if !nilPolicy.IsEnabled() {
		t.Error("nil policy should be enabled by default")
	}

	emptyPolicy := &ContentGuardPolicy{}
	if !emptyPolicy.IsEnabled() {
		t.Error("empty policy should be enabled by default")
	}

	f := false
	disabledPolicy := &ContentGuardPolicy{Enabled: &f}
	if disabledPolicy.IsEnabled() {
		t.Error("disabled policy should not be enabled")
	}

	tr := true
	enabledPolicy := &ContentGuardPolicy{Enabled: &tr}
	if !enabledPolicy.IsEnabled() {
		t.Error("explicitly enabled policy should be enabled")
	}
}
