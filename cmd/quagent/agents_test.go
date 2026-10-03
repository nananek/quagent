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
