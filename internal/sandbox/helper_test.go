package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// x32ABIBit は amd64 で x32 ABI の syscall 番号に立つビット。
const x32ABIBit = 0x40000000

// TestSandboxHelper はサブプロセスとしてだけ動く。seccomp / Landlock は一度
// かけると戻せないので、親のテストプロセスでは試せない。
func TestSandboxHelper(t *testing.T) {
	switch os.Getenv("QUAGENT_SANDBOX_HELPER") {
	case "":
		t.Skip("サブプロセス専用")
	case "seccomp":
		checkGetppidDenied(&Policy{Mode: "compat", ExtraDeny: []string{"getppid"}})
	case "getppid":
		wantGetppidEPERM()
	case "launch":
		// 別プロセスで __sandbox に exec させる。exec 先はこのテストバイナリを
		// getppid モードで起動したもの。方針は親から渡されたパス。
		parent := os.Getenv("QUAGENT_SANDBOX_PARENT")
		_ = os.Setenv("QUAGENT_SANDBOX_HELPER", "getppid")
		if err := RunWith([]string{"--", os.Args[0], "-test.run=^TestSandboxHelper$"}, parent); err != nil {
			fmt.Fprintln(os.Stderr, "RunWith:", err)
			os.Exit(2)
		}
		os.Exit(0) // exec に成功すれば戻らない
	case "landlock":
		checkLandlock()
	case "x32":
		checkX32Rejected(&Policy{Mode: "compat"})
	}
}

// checkX32Rejected は x32 ABI の syscall 自体が拒否されることを確かめる。deny 一覧に
// 無い getppid を x32 で呼ぶ。拒否されればここには戻らない (SIGSYS)。戻ってきたら
// os.Exit(0) で「素通り」を親に伝える (x32 非対応のカーネルは ENOSYS で 3)。
func checkX32Rejected(p *Policy) {
	if err := p.Apply(); err != nil {
		fmt.Fprintln(os.Stderr, "Apply:", err)
		os.Exit(2)
	}
	_, _, errno := unix.Syscall(uintptr(x32ABIBit|unix.SYS_GETPPID), 0, 0, 0)
	if errno == unix.ENOSYS {
		os.Exit(3) // カーネルが x32 ABI を持たない
	}
	os.Exit(0) // 素通り = 失敗 (親が検知する)
}

// TestSeccompRejectsX32 は、x32 ABI が arch を偽装して deny 一覧をすり抜けるのを
// 塞げているかを確かめる。修正前は素通りしてヘルパーが 0 で終わる。
func TestSeccompRejectsX32(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("x32 ABI は amd64 のみ")
	}
	runX32Helper(t)
}

// runX32Helper はヘルパーを起動し、SIGSYS で殺される (拒否) か、x32 非対応の
// 3 で終わる (skip) ことを確かめる。素通り (終了コード 0) なら失敗。
func runX32Helper(t *testing.T) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSandboxHelper$")
	cmd.Env = append(os.Environ(), "QUAGENT_SANDBOX_HELPER=x32")
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() && ws.Signal() == syscall.SIGSYS {
				return
			}
			if ws.Exited() && ws.ExitStatus() == 3 {
				t.Skip("このカーネルは x32 ABI が無効")
			}
		}
	}
	t.Fatalf("x32 syscall が拒否されなかった (arch 検査だけではすり抜ける): err=%v out=%s", err, out)
}

// checkGetppidDenied は方針をかけてから getppid を呼び、EPERM になることを確かめる。
func checkGetppidDenied(p *Policy) {
	if err := p.Apply(); err != nil {
		fmt.Fprintln(os.Stderr, "Apply:", err)
		os.Exit(2)
	}
	wantGetppidEPERM()
}

func wantGetppidEPERM() {
	_, _, errno := unix.Syscall(unix.SYS_GETPPID, 0, 0, 0)
	if errno == unix.EPERM {
		os.Exit(0)
	}
	fmt.Fprintf(os.Stderr, "getppid errno=%v (EPERM を期待)\n", errno)
	os.Exit(7)
}

// checkLandlock は rw への書き込みは通り、それ以外への書き込みは拒否され、
// 読み取りは制限されないことを確かめる。
func checkLandlock() {
	base := os.Getenv("QUAGENT_LANDLOCK_BASE")
	rw := filepath.Join(base, "rw")
	other := filepath.Join(base, "other")
	mustMkdir(rw)
	mustMkdir(other)
	secret := filepath.Join(other, "secret.txt")
	mustWrite(secret, "hi")

	p := &Policy{Landlock: true, ReadWritePaths: []string{rw}}
	if err := p.Apply(); err != nil {
		fmt.Fprintln(os.Stderr, "Apply:", err)
		os.Exit(2)
	}
	if err := os.WriteFile(filepath.Join(rw, "new.txt"), []byte("x"), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "rw への書き込みが拒否された:", err)
		os.Exit(3)
	}
	if err := os.WriteFile(filepath.Join(other, "new.txt"), []byte("x"), 0o644); err == nil {
		fmt.Fprintln(os.Stderr, "rw 以外への書き込みが通った")
		os.Exit(4)
	}
	if _, err := os.ReadFile(secret); err != nil {
		fmt.Fprintln(os.Stderr, "読み取りが拒否された:", err)
		os.Exit(5)
	}
	os.Exit(0)
}

func mustMkdir(dir string) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func mustWrite(path, data string) {
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func TestSeccompDeniesSyscall(t *testing.T) {
	if _, ok := auditArch(); !ok {
		t.Skip("このアーキテクチャは対象外")
	}
	runHelper(t, map[string]string{"QUAGENT_SANDBOX_HELPER": "seccomp"})
}

func TestLauncherAppliesPolicy(t *testing.T) {
	if _, ok := auditArch(); !ok {
		t.Skip("このアーキテクチャは対象外")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "sandbox.json")
	if err := os.WriteFile(path, []byte(`{"enabled":true,"mode":"compat","extra_deny":["getppid"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	runHelper(t, map[string]string{
		"QUAGENT_SANDBOX_HELPER": "launch",
		"QUAGENT_SANDBOX_PARENT": path,
	})
}

func TestLandlockConfinesWrites(t *testing.T) {
	abi, err := landlockABI()
	if err != nil || abi < 1 {
		t.Skip("Landlock が使えない")
	}
	runHelper(t, map[string]string{
		"QUAGENT_SANDBOX_HELPER": "landlock",
		"QUAGENT_LANDLOCK_BASE":  t.TempDir(),
	})
}

func TestWrap(t *testing.T) {
	got := Wrap("/usr/local/bin/quagent-guest", []string{"bash", "-lc", "echo hi"})
	want := []string{"/usr/local/bin/quagent-guest", LauncherCommand, "--", "bash", "-lc", "echo hi"}
	if len(got) != len(want) {
		t.Fatalf("got=%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got=%v want=%v", got, want)
		}
	}
}

// runHelper はこのテストバイナリを env 付きで起動し、終了コード 0 を確かめる。
func runHelper(t *testing.T, env map[string]string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSandboxHelper$")
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper が失敗: %v\n%s", err, out)
	}
}
