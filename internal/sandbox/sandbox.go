// Package sandbox は VM の中のコマンドに、本人には外せない一枚 (seccomp と
// Landlock) をかける。
//
// どちらもカーネルが強制し、一度かけると本人は緩められず、子プロセスへ継承される。
// VM の外 (host) の許可制を置き換えるものではなく、VM 脱出の足がかりになる syscall
// を減らすための追加の一枚。方針は host が cloud-init で ConfigPath に置き、起動役
// (自身の隠しサブコマンド `__sandbox`) がコマンドを exec する前にかける。
package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// ConfigPath は VM の中に方針を書いたファイル。host が cloud-init で置く (root 所有)。
const ConfigPath = "/etc/quagent/sandbox.json"

// LauncherCommand は方針をかけてからコマンドを起動する隠しサブコマンド名。
const LauncherCommand = "__sandbox"

// Policy は VM の中でコマンドにかける制限。
type Policy struct {
	// Enabled を false にすると何もしない。省略 (null) なら有効。
	Enabled *bool `json:"enabled,omitempty"`
	// Mode は "compat" (既定) か "strict"。
	//   - compat: rootless docker / opencode / claude を壊さない控えめな拒否。
	//   - strict: mount / unshare / io_uring / perf なども拒否する。
	Mode string `json:"mode,omitempty"`
	// ExtraDeny は追加で拒否する syscall 名 (例 "chroot")。
	ExtraDeny []string `json:"extra_deny,omitempty"`
	// Landlock を true にすると、書き込み・作成・削除などのファイル操作を
	// ReadWritePaths の下だけに限る (読み取りは制限しない)。
	Landlock bool `json:"landlock,omitempty"`
	// ReadWritePaths は Landlock で書き込みを許すパス。空なら既定
	// (ホーム・/work・/tmp・/var/tmp・/run/user/<uid>・/dev/shm)。
	ReadWritePaths []string `json:"read_write_paths,omitempty"`
}

// Default は設定が無いときに使う方針。compat で有効。
func Default() *Policy { return &Policy{Enabled: boolPtr(true), Mode: "compat"} }

// On は方針が有効かを返す。Enabled が省略 (null) なら有効とみなす。
func (p *Policy) On() bool { return p != nil && (p.Enabled == nil || *p.Enabled) }

func boolPtr(b bool) *bool { return &b }

// Load は path から方針を読む。ファイルが無ければ (nil, nil)。
func Load(path string) (*Policy, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p Policy
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &p, nil
}

// JSON は host が cloud-init で VM へ渡す中身を返す。
func (p *Policy) JSON() ([]byte, error) { return json.MarshalIndent(p, "", "  ") }

func (p *Policy) validate() error {
	switch p.Mode {
	case "", "compat", "strict":
		return nil
	default:
		return fmt.Errorf("sandbox.mode は compat / strict のどれか: %q", p.Mode)
	}
}

// Validate は方針を検査する (mode と syscall 名)。host が VM を起動する前に呼ぶ。
func (p *Policy) Validate() error {
	if err := p.validate(); err != nil {
		return err
	}
	_, err := p.DenyNumbers()
	return err
}

// compatDeny は rootless docker / opencode / claude を壊さずに拒否できる、
// 古典的な攻撃面・永続化の入口。これらを使う普通の開発作業は無い。
var compatDeny = []string{
	"bpf",
	"init_module", "finit_module", "delete_module",
	"kexec_load", "kexec_file_load",
	"reboot", "swapon", "swapoff",
	"ptrace",
	"userfaultfd", "kcmp",
	"add_key", "keyctl", "request_key",
	"name_to_handle_at", "open_by_handle_at",
}

// strictDeny は compat に加えて拒否するもの。rootless docker のデーモンは
// agent のプロセス木の外 (systemd) にいるので通常は影響しないが、agent が直接
// unshare / bwrap / io_uring を使う作業はできなくなる。
var strictDeny = []string{
	"mount", "umount2", "pivot_root", "chroot",
	"setns", "unshare",
	"acct", "quotactl", "lookup_dcookie",
	"perf_event_open", "process_vm_readv", "process_vm_writev",
	"io_uring_setup", "io_uring_enter", "io_uring_register",
	"mount_setattr", "move_mount", "fsopen", "fsconfig", "fsmount", "open_tree",
}

// denyNames は方針で拒否する syscall 名を返す (重複は除く)。
func (p *Policy) denyNames() []string {
	names := append([]string{}, compatDeny...)
	if p.Mode == "strict" {
		names = append(names, strictDeny...)
	}
	names = append(names, p.ExtraDeny...)
	return names
}

// DenyNumbers は拒否する syscall 番号を返す。未知の名前はエラー (書いた方針を
// 黙って無視しない)。
func (p *Policy) DenyNumbers() ([]int, error) {
	seen := map[int]bool{}
	var out []int
	for _, n := range p.denyNames() {
		nr, ok := syscallNumbers[n]
		if !ok {
			return nil, fmt.Errorf("未知の syscall 名: %q", n)
		}
		if !seen[nr] {
			seen[nr] = true
			out = append(out, nr)
		}
	}
	return out, nil
}

