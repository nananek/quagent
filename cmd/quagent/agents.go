package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/nananek/quagent/internal/authproxy"
	"github.com/nananek/quagent/internal/config"
	"github.com/nananek/quagent/internal/hostsvc"
	"github.com/nananek/quagent/internal/mcpsrv"
)

// agentSpec は VM 内で動かすエージェントの準備と起動のしかた。
// どのエージェントも「檻の中では確認なしで勝手に動く」設定で起動する。
type agentSpec struct {
	// setup は VM 内に設定を書く。providers は認証プロキシに登録した provider ID、
	// token は窓口の合言葉 (LLM プロキシと MCP の両方に要る)。
	setup func(g vmGuest, cfg *config.Config, providers []string, token string) error
	// entrypoint は VM の /entrypoint.sh に書く内容 (/work でエージェントを起動する)。
	// シェルから何度でも呼び戻せるよう、ここでは exec せず、終わったら戻る。
	entrypoint string
}

// DefaultAgent は特に指定がないときのエージェント。
const DefaultAgent = "opencode"

var agents = map[string]agentSpec{
	"opencode": {
		setup: setupOpencode,
		entrypoint: `#!/bin/bash
# quagent: エージェントを起動する (終了したあと ↑ で呼び戻せる)
cd /work
# 更新確認とモデル一覧の取得は外へ出られず DNS の拒否が並ぶだけなので止める
OPENCODE_DISABLE_AUTOUPDATE=1 OPENCODE_DISABLE_MODELS_FETCH=1 opencode --auto /work`,
	},
	"claude": {
		setup: setupClaude,
		entrypoint: `#!/bin/bash
# quagent: エージェントを起動する (終了したあと ↑ で呼び戻せる)
cd /work
claude --dangerously-skip-permissions`,
	},
	"agy": {
		setup: setupAgy,
		entrypoint: `#!/bin/bash
# quagent: エージェントを起動する (終了したあと ↑ で呼び戻せる)
cd /work
if [ -f ~/.gemini/antigravity-cli/env.sh ]; then
  . ~/.gemini/antigravity-cli/env.sh
fi
agy --dangerously-skip-permissions ${AGY_FLAGS:-}`,
	},
}

// paneCommand はエージェントのペインで実行するコマンド。エージェントが終わったら
// シェルに落とし、↑ で /entrypoint.sh を呼び戻せるようにする (非ログインの対話
// シェルなので ~/.bashrc が確実に読まれる)。
const paneCommand = "/entrypoint.sh; exec bash -i"

// historyMarker は ~/.bashrc への追記が済んでいるかの目印。
const historyMarker = "quagent: エージェントを終了したあと"

// historySnippet は作業ユーザーの ~/.bashrc に追記する。対話シェルが始まるとき、
// 履歴を読んだあとの最初のプロンプトで /entrypoint.sh を履歴の先頭に入れるので、
// エージェントを終了したあと ↑ を押すだけで呼び戻せる。追記は 1 回だけで、
// 元の PROMPT_COMMAND があれば戻す。
const historySnippet = `# quagent: エージェントを終了したあと、↑ で /entrypoint.sh を呼び戻せるようにする
if [ -n "$PS1" ] && [ -z "$QUAGENT_HISTORY_SEEDED" ]; then
  QUAGENT_HISTORY_SEEDED=1
  __quagent_prompt_command="$PROMPT_COMMAND"
  quagent_seed_history() {
    history -s /entrypoint.sh
    PROMPT_COMMAND="$__quagent_prompt_command"
    unset __quagent_prompt_command
    unset -f quagent_seed_history
  }
  PROMPT_COMMAND=quagent_seed_history
fi`

// setupHistory は ~/.bashrc に上の仕掛けを 1 回だけ追記する (何度呼んでも増えない)。
func setupHistory(g vmGuest) error {
	script := `grep -qF ` + shellQuote(historyMarker) + ` ~/.bashrc 2>/dev/null || cat >> ~/.bashrc <<'QUAGENT_HISTORY'
` + historySnippet + `
QUAGENT_HISTORY`
	if out, err := g.sh(script, nil); err != nil {
		return fmt.Errorf("~/.bashrc に履歴の仕掛けを書けない: %v: %s", err, out)
	}
	return nil
}

