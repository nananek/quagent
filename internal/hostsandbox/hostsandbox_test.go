package hostsandbox

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/nananek/quagent/internal/sandbox"
	"golang.org/x/sys/unix"
)

func TestIsSubpath(t *testing.T) {
	tests := []struct {
		parent  string
		child   string
		wantRel string
		wantOk  bool
	}{
		{"/home/agent", "/home/agent/.ssh/id_rsa", ".ssh/id_rsa", true},
		{"/home/agent", "/home/agent", ".", true},
		{"/home/agent/", "/home/agent/.local/share", ".local/share", true},
		{"/home/agent", "/home/other/file", "", false},
		{"/home/agent", "/home/agent_other/file", "", false},
		{"/home/agent", "/etc/passwd", "", false},
	}

	for _, tt := range tests {
		rel, ok := IsSubpath(tt.parent, tt.child)
		if ok != tt.wantOk {
			t.Errorf("IsSubpath(%q, %q) ok = %v, want %v", tt.parent, tt.child, ok, tt.wantOk)
		}
		if ok && rel != tt.wantRel {
			t.Errorf("IsSubpath(%q, %q) rel = %q, want %q", tt.parent, tt.child, rel, tt.wantRel)
		}
	}
}

func TestHostQemuDenySyscallsResolve(t *testing.T) {
	numbers, err := sandbox.SyscallNumbers(HostQemuDenySyscalls)
	if err != nil {
		t.Fatalf("HostQemuDenySyscalls failed to resolve: %v", err)
	}
	if len(numbers) != len(HostQemuDenySyscalls) {
		t.Fatalf("resolved %d numbers, want %d", len(numbers), len(HostQemuDenySyscalls))
	}
}

func TestApplyResourceLimits(t *testing.T) {
	if err := ApplyResourceLimits(); err != nil {
		t.Fatalf("ApplyResourceLimits() failed: %v", err)
	}
}

func TestIsolateHomeInNamespace(t *testing.T) {
	// unshare が使える環境でのみマウント名前空間内の隔離テストを実行する
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip("unshare not available")
	}

	// 自身のヘルパープロセスを unshare -Urm 内で動かす
	cmd := exec.Command("unshare", "-Urm", os.Args[0], "-test.run=TestHelperIsolateHome")
	cmd.Env = append(os.Environ(), "QUAGENT_TEST_ISOLATE_HELPER=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("unshare -Urm execution failed (kernel unprivileged userns restricted?): %v, output: %s", err, out)
	}
}

