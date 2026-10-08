package vm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// argAfter は flag の直後の値を返す (無ければ空)。
func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// UEFI のときだけ OVMF を -bios で渡す。既定 (BIOS) のときは渡さない。
func TestQemuArgvUEFI(t *testing.T) {
	fw := filepath.Join(t.TempDir(), "OVMF.fd")
	if err := os.WriteFile(fw, []byte("firmware"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := QemuOpts{Disk: "disk", Seed: "seed", CPUs: 4, MemMiB: 4096, ConsoleLog: "console",
		UEFI: true, FirmwarePath: fw}
	args, err := QemuArgv(opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := argAfter(args, "-bios"); got != fw {
		t.Fatalf("-bios が %q でない: %v", got, args)
	}
	opts.UEFI, opts.FirmwarePath = false, ""
	if args, err = QemuArgv(opts); err != nil {
		t.Fatal(err)
	}
	if got := argAfter(args, "-bios"); got != "" {
		t.Fatalf("BIOS なのに -bios がある: %v", args)
	}
}

// OVMF が見つからなければエラーにする (黙って BIOS で起動しない)。
func TestOVMFPathNotFound(t *testing.T) {
	if _, err := ovmfPath(nil); err == nil {
		t.Fatal("候補が無いのに成功した")
	}
}

// ルートディスクは discard を有効にする (焼き込み後の fstrim で qcow2 を小さくする)。
func TestQemuArgvDiscard(t *testing.T) {
	args, err := QemuArgv(QemuOpts{Disk: "disk", Seed: "seed", CPUs: 1, MemMiB: 256, ConsoleLog: "console"})
	if err != nil {
		t.Fatal(err)
	}
	root := ""
	for _, a := range args {
		if strings.Contains(a, "id=root") {
			root = a
		}
	}
	if !strings.Contains(root, "discard=unmap") {
		t.Fatalf("ルートディスクに discard=unmap が無い: %q", root)
	}
}

func TestFreePort(t *testing.T) {
	p1, err := FreePort()
	if err != nil {
		t.Fatalf("FreePort() error: %v", err)
	}
	if p1 <= 0 || p1 > 65535 {
		t.Fatalf("FreePort() returned invalid port: %d", p1)
	}

	p2, err := FreePort()
	if err != nil {
		t.Fatalf("FreePort() second call error: %v", err)
	}
	if p2 <= 0 || p2 > 65535 {
		t.Fatalf("FreePort() returned invalid port: %d", p2)
	}
}

func TestOVMFPathFound(t *testing.T) {
	tmp := t.TempDir()
	p1 := filepath.Join(tmp, "non-existent.fd")
	p2 := filepath.Join(tmp, "found.fd")
	if err := os.WriteFile(p2, []byte("ovmf"), 0o644); err != nil {
		t.Fatal(err)
	}

	found, err := ovmfPath([]string{p1, p2})
	if err != nil {
		t.Fatalf("ovmfPath error: %v", err)
	}
	if found != p2 {
		t.Fatalf("ovmfPath() = %q, want %q", found, p2)
	}
}

func TestQemuArgvAllOptions(t *testing.T) {
	opts := QemuOpts{
		Disk:       "/path/to/disk.qcow2",
		Seed:       "/path/to/seed.iso",
		CPUs:       8,
		MemMiB:     8192,
		ConsoleLog: "/path/to/console.log",
		Netdev:     "dns=10.0.2.3,hostfwd=tcp::2222-:22",
		VsockCID:   12345,
		NestedVirt: true,
		DataDisks: []DataDisk{
			{Serial: "datadisk1", Path: "/path/to/disk,with,commas"},
		},
		Extra: []string{"-snapshot", "-daemonize"},
	}

	argv, err := QemuArgv(opts)
	if err != nil {
		t.Fatalf("QemuArgv error: %v", err)
	}

	// CPU
	if got := argAfter(argv, "-cpu"); got != "host" {
		t.Errorf("expected -cpu host with NestedVirt=true, got %q", got)
	}

	// Netdev
	if got := argAfter(argv, "-netdev"); !strings.Contains(got, "dns=10.0.2.3,hostfwd=tcp::2222-:22") {
		t.Errorf("expected netdev option to contain dns=10.0.2.3..., got %q", got)
	}

	// Vsock
	vsockFound := false
	for _, a := range argv {
		if strings.Contains(a, "guest-cid=12345") {
			vsockFound = true
			break
		}
	}
	if !vsockFound {
		t.Errorf("expected vsock device with guest-cid=12345 in argv: %v", argv)
	}

	// DataDisks with escaped commas
	diskFound := false
	for _, a := range argv {
		if strings.Contains(a, "/path/to/disk,,with,,commas") {
			diskFound = true
			break
		}
	}
	if !diskFound {
		t.Errorf("expected escaped comma disk path in argv: %v", argv)
	}

	// Extra
	if len(argv) < 2 || argv[len(argv)-2] != "-snapshot" || argv[len(argv)-1] != "-daemonize" {
		t.Errorf("expected extra arguments at the end of argv: %v", argv)
	}
}

func TestHostDNS(t *testing.T) {
	// HostDNS はホスト環境の resolv.conf を読む。見つかれば有効なIP文字列を返し、
	// 見つからなければエラーを返す。パニックしないことを確認。
	_, _ = HostDNS()
}

func TestOVMFPath(t *testing.T) {
	// システム上に OVMF があればパス、なければエラーが返る
	_, _ = OVMFPath()
}

func TestMakeSeed(t *testing.T) {
	tmp := t.TempDir()
	// xorriso が環境にない場合はエラーが返ることを確認
	_, err := MakeSeed(tmp, "inst-1", "host-1", "#cloud-config\n", map[string]string{"extra.txt": filepath.Join(tmp, "extra.txt")})
	if err == nil {
		// xorriso がある環境なら seed.iso ができている
		if _, statErr := os.Stat(filepath.Join(tmp, "seed.iso")); statErr != nil {
			t.Errorf("seed.iso not found: %v", statErr)
		}
	}
}

func TestMakeOverlay(t *testing.T) {
	tmp := t.TempDir()
	base := filepath.Join(tmp, "base.qcow2")
	overlay := filepath.Join(tmp, "overlay.qcow2")
	_ = os.WriteFile(base, []byte("fake qcow2"), 0o644)
	_ = MakeOverlay(base, overlay)
}

func TestQemuArgvSandbox(t *testing.T) {
	tTrue := true
	tFalse := false

	optsEnabled := QemuOpts{Disk: "disk", Seed: "seed", CPUs: 1, MemMiB: 256, ConsoleLog: "console", Sandbox: &tTrue}
	argv, err := QemuArgv(optsEnabled)
	if err != nil {
		t.Fatal(err)
	}
	got := argAfter(argv, "-sandbox")
	want := "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny"
	if got != want {
		t.Fatalf("-sandbox option is %q, want %q", got, want)
	}

	optsDisabled := QemuOpts{Disk: "disk", Seed: "seed", CPUs: 1, MemMiB: 256, ConsoleLog: "console", Sandbox: &tFalse}
	argv2, err := QemuArgv(optsDisabled)
	if err != nil {
		t.Fatal(err)
	}
	if got := argAfter(argv2, "-sandbox"); got != "" {
		t.Fatalf("expected no -sandbox when Sandbox is false, got %q", got)
	}
}

func TestProbeQemuSeccomp(t *testing.T) {
	// ProbeQemuSeccomp does not panic whether qemu is installed or not
	_ = ProbeQemuSeccomp()
}