// agentNames はエージェント名の一覧を返す。
func agentNames() []string {
	var names []string
	for n := range agents {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func guestMCPURL() string { return hostsvc.GuestOrigin() + mcpsrv.Path }

// setupOpencode は opencode が provider を認証プロキシ経由で使い、quagent の MCP を
// 使うよう設定する。apiKey は窓口の合言葉 (本物の鍵はプロキシが host 側で付け替える)。
func setupOpencode(g vmGuest, cfg *config.Config, providers []string, token string) error {
	prov := map[string]any{}
	for _, id := range providers {
		prov[id] = map[string]any{"options": map[string]any{
			"baseURL": authproxy.GuestBaseURL(hostsvc.GuestOrigin(), id),
			"apiKey":  token,
		}}
	}
	conf := map[string]any{
		"$schema":    "https://opencode.ai/config.json",
		"autoupdate": false,
		"provider":   prov,
		"mcp": map[string]any{
			"quagent": map[string]any{"type": "remote", "url": guestMCPURL(), "enabled": true, "oauth": false,
				"headers": map[string]string{"Authorization": "Bearer " + token}},
		},
	}
	if cfg.Opencode.Model != "" {
		conf["model"] = cfg.Opencode.Model
	}
	return writeJSON(g, "~/.config/opencode/opencode.json", conf)
}

// claudeProvider は Claude Code に使わせる provider ID (Anthropic API)。
const claudeProvider = "anthropic"

// setupClaude は Claude Code が Anthropic API を認証プロキシ経由で使い、quagent の
// MCP を使うよう設定する。鍵の代わりに窓口の合言葉を、API キーなら apiKeyHelper で、
// サブスクリプションならトークン (CLAUDE_CODE_OAUTH_TOKEN) として渡す。初回の
// 案内・作業ディレクトリの信頼・権限確認の省略の確認は済ませておく。
func setupClaude(g vmGuest, cfg *config.Config, providers []string, token string) error {
	if !slices.Contains(providers, claudeProvider) {
		return fmt.Errorf("Claude Code を使うには config.json の providers に %q (Anthropic API) を設定する", claudeProvider)
	}
	env := map[string]string{
		"ANTHROPIC_BASE_URL":                       authproxy.GuestBaseURL(hostsvc.GuestOrigin(), claudeProvider),
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		"DISABLE_AUTOUPDATER":                      "1",
	}
	settings := map[string]any{"env": env}
	if plan := cfg.Claude.Subscription; plan != "" {
		if !slices.Contains([]string{"pro", "max", "team", "enterprise"}, plan) {
			return fmt.Errorf("claude.subscription は pro / max / team / enterprise のどれか: %q", plan)
		}
		// Claude Code は Authorization: Bearer <合言葉> で送り、プロキシが本物のトークンに付け替える
		p := cfg.Providers[claudeProvider]
		if !strings.EqualFold(p.HeaderName(), "Authorization") || p.HeaderPrefix() != "Bearer " {
			return fmt.Errorf("claude.subscription では providers の %q に header / prefix を指定しない (既定の Authorization: Bearer で付ける)", claudeProvider)
		}
		env["CLAUDE_CODE_OAUTH_TOKEN"] = token
		// トークンを環境変数で渡すとプランを問い合わせないので、表示用に教える
		env["CLAUDE_CODE_SUBSCRIPTION_TYPE"] = plan
	} else {
		settings["apiKeyHelper"] = "echo " + token
	}
	if cfg.Claude.Model != "" {
		settings["model"] = cfg.Claude.Model
	}
	if theme := claudeTheme(cfg); theme != "" {
		settings["theme"] = theme
	}
	if err := writeJSON(g, "~/.claude/settings.json", settings); err != nil {
		return err
	}
	state := map[string]any{
		"hasCompletedOnboarding":        true,
		"bypassPermissionsModeAccepted": true,
		"projects": map[string]any{
			"/work": map[string]any{"hasTrustDialogAccepted": true},
		},
		"mcpServers": map[string]any{
			"quagent": map[string]any{"type": "http", "url": guestMCPURL(),
				"headers": map[string]string{"Authorization": "Bearer " + token}},
		},
	}
	return writeJSON(g, "~/.claude.json", state)
}

// claudeTheme は VM 内の Claude Code のカラーテーマを返す。config の claude.theme が
// あればそれ、無ければ host の Claude Code の設定のもの (読めなければ空)。
func claudeTheme(cfg *config.Config) string {
	if cfg.Claude.Theme != "" {
		return cfg.Claude.Theme
	}
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".claude")
	}
	b, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		return ""
	}
	var host struct {
		Theme string `json:"theme"`
	}
	if json.Unmarshal(b, &host) != nil {
		return ""
	}
	return host.Theme
}

