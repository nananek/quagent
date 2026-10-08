package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nananek/quagent/internal/access"
	"github.com/nananek/quagent/internal/hostsandbox"
	"github.com/nananek/quagent/internal/netns"
)

func TestDispatchValidation(t *testing.T) {
	// 1. 未知のコマンド -> usage を出して exit するのではなく、ここでは引数異常のエラーを検証
	// 子コマンドの引数バリデーション
	if err := dispatch([]string{"__guest"}); err == nil {
		t.Error("expected error for __guest without port")
	}
	if err := dispatch([]string{"__guest", "not_a_port"}); err == nil {
		t.Error("expected error for __guest with invalid port")
	}

	if err := dispatch([]string{"__console"}); err == nil {
		t.Error("expected error for __console without socket path")
	}

	if err := dispatch([]string{netns.ChildCommand}); err == nil {
		t.Error("expected error for netns.ChildCommand without spec path")
	}

	if err := dispatch([]string{hostsandbox.LauncherCommand}); err == nil {
		t.Error("expected error for hostsandbox.LauncherCommand without spec path")
	}

	if err := dispatch([]string{"image"}); err == nil {
		t.Error("expected error for image without subcommand")
	}
	if err := dispatch([]string{"image", "unknown"}); err == nil {
		t.Error("expected error for image with unknown subcommand")
	}
	if err := dispatch([]string{"image", "rm"}); err == nil {
		t.Error("expected error for image rm without image name")
	}

	if err := dispatch([]string{"always", "unknown"}); err == nil {
		t.Error("expected error for always with unknown subcommand")
	}
	if err := dispatch([]string{"always", "rm"}); err == nil {
		t.Error("expected error for always rm without domain")
	}
}

func TestDispatchImageRecipes(t *testing.T) {
	if err := dispatch([]string{"image", "recipes"}); err != nil {
		t.Fatalf("dispatch(image recipes) error: %v", err)
	}
}

func TestDispatchImageLs(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	if err := dispatch([]string{"image", "ls"}); err != nil {
		t.Fatalf("dispatch(image ls) error: %v", err)
	}
}

func TestDispatchAlwaysLs(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	if err := dispatch([]string{"always", "ls"}); err != nil {
		t.Fatalf("dispatch(always ls) error: %v", err)
	}
}

func TestDispatchAlwaysRm(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	p := access.AlwaysPath()
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte(`["example.com"]`), 0o600)
	if err := dispatch([]string{"always", "rm", "example.com"}); err != nil {
		t.Fatalf("dispatch(always rm example.com) error: %v", err)
	}
}

func TestCmdRunArgErrors(t *testing.T) {
	// 不正なエージェント名
	if err := cmdRun([]string{"--agent", "unknown_agent"}); err == nil {
		t.Error("expected error for unknown agent")
	}

	// 存在しないリポジトリパス
	nonExistent := filepath.Join(t.TempDir(), "nonexistent")
	if err := cmdRun([]string{"--repo", nonExistent}); err == nil {
		t.Error("expected error for nonexistent repo")
	}

	// gitリポジトリでないディレクトリ
	emptyDir := t.TempDir()
	if err := cmdRun([]string{"--repo", emptyDir}); err == nil {
		t.Error("expected error for non-git directory")
	}
}
