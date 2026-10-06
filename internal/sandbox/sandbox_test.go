package sandbox

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDenyNumbersModes(t *testing.T) {
	compat, err := (&Policy{}).DenyNumbers()
	if err != nil {
		t.Fatal(err)
	}
	if !contains(compat, unix.SYS_BPF) {
		t.Fatalf("compat で bpf を拒否していない: %v", compat)
	}
	// io_uring は Bun が使うことがあるので compat では拒否しない
	if contains(compat, unix.SYS_IO_URING_SETUP) {
		t.Fatalf("compat で io_uring を拒否している: %v", compat)
	}

	strict, err := (&Policy{Mode: "strict"}).DenyNumbers()
	if err != nil {
		t.Fatal(err)
	}
	for _, nr := range []int{unix.SYS_MOUNT, unix.SYS_UNSHARE, unix.SYS_IO_URING_SETUP} {
		if !contains(strict, nr) {
			t.Fatalf("strict で %d を拒否していない", nr)
		}
	}
}

func TestDenyNumbersExtraAndDedup(t *testing.T) {
	p := &Policy{ExtraDeny: []string{"chroot", "ptrace"}}
	got, err := p.DenyNumbers()
	if err != nil {
		t.Fatal(err)
	}
	if !contains(got, unix.SYS_CHROOT) {
		t.Fatalf("extra の chroot が入っていない: %v", got)
	}
	// ptrace は compat と extra の両方にあるが、重複しないこと
	if n := count(got, unix.SYS_PTRACE); n != 1 {
		t.Fatalf("ptrace が %d 回入っている", n)
	}
}

func TestDenyNumbersUnknown(t *testing.T) {
	if _, err := (&Policy{ExtraDeny: []string{"not_a_syscall"}}).DenyNumbers(); err == nil {
		t.Fatal("未知の syscall 名を受け付けた")
	}
}

func TestValidateMode(t *testing.T) {
	if err := (&Policy{Mode: "loose"}).Validate(); err == nil {
		t.Fatal("不正な mode を受け付けた")
	}
}

func TestOnDefault(t *testing.T) {
	if !Default().On() {
		t.Fatal("既定が有効でない")
	}
	if !(&Policy{}).On() {
		t.Fatal("enabled 省略は有効のはず")
	}
	if (&Policy{Enabled: boolPtr(false)}).On() {
		t.Fatal("enabled:false が有効になっている")
	}
}

func TestLoadMissingIsNil(t *testing.T) {
	p, err := Load(filepath.Join(t.TempDir(), "none.json"))
	if err != nil || p != nil {
		t.Fatalf("p=%v err=%v", p, err)
	}
}

func TestLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox.json")
	if err := os.WriteFile(path, []byte(`{"enabled":true,"mode":"strict","landlock":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !p.On() || p.Mode != "strict" || !p.Landlock {
		t.Fatalf("p=%+v", p)
	}
}

func TestLoadModeOnlyStaysOn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox.json")
	if err := os.WriteFile(path, []byte(`{"mode":"strict"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !p.On() {
		t.Fatal("mode だけ書いたときに無効になった")
	}
}

func contains(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func count(xs []int, v int) int {
	n := 0
	for _, x := range xs {
		if x == v {
			n++
		}
	}
	return n
}
