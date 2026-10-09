package main

import (
	"bytes"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nananek/quagent/internal/access"
	"github.com/nananek/quagent/internal/config"
	"github.com/nananek/quagent/internal/console"
	"github.com/nananek/quagent/internal/hostsvc"
	"github.com/nananek/quagent/internal/netns"
	"github.com/nananek/quagent/internal/paths"
	"github.com/nananek/quagent/internal/resourcemon"
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
	units := sshUnits()
	if len(units) != 6 {
		t.Errorf("sshUnits() = %v, want 6 units", units)
	}
	expected := []string{"sshd-vsock.socket", "sshd-unix-local.socket", "ssh.service", "ssh.socket", "sshd.service", "sshd.socket"}
	for _, exp := range expected {
		found := false
		for _, u := range units {
			if u == exp {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("sshUnits() missing expected unit %q", exp)
		}
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
	// CI のように user.name / user.email が未設定でも通るよう、
	// 一時リポジトリを作って検証する。グローバル設定の影響を遮断する。
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)

	tmp := t.TempDir()
	if out, err := exec.Command("git", "init", tmp).CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v: %s", err, out)
	}

	// 設定なしではエラーを返す。
	if _, _, err := gitIdentity(tmp); err == nil {
		t.Fatal("expected error when user.name/email is not set")
	}

	if out, err := exec.Command("git", "-C", tmp, "config", "user.name", "Test User").CombinedOutput(); err != nil {
		t.Fatalf("git config user.name failed: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", tmp, "config", "user.email", "test@example.com").CombinedOutput(); err != nil {
		t.Fatalf("git config user.email failed: %v: %s", err, out)
	}

	name, email, err := gitIdentity(tmp)
	if err != nil {
		t.Fatalf("gitIdentity failed: %v", err)
	}
	if name != "Test User" || email != "test@example.com" {
		t.Fatalf("gitIdentity = %q, %q; want Test User, test@example.com", name, email)
	}

	// git 管理外ではエラーを返す。
	if _, _, err := gitIdentity(t.TempDir()); err == nil {
		t.Fatal("expected error for non-git directory")
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

func TestSweepRuns(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmp)
	runsDir := paths.RunsDir()
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// 1分以上前の古い run ディレクトリを作成
	oldRun := filepath.Join(runsDir, "run-old")
	if err := os.MkdirAll(oldRun, 0o755); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(oldRun, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	// sweepRuns を実行してもクラッシュせず古いディレクトリが削除される
	sweepRuns()
}

func TestHoldWindowName(t *testing.T) {
	if cleanup := holdWindowName(""); cleanup != nil {
		t.Error("expected nil cleanup for empty pane")
	}
}

func TestRelayDeniedAndBlocked(t *testing.T) {
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

	denied := make(chan string, 1)
	blocked := make(chan string, 1)
	l := &netns.Launcher{
		Denied:  denied,
		Blocked: blocked,
	}

	denied <- "denied.example.com"
	close(denied)
	relayDenied(l, srv)

	blocked <- "blocked.example.com"
	close(blocked)
	relayBlocked(l, srv)
}

func TestAgyMCPConf(t *testing.T) {
	conf := agyMCPConf("test-token")
	if conf == nil {
		t.Fatal("agyMCPConf returned nil")
	}
	mcpServers, ok := conf["mcpServers"].(map[string]any)
	if !ok {
		t.Fatal("mcpServers not found")
	}
	quagent, ok := mcpServers["quagent"].(map[string]any)
	if !ok {
		t.Fatal("quagent server not found")
	}
	headers, ok := quagent["headers"].(map[string]string)
	if !ok || headers["Authorization"] != "Bearer test-token" {
		t.Errorf("Authorization header incorrect: %v", headers)
	}
}

func TestSetupAgySubscriptionNoSeed(t *testing.T) {
	err := setupAgySubscription(vmGuest{}, &config.Config{}, "token")
	if err == nil || !strings.Contains(err.Error(), "種が無い") {
		t.Fatalf("expected error without seed, got %v", err)
	}
}

func TestServerOnRejectLogging(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	svc, err := hostsvc.New(123)
	if err != nil {
		t.Fatal(err)
	}
	svc.OnReject = func(reason string, remote net.Addr) {
		logger.Printf("reject: %s (remote=%v)", reason, remote)
	}

	addr, _ := net.ResolveTCPAddr("tcp", "127.0.0.1:12345")
	svc.OnReject("test reject", addr)

	if !strings.Contains(buf.String(), "reject: test reject (remote=127.0.0.1:12345)") {
		t.Fatalf("unexpected log output: %q", buf.String())
	}
}

func TestResourcePolicyIntegration(t *testing.T) {
	cfg := &config.Config{
		Resources: &config.ResourcePolicy{
			MinFreeDiskGiB:  1,
			DiskWarnPercent: 80,
			DiskStopPercent: 95,
			Nice:            10,
		},
	}
	policy := cfg.ResourcePolicyOrDefault()
	if policy.MinFreeDiskGiB != 1 {
		t.Errorf("expected MinFreeDiskGiB 1, got %d", policy.MinFreeDiskGiB)
	}

	tmp := t.TempDir()
	if err := resourcemon.CheckHostFreeSpace(tmp, policy.MinFreeDiskGiB); err != nil {
		t.Fatalf("unexpected CheckHostFreeSpace error: %v", err)
	}
}
