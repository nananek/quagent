package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// getppid は拒否しても実害が無く、結果を観測しやすい (呼べば親 pid、拒否されれば
// EPERM) のでテストで使う。production の syscall 表には入れず、テストバイナリだけが
// 知る (設定で書けるのは実際に拒否したい syscall だけにする)。
func init() { syscallNumbers["getppid"] = unix.SYS_GETPPID }

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

func TestLoadInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte(`{invalid`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestLoadInvalidMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "badmode.json")
	if err := os.WriteFile(path, []byte(`{"mode":"invalid"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for invalid mode in Load")
	}
}

func TestPolicyJSON(t *testing.T) {
	p := &Policy{
		Mode:      "compat",
		ExtraDeny: []string{"chroot"},
	}
	b, err := p.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 {
		t.Fatal("expected non-empty JSON")
	}
}

func TestPolicyReadWritePaths(t *testing.T) {
	// 1. 明示パスあり
	explicit := &Policy{ReadWritePaths: []string{"/custom/path"}}
	paths, isExplicit := explicit.readWritePaths()
	if !isExplicit || len(paths) != 1 || paths[0] != "/custom/path" {
		t.Fatalf("explicit paths = %v, %v", paths, isExplicit)
	}

	// 2. 既定パス
	def := &Policy{}
	defPaths, isExplicitDef := def.readWritePaths()
	if isExplicitDef {
		t.Fatal("expected isExplicit = false for default paths")
	}
	foundWork := false
	for _, p := range defPaths {
		if p == "/work" {
			foundWork = true
			break
		}
	}
	if !foundWork {
		t.Fatalf("expected /work in default paths, got %v", defPaths)
	}
}

func TestRunWithValidation(t *testing.T) {
	// 1. 空のargv
	if err := RunWith(nil, ""); err == nil {
		t.Fatal("expected error for nil argv")
	}
	if err := RunWith([]string{"--"}, ""); err == nil {
		t.Fatal("expected error for argv containing only '--'")
	}

	// 2. 壊れた設定ファイルパス
	badPath := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(badPath, []byte(`{invalid`), 0o644)
	if err := RunWith([]string{"echo"}, badPath); err == nil {
		t.Fatal("expected error for invalid config in RunWith")
	}
}

func TestSyscallNumber(t *testing.T) {
	nr, ok := SyscallNumber("bpf")
	if !ok || nr != unix.SYS_BPF {
		t.Fatalf("SyscallNumber(bpf) = (%d, %v), want (%d, true)", nr, ok, unix.SYS_BPF)
	}
	_, ok = SyscallNumber("nonexistent_syscall")
	if ok {
		t.Fatal("expected false for nonexistent_syscall")
	}
}

func TestSyscallNumbers(t *testing.T) {
	nums, err := SyscallNumbers([]string{"bpf", "ptrace", "bpf"})
	if err != nil {
		t.Fatal(err)
	}
	if len(nums) != 2 {
		t.Fatalf("expected dedup length 2, got %d", len(nums))
	}
	_, err = SyscallNumbers([]string{"bpf", "unknown_sc"})
	if err == nil {
		t.Fatal("expected error for unknown syscall")
	}
}

func TestDenyVsockPolicy(t *testing.T) {
	var pNil *Policy
	if !pNil.DenyVsockOn() {
		t.Fatal("nil policy should have DenyVsockOn = true")
	}

	pDef := Default()
	if !pDef.DenyVsockOn() {
		t.Fatal("default policy should have DenyVsockOn = true")
	}

	pEmpty := &Policy{}
	if !pEmpty.DenyVsockOn() {
		t.Fatal("empty policy should have DenyVsockOn = true")
	}

	tFalse := false
	pOff := &Policy{DenyVsock: &tFalse}
	if pOff.DenyVsockOn() {
		t.Fatal("policy with DenyVsock=false should have DenyVsockOn = false")
	}

	tTrue := true
	pOn := &Policy{DenyVsock: &tTrue}
	if !pOn.DenyVsockOn() {
		t.Fatal("policy with DenyVsock=true should have DenyVsockOn = true")
	}

	b, err := pOff.JSON()
	if err != nil {
		t.Fatal(err)
	}
	tmpFile := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(tmpFile, b, 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(tmpFile)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DenyVsockOn() {
		t.Fatal("loaded policy should preserve DenyVsock = false")
	}
}

func TestBuildSeccompFilter(t *testing.T) {
	// amd64, denyVsock=false
	f1 := buildSeccompFilter(unix.AUDIT_ARCH_X86_64, uint32(unix.EPERM), []int{unix.SYS_GETPPID}, false)
	// amd64, denyVsock=true
	f2 := buildSeccompFilter(unix.AUDIT_ARCH_X86_64, uint32(unix.EPERM), []int{unix.SYS_GETPPID}, true)
	if len(f2) <= len(f1) {
		t.Fatalf("expected f2 (with vsock deny) to have more instructions than f1: %d <= %d", len(f2), len(f1))
	}

	// arm64, denyVsock=false
	f3 := buildSeccompFilter(unix.AUDIT_ARCH_AARCH64, uint32(unix.EPERM), nil, false)
	// arm64, denyVsock=true
	f4 := buildSeccompFilter(unix.AUDIT_ARCH_AARCH64, uint32(unix.EPERM), nil, true)
	if len(f4) <= len(f3) {
		t.Fatalf("expected f4 to have more instructions than f3: %d <= %d", len(f4), len(f3))
	}

	// 未知のアーキテクチャ
	f5 := buildSeccompFilter(0x1234, uint32(unix.EPERM), nil, true)
	if len(f5) == 0 {
		t.Fatal("expected filter for unknown arch")
	}
}

func TestPolicy_MaskAgentConfigAndDockerInherit(t *testing.T) {
	// nil policy
	var nilP *Policy
	if !nilP.MaskAgentConfigOn() {
		t.Errorf("expected nil policy to have MaskAgentConfigOn true")
	}
	if !nilP.DockerInheritOn() {
		t.Errorf("expected nil policy to have DockerInheritOn true")
	}
	if names := nilP.DenyNames(); len(names) == 0 {
		t.Errorf("expected nil policy to have non-empty DenyNames")
	}

	// default policy (omitted fields -> true)
	p := Default()
	if !p.MaskAgentConfigOn() {
		t.Errorf("expected Default to have MaskAgentConfigOn true")
	}
	if !p.DockerInheritOn() {
		t.Errorf("expected Default to have DockerInheritOn true")
	}

	// explicit false
	f := false
	p2 := &Policy{
		MaskAgentConfig: &f,
		DockerInherit:   &f,
	}
	if p2.MaskAgentConfigOn() {
		t.Errorf("expected false for MaskAgentConfigOn")
	}
	if p2.DockerInheritOn() {
		t.Errorf("expected false for DockerInheritOn")
	}

	// JSON unmarshaling
	raw := []byte(`{"mask_agent_config": false, "docker_inherit": true}`)
	var p3 Policy
	if err := json.Unmarshal(raw, &p3); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if p3.MaskAgentConfigOn() {
		t.Errorf("expected false for p3.MaskAgentConfigOn")
	}
	if !p3.DockerInheritOn() {
		t.Errorf("expected true for p3.DockerInheritOn")
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
