package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/nananek/quagent/internal/config"
)

func TestCodexConfigTOML(t *testing.T) {
	got := codexConfigTOML("gpt-5-codex", "http://host:1234/mcp")
	for _, want := range []string{
		`model = "gpt-5-codex"`,
		"[mcp_servers.quagent]",
		`url = "http://host:1234/mcp"`,
		`bearer_token_env_var = "QUAGENT_TOKEN"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("codexConfigTOML に %q が無い:\n%s", want, got)
		}
	}

	// model 未指定なら model 行を出さない
	noModel := codexConfigTOML("", "http://host/mcp")
	if strings.Contains(noModel, "model =") {
		t.Errorf("model 未指定なのに model 行がある:\n%s", noModel)
	}
}

func TestRefreshSourcesFromConfig(t *testing.T) {
	cfg := &config.Config{}
	cfg.Agy.RefreshTokenEnv = "AGY_ENV"
	cfg.Agy.RefreshTokenFile = "/tmp/agy.json"
	cfg.Agy.RefreshTokenCommand = []string{"agy", "print-token"}
	cfg.Codex.RefreshTokenEnv = "CODEX_ENV"
	cfg.Codex.RefreshTokenFile = "/tmp/codex.json"
	cfg.Codex.RefreshTokenCommand = []string{"codex", "print-token"}

	agy := agyRefreshSource(cfg)
	if agy.Env != "AGY_ENV" || agy.File != "/tmp/agy.json" || len(agy.Command) != 2 {
		t.Errorf("agyRefreshSource = %+v", agy)
	}
	codex := codexRefreshSource(cfg)
	if codex.Env != "CODEX_ENV" || codex.File != "/tmp/codex.json" || len(codex.Command) != 2 {
		t.Errorf("codexRefreshSource = %+v", codex)
	}
}

func TestPkgCacheWriteFiles(t *testing.T) {
	user := "vuser"
	files, cmd := pkgCacheWriteFiles(user)
	// ゲスト側のホームは環境依存の値なので、リテラルを避けて組み立てる。
	home := filepath.Join("/home", user)

	for _, want := range []string{
		"/etc/profile.d/quagent-pkgcache.sh",
		`export GOPROXY="http://127.0.0.1:5000/go,direct"`,
		`export PIP_INDEX_URL="http://127.0.0.1:5000/pypi/simple/"`,
		`export NPM_CONFIG_REGISTRY="http://127.0.0.1:5000/npm/"`,
		filepath.Join(home, ".cargo", "config.toml"),
		"sparse+http://127.0.0.1:5000/cargo/index/",
		filepath.Join(home, ".npmrc"),
		filepath.Join(home, ".config", "pip", "pip.conf"),
	} {
		if !strings.Contains(files, want) {
			t.Errorf("pkgCacheWriteFiles の files に %q が無い:\n%s", want, files)
		}
	}
	if !strings.Contains(cmd, filepath.Join(home, ".cargo")) || !strings.Contains(cmd, "mkdir -p") {
		t.Errorf("pkgCacheWriteFiles の cmd が不正: %q", cmd)
	}
}

func TestToolServerToolsEmptyConfig(t *testing.T) {
	cfg := &config.Config{}
	var logs []string
	regs := toolServerTools(cfg, func(s string) { logs = append(logs, s) })
	if len(regs) != 0 {
		t.Fatalf("ツールサーバー未設定で %d 個の登録が返った", len(regs))
	}
	if len(logs) != 0 {
		t.Fatalf("ログが出た: %v", logs)
	}
}