// syscallNumbers は設定で書ける syscall 名 -> 番号。linux の共通番号。
var syscallNumbers = map[string]int{
	"bpf":               unix.SYS_BPF,
	"init_module":       unix.SYS_INIT_MODULE,
	"finit_module":      unix.SYS_FINIT_MODULE,
	"delete_module":     unix.SYS_DELETE_MODULE,
	"kexec_load":        unix.SYS_KEXEC_LOAD,
	"kexec_file_load":   unix.SYS_KEXEC_FILE_LOAD,
	"reboot":            unix.SYS_REBOOT,
	"swapon":            unix.SYS_SWAPON,
	"swapoff":           unix.SYS_SWAPOFF,
	"ptrace":            unix.SYS_PTRACE,
	"userfaultfd":       unix.SYS_USERFAULTFD,
	"kcmp":              unix.SYS_KCMP,
	"add_key":           unix.SYS_ADD_KEY,
	"keyctl":            unix.SYS_KEYCTL,
	"request_key":       unix.SYS_REQUEST_KEY,
	"name_to_handle_at": unix.SYS_NAME_TO_HANDLE_AT,
	"open_by_handle_at": unix.SYS_OPEN_BY_HANDLE_AT,
	"perf_event_open":   unix.SYS_PERF_EVENT_OPEN,
	"process_vm_readv":  unix.SYS_PROCESS_VM_READV,
	"process_vm_writev": unix.SYS_PROCESS_VM_WRITEV,
	"io_uring_setup":    unix.SYS_IO_URING_SETUP,
	"io_uring_enter":    unix.SYS_IO_URING_ENTER,
	"io_uring_register": unix.SYS_IO_URING_REGISTER,
	"mount":             unix.SYS_MOUNT,
	"umount2":           unix.SYS_UMOUNT2,
	"pivot_root":        unix.SYS_PIVOT_ROOT,
	"chroot":            unix.SYS_CHROOT,
	"setns":             unix.SYS_SETNS,
	"unshare":           unix.SYS_UNSHARE,
	"acct":              unix.SYS_ACCT,
	"quotactl":          unix.SYS_QUOTACTL,
	"lookup_dcookie":    unix.SYS_LOOKUP_DCOOKIE,
	"mount_setattr":     unix.SYS_MOUNT_SETATTR,
	"move_mount":        unix.SYS_MOVE_MOUNT,
	"fsopen":            unix.SYS_FSOPEN,
	"fsconfig":          unix.SYS_FSCONFIG,
	"fsmount":           unix.SYS_FSMOUNT,
	"open_tree":         unix.SYS_OPEN_TREE,
	"getppid":           unix.SYS_GETPPID, // 動作確認用
}

// Apply は no_new_privs を立て、Landlock と seccomp をこのスレッドにかける。
// 戻ってきたあとは、このスレッドで起動するコマンドすべてに効く。
func (p *Policy) Apply() error {
	if !p.On() {
		return nil
	}
	if err := p.validate(); err != nil {
		return err
	}
	// Landlock も seccomp もスレッド単位。exec を呼ぶこのスレッドに固定する。
	runtime.LockOSThread()
	// no_new_privs は Landlock / seccomp を非特権でかける前提。一度立てると戻せない。
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs を立てられない: %w", err)
	}
	if p.Landlock {
		if err := applyLandlock(p.readWritePaths()); err != nil {
			return err
		}
	}
	deny, err := p.DenyNumbers()
	if err != nil {
		return err
	}
	if len(deny) > 0 {
		if err := applySeccomp(deny); err != nil {
			return err
		}
	}
	return nil
}

// readWritePaths は Landlock で書き込みを許すパス (未指定なら既定)。
func (p *Policy) readWritePaths() []string {
	if len(p.ReadWritePaths) > 0 {
		return p.ReadWritePaths
	}
	var paths []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(paths, home)
	}
	paths = append(paths, "/work", "/tmp", "/var/tmp", "/dev/shm")
	paths = append(paths, fmt.Sprintf("/run/user/%d", os.Getuid()))
	return paths
}

// Run は隠しサブコマンド `__sandbox` の本体。方針をかけてから target を起動する。
// argv は ["--", target, ...] か [target, ...]。
func Run(argv []string) error { return RunWith(argv, ConfigPath) }

// RunWith は方針のパスを指定できる Run (テスト用)。
func RunWith(argv []string, path string) error {
	if len(argv) > 0 && argv[0] == "--" {
		argv = argv[1:]
	}
	if len(argv) == 0 {
		return errors.New("起動するコマンドが無い")
	}
	p, err := Load(path)
	if err != nil {
		return err
	}
	if p != nil {
		if err := p.Apply(); err != nil {
			return fmt.Errorf("sandbox: %w", err)
		}
	}
	target := argv[0]
	if !strings.ContainsRune(target, '/') {
		if target, err = exec.LookPath(argv[0]); err != nil {
			return err
		}
	}
	return syscall.Exec(target, argv, os.Environ())
}

// Wrap は argv を、方針をかける起動役 (自分自身) 経由に包む。self は quagent
// 自身のパス (os.Executable())。
func Wrap(self string, argv []string) []string {
	out := make([]string, 0, len(argv)+3)
	out = append(out, self, LauncherCommand, "--")
	return append(out, argv...)
}
