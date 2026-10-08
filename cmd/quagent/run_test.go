package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nananek/quagent/internal/access"
	"github.com/nananek/quagent/internal/config"
	"github.com/nananek/quagent/internal/console"
	"github.com/nananek/quagent/internal/netns"
	"github.com/nananek/quagent/internal/paths"
)

func TestNormalizePassthrough(t *testing.T) {
	// 1. 空スライス
	got, err := normalizePassthrough(nil)
	if err != nil || got != nil {
		t.Fatalf("normalizePassthrough(nil) = %v, %v; want nil, nil", got, err)
	}

	// 2. 正常系
	in := []string{"Example.COM", "*.Api.Example.Com"}
	got, err = normalizePassthrough(in)
	if err != nil {
		t.Fatalf("normalizePassthrough valid failed: %v", err)
	}
	want := []string{"*.api.example.com", "example.com"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("normalizePassthrough = %v, want %v", got, want)
	}

	// 3. 不正なドメイン
	if _, err := normalizePassthrough([]string{"bad..domain"}); err == nil {
		t.Fatal("expected error on invalid domain")
	}
}

func TestHostTimezone(t *testing.T) {
	t.Setenv("TZ", "Asia/Tokyo")
	if got := hostTimezone(); got != "Asia/Tokyo" {
		t.Errorf("hostTimezone() = %q, want Asia/Tokyo", got)
	}

	t.Setenv("TZ", "UTC")
	if got := hostTimezone(); got != "UTC" {
		t.Errorf("hostTimezone() = %q, want UTC", got)
	}

	t.Setenv("TZ", "../invalid/path")
	if got := hostTimezone(); got != "" {
		t.Errorf("hostTimezone() with path traversal = %q, want ''", got)
	}
}

func TestSSHUnits(t *testing.T) {
	unitsOn := sshUnits(true)
	if len(unitsOn) != 2 {
		t.Errorf("sshUnits(true) = %v, want 2 units", unitsOn)
	}

	unitsOff := sshUnits(false)
	if len(unitsOff) != 6 {
		t.Errorf("sshUnits(false) = %v, want 6 units", unitsOff)
	}
}

func TestMaskCmd(t *testing.T) {
	cmd := maskCmd([]string{"ssh.service", "sshd.socket"})
	if !strings.Contains(cmd, "systemctl mask --now") || !strings.Contains(cmd, "ssh.service") || !strings.Contains(cmd, "sshd.socket") {
		t.Errorf("maskCmd output unexpected: %s", cmd)
	}
}

func TestClipboardSink(t *testing.T) {
	// 1. デフォルト / tmux
	sink, err := clipboardSink(config.Clipboard{})
	if err != nil || sink == nil {
		t.Fatalf("clipboardSink default error: %v", err)
	}
	sink, err = clipboardSink(config.Clipboard{Method: "tmux"})
	if err != nil || sink == nil {
		t.Fatalf("clipboardSink tmux error: %v", err)
	}

	// 2. osc52
	sink, err = clipboardSink(config.Clipboard{Method: "osc52"})
	if err != nil || sink == nil {
		t.Fatalf("clipboardSink osc52 error: %v", err)
	}

	// 3. command 成功
	sink, err = clipboardSink(config.Clipboard{Method: "command", Command: []string{"cat"}})
	if err != nil || sink == nil {
		t.Fatalf("clipboardSink command error: %v", err)
	}

	// 4. command コマンド未指定
	if _, err := clipboardSink(config.Clipboard{Method: "command"}); err == nil {
		t.Fatal("expected error when command is empty")
	}

	// 5. 不明なメソッド
	if _, err := clipboardSink(config.Clipboard{Method: "unknown"}); err == nil {
		t.Fatal("expected error for unknown clipboard method")
	}
}

func TestNewCID(t *testing.T) {
	for range 10 {
		cid := newCID()
		if cid < 3 {
			t.Errorf("newCID() = %d, want >= 3", cid)
		}
	}
}

func TestShellJoin(t *testing.T) {
	got := shellJoin([]string{"echo", "hello world", "foo'bar"})
	want := "'echo' 'hello world' 'foo'\\''bar'"
	if got != want {
		t.Errorf("shellJoin = %q, want %q", got, want)
	}
}

func TestRepoRoot(t *testing.T) {
	root, err := repoRoot(".")
	if err != nil {
		t.Fatalf("repoRoot(.) error: %v", err)
	}
	if !filepath.IsAbs(root) {
		t.Errorf("repoRoot = %q, want absolute path", root)
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		t.Errorf("repoRoot = %q is not a directory: %v", root, err)
	}
	if fi, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		// worktree の場合は .git がファイルになることもあるので存在だけ確認する。
		t.Errorf("repoRoot = %q has no .git: %v", root, err)
	} else if fi.IsDir() || fi.Mode().IsRegular() {
		// ok: .git がディレクトリ (通常clone) かファイル (worktree) のいずれか。
	} else {
		t.Errorf("repoRoot = %q has unexpected .git: %v", root, fi.Mode())
	}

	// git 管理外のディレクトリではエラーを返す。
	tmp := t.TempDir()
	if _, err := repoRoot(tmp); err == nil {
		t.Fatal("expected error for non-git directory")
	}

	if _, err := repoRoot("/nonexistent/directory/quagent"); err == nil {
		t.Fatal("expected error for non-git directory")
	}
}

func TestGitIdentity(t *testing.T) {
	name, email, err := gitIdentity(".")
	if err != nil {
		t.Fatalf("gitIdentity failed: %v", err)
	}
	if name == "" || email == "" {
		t.Fatalf("gitIdentity returned empty name/email: %q, %q", name, email)
	}
}

func TestLockRun(t *testing.T) {
	tmp := t.TempDir()

	// 1. ロック取得
	f1, err := lockRun(tmp)
	if err != nil {
		t.Fatalf("lockRun failed: %v", err)
	}

	// 2. 二重取得は失敗
	if _, err := lockRun(tmp); err == nil {
		t.Fatal("expected error on duplicate lockRun")
	}

	// 3. 解放
	if err := f1.Close(); err != nil {
		t.Fatal(err)
	}

	// 4. 解放後は再取得可能
	f2, err := lockRun(tmp)
	if err != nil {
		t.Fatalf("second lockRun failed after close: %v", err)
	}
	_ = f2.Close()
}

func TestSaveLogs(t *testing.T) {
	tmpState := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmpState)

	runDir := filepath.Join(tmpState, "quagent", "runs", "run1")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "host.log"), []byte("host log content"), 0o644); err != nil {
		t.Fatal(err)
	}

	saveLogs(runDir)

	savedLog := filepath.Join(paths.LogsDir(), "run1", "host.log")
	b, err := os.ReadFile(savedLog)
	if err != nil || string(b) != "host log content" {
		t.Fatalf("saveLogs failed: %v, content = %q", err, string(b))
	}
}

type noopApplier struct{}

func (noopApplier) SetGrants([]netns.Grant) error { return nil }

func TestRelayNames(t *testing.T) {
	tmp := t.TempDir()
	m, err := access.NewManager(noopApplier{}, filepath.Join(tmp, "always.json"))
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(tmp, "console.sock")
	srv, err := console.NewServer(m, sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)

	ch := make(chan string, 5)
	done := make(chan struct{})
	go func() {
		relayNames(ch, srv, "TEST: ")
		close(done)
	}()

	ch <- "domain1.com"
	ch <- "domain2.com"
	close(ch)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("relayNames did not exit on channel close")
	}
}