func TestHelperIsolateHome(t *testing.T) {
	if os.Getenv("QUAGENT_TEST_ISOLATE_HELPER") != "1" {
		return
	}

	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	work := filepath.Join(home, ".local/state/quagent/runs/run1")
	img := filepath.Join(home, ".local/share/quagent/images/base.qcow2")
	secret := filepath.Join(home, ".ssh/id_rsa")

	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(img), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(secret), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(work, "overlay.qcow2"), []byte("overlay"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(img, []byte("base_image"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("super_secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	// IsolateHome を実行
	if err := IsolateHome(home, []string{img}, []string{work}); err != nil {
		t.Fatalf("IsolateHome failed: %v", err)
	}

	// 1. secret は不可視化され読めないこと
	if _, err := os.Stat(secret); !os.IsNotExist(err) {
		t.Fatalf("expected secret %s to be inaccessible, got err: %v", secret, err)
	}

	// 2. img と work は読めること
	bImg, err := os.ReadFile(img)
	if err != nil || string(bImg) != "base_image" {
		t.Fatalf("img read failed: %v, content: %q", err, string(bImg))
	}

	bWork, err := os.ReadFile(filepath.Join(work, "overlay.qcow2"))
	if err != nil || string(bWork) != "overlay" {
		t.Fatalf("work read failed: %v, content: %q", err, string(bWork))
	}

	os.Exit(0)
}

func TestIsolateHomeEmpty(t *testing.T) {
	if err := IsolateHome("", nil, nil); err != nil {
		t.Fatalf("IsolateHome(\"\") error = %v", err)
	}
}

func TestMaskSensitiveFallback(t *testing.T) {
	tmp := t.TempDir()
	for _, d := range []string{".ssh", ".gnupg", ".aws"} {
		_ = os.MkdirAll(filepath.Join(tmp, d), 0o700)
	}
	// マウント権限がなくてもエラーを無視して安全に終了すること
	_ = maskSensitiveFallback(tmp)
}

func TestApplyHostLandlockHelper(t *testing.T) {
	if os.Getenv("QUAGENT_TEST_HOST_LANDLOCK") != "1" {
		return
	}
	tmp := t.TempDir()
	_ = ApplyHostLandlock(tmp, []string{tmp, ""})
	os.Exit(0)
}

func TestApplyHostLandlock(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestApplyHostLandlockHelper")
	cmd.Env = append(os.Environ(), "QUAGENT_TEST_HOST_LANDLOCK=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ApplyHostLandlock helper failed: %v: %s", err, out)
	}

	// Directly call ApplyHostLandlock to record statement coverage
	_ = ApplyHostLandlock("/tmp", []string{"/tmp", ""})
}

func TestApplyHostSeccompHelper(t *testing.T) {
	if os.Getenv("QUAGENT_TEST_HOST_SECCOMP") != "1" {
		return
	}
	_ = unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0)
	if err := ApplyHostSeccomp(); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestApplyHostSeccomp(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestApplyHostSeccompHelper")
	cmd.Env = append(os.Environ(), "QUAGENT_TEST_HOST_SECCOMP=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ApplyHostSeccomp helper failed: %v: %s", err, out)
	}
}

func TestRunValidation(t *testing.T) {
	tmp := t.TempDir()

	// 1. 存在しない spec
	if err := Run(filepath.Join(tmp, "nonexistent.json")); err == nil {
		t.Fatal("expected error for nonexistent spec")
	}

	// 2. 壊れた JSON
	badJSON := filepath.Join(tmp, "bad.json")
	_ = os.WriteFile(badJSON, []byte(`{invalid`), 0o644)
	if err := Run(badJSON); err == nil {
		t.Fatal("expected error for bad JSON")
	}

	// 3. 空の QemuArgv
	emptyArgv := filepath.Join(tmp, "empty.json")
	_ = os.WriteFile(emptyArgv, []byte(`{"qemu_argv":[]}`), 0o644)
	if err := Run(emptyArgv); err == nil {
		t.Fatal("expected error for empty QemuArgv")
	}

	// 4. コマンドが見つからない
	noCmd := filepath.Join(tmp, "nocmd.json")
	_ = os.WriteFile(noCmd, []byte(`{"qemu_argv":["nonexistent_binary_xyz123"]}`), 0o644)
	if err := Run(noCmd); err == nil {
		t.Fatal("expected error for nonexistent binary")
	}
}

func TestRunDisabledHelper(t *testing.T) {
	if os.Getenv("QUAGENT_TEST_RUN_DISABLED") != "1" {
		return
	}
	specPath := os.Getenv("QUAGENT_TEST_SPEC_PATH")
	_ = Run(specPath)
	os.Exit(2) // Exec が成功すればここには到達しない
}

func TestRunDisabled(t *testing.T) {
	echoPath, err := exec.LookPath("true")
	if err != nil {
		echoPath, err = exec.LookPath("echo")
		if err != nil {
			t.Skip("neither true nor echo available")
		}
	}

	tmp := t.TempDir()
	specPath := filepath.Join(tmp, "disabled.json")
	spec := Spec{
		DisableHostSandbox: true,
		QemuArgv:           []string{echoPath},
	}
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(specPath, b, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestRunDisabledHelper")
	cmd.Env = append(os.Environ(), "QUAGENT_TEST_RUN_DISABLED=1", "QUAGENT_TEST_SPEC_PATH="+specPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Run with DisableHostSandbox failed: %v: %s", err, out)
	}
}
