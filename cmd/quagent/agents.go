package main

import (
	"encoding/json"
	"fmt"
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
	// command はエージェントのペインで実行するシェルスクリプト (/work で起動し、
	// 終わったらシェルに落とす)。
	command string
}

// DefaultAgent は特に指定がないときのエージェント。
const DefaultAgent = "opencode"

var agents = map[string]agentSpec{
	"opencode": {
		setup: setupOpencode,
		// 更新確認とモデル一覧の取得は外へ出られず DNS の拒否が並ぶだけなので止める
		command: "cd /work && OPENCODE_DISABLE_AUTOUPDATE=1 OPENCODE_DISABLE_MODELS_FETCH=1 opencode --auto /work; exec bash -l",
	},
	"claude": {
		setup:   setupClaude,
		command: "cd /work && claude --dangerously-skip-permissions; exec bash -l",
	},
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
