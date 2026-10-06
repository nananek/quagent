package vm

import (
	"os"
	"path/filepath"
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
