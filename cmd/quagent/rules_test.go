package main

import (
	"strings"
	"testing"
)

func TestGuestRulesContent(t *testing.T) {
	requiredKeywords := []string{
		"使い捨て QEMU VM",
		"プルリクエスト",
		".tmp/",
		"auto-memory",
		"リポジトリの skill",
		"~/.config/quagent/skills",
		"~/.ssh/quagent-mark",
		"commit.gpgsign=true",
		"rootless Docker",
		"再現可能な検証手順",
		"マシン固有事情",
	}
	for _, kw := range requiredKeywords {
		if !strings.Contains(guestRulesContent, kw) {
			t.Errorf("guestRulesContent に期待されるキーワード %q が含まれていません", kw)
		}
	}
}

func TestLinkRulesScript(t *testing.T) {
	script := linkRulesScript()
	expectedPaths := []string{
		"~/.config/opencode/AGENTS.md",
		"~/.claude/CLAUDE.md",
		"~/.gemini/config/GEMINI.md",
		"~/.gemini/antigravity-cli/GEMINI.md",
		"~/.gemini/config/rules/quagent.md",
		"~/.gemini/antigravity-cli/rules/quagent.md",
		"~/.codex/instructions.md",
		"~/.codex/CODEX.md",
	}
	for _, p := range expectedPaths {
		if !strings.Contains(script, p) {
			t.Errorf("linkRulesScript に期待されるパス %q が含まれていません", p)
		}
	}
	if !strings.Contains(script, "ln -sfn ~/.quagent/rules.md") {
		t.Errorf("linkRulesScript に正しいシンボリックリンクコマンドが含まれていません")
	}
}