func writeJSON(g vmGuest, path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := g.writeFile(path, b); err != nil {
		return err
	}
	// 窓口の合言葉が入っているので、作業ユーザー以外には読ませない
	if out, err := g.sh("chmod 600 "+path, nil); err != nil {
		return fmt.Errorf("%s の権限を変えられない: %v: %s", path, err, out)
	}
	return nil
}

// agyProvider は agy に使わせる provider ID (Gemini API)。
const agyProvider = "gemini"

// setupAgy は agy (Antigravity CLI) が Gemini API を認証プロキシ経由で使い、
// quagent の MCP を使うよう設定する。
func setupAgy(g vmGuest, cfg *config.Config, providers []string, token string) error {
	if !slices.Contains(providers, agyProvider) {
		return fmt.Errorf("agy を使うには config.json の providers に %q (Gemini API) を設定する", agyProvider)
	}
	settings := map[string]any{
		"modelProvider":     "gemini",
		"trustedWorkspaces": []string{"/work"},
	}
	if err := writeJSON(g, "~/.gemini/antigravity-cli/settings.json", settings); err != nil {
		return err
	}
	mcpConf := map[string]any{
		"mcpServers": map[string]any{
			"quagent": map[string]any{
				"disabled":  false,
				"serverUrl": guestMCPURL(),
				"headers": map[string]string{
					"Authorization": "Bearer " + token,
				},
			},
		},
	}
	if err := writeJSON(g, "~/.gemini/config/mcp_config.json", mcpConf); err != nil {
		return err
	}
	_ = writeJSON(g, "~/.gemini/antigravity-cli/mcp_config.json", mcpConf)

	envLines := []string{
		fmt.Sprintf("export GEMINI_API_KEY=%s", shellQuote(token)),
		fmt.Sprintf("export GOOGLE_GEMINI_BASE_URL=%s", shellQuote(authproxy.GuestBaseURL(hostsvc.GuestOrigin(), agyProvider))),
	}
	if cfg.Agy.Model != "" {
		envLines = append(envLines, fmt.Sprintf("export AGY_FLAGS=%s", shellQuote("--model "+cfg.Agy.Model)))
	}
	envPath := "~/.gemini/antigravity-cli/env.sh"
	if err := g.writeFile(envPath, []byte(strings.Join(envLines, "\n")+"\n")); err != nil {
		return err
	}
	if out, err := g.sh("chmod 600 "+envPath, nil); err != nil {
		return fmt.Errorf("%s の権限を変えられない: %v: %s", envPath, err, out)
	}

	marker := "quagent: agy env"
	script := `grep -qF ` + shellQuote(marker) + ` ~/.bashrc 2>/dev/null || cat >> ~/.bashrc <<'AGY_ENV'
# ` + marker + `
if [ -f ~/.gemini/antigravity-cli/env.sh ]; then
  . ~/.gemini/antigravity-cli/env.sh
fi
AGY_ENV`
	if out, err := g.sh(script, nil); err != nil {
		return fmt.Errorf("~/.bashrc に agy 環境変数を書けない: %v: %s", err, out)
	}
	return nil
}
