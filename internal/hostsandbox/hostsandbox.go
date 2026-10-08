// Package hostsandbox はホスト側で動作する QEMU プロセスに多層の
// サンドボックス (名前空間分離、ディレクトリの最小化・ホワイトリスト化、
// Landlock、Seccomp、リソース制限) を適用する。
package hostsandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/nananek/quagent/internal/sandbox"
	"golang.org/x/sys/unix"
)

// LauncherCommand はホスト側 QEMU をサンドボックス内で起動する隠しサブコマンド名。
const LauncherCommand = "__host_sandbox"

// HostQemuDenySyscalls は QEMU プロセスに対してホスト側で拒否する危険なシステムコール。
// QEMU が通常動作 (KVM/vhost-vsock/slirp) で使わないカーネル攻撃・脱出・監視用システムコール群。
var HostQemuDenySyscalls = []string{
	// カーネル攻撃・永続化・LPE 経路
	"bpf",
	"init_module", "finit_module", "delete_module",
	"kexec_load", "kexec_file_load",
	"reboot", "swapon", "swapoff",
	"ptrace",
	"userfaultfd", "kcmp",
	"add_key", "keyctl", "request_key",
	"name_to_handle_at", "open_by_handle_at",
	// 名前空間・マウント脱出
	"mount", "umount2", "pivot_root", "chroot",
	"setns", "unshare",
	"mount_setattr", "move_mount", "fsopen", "fsconfig", "fsmount", "open_tree",
	// プロセス間干渉・監視
	"process_vm_readv", "process_vm_writev",
	"acct", "quotactl", "lookup_dcookie",
	"perf_event_open",
	// io_uring
	"io_uring_setup", "io_uring_enter", "io_uring_register",
}

// SensitiveDirCandidates はホームディレクトリのホワイトリスト化が使えない場合の
// フォールバック用マスキング対象ディレクトリ (相対パス)。
var SensitiveDirCandidates = []string{
	".ssh",
	".gnupg",
	".aws",
	".azure",
	".kube",
	".docker",
	".config/gh",
	".config/gcloud",
	".config/op",
	".claude",
	".gemini",
}

// IsSubpath は child が parent の配下にあるか調べ、相対パスと true を返す。
func IsSubpath(parent, child string) (string, bool) {
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)
	if parent == child {
		return ".", true
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return "", false
	}
	return rel, true
}

// ApplyResourceLimits は QEMU プロセスのリソース制限を設定する。
// コアダンプ (RLIMIT_CORE) を 0 にして機密情報漏洩とディスク枯渇を防ぐ。
func ApplyResourceLimits() error {
	var rlim unix.Rlimit
	rlim.Cur = 0
	rlim.Max = 0
	if err := unix.Setrlimit(unix.RLIMIT_CORE, &rlim); err != nil {
		return fmt.Errorf("RLIMIT_CORE を 0 に設定できない: %w", err)
	}
	return nil
}

// ApplyHostSeccomp はホスト側 QEMU 用の seccomp フィルタを適用する。
func ApplyHostSeccomp() error {
	deny, err := sandbox.SyscallNumbers(HostQemuDenySyscalls)
	if err != nil {
		return fmt.Errorf("ホスト seccomp の syscall 番号解決に失敗: %w", err)
	}
	return sandbox.ApplySeccomp(deny)
}

// ApplyHostLandlock は QEMU の書き込み先を workDir・dataDisks と QEMU が動作に
// 要るデバイス・一時領域に限る。Landlock は読み取りを制限しないので、ベースimage・
// ファームウェア・共有ライブラリの読み取りはそのまま通る。書き込みだけが対象で、
// /dev/kvm (/dev/vhost-vsock) や /dev/null への書き込みを塞ぐと QEMU が
// "Permission denied" で起動できないため、明示的に許す。無いパスは飛ばす
// (explicit=false。KVM の無い環境等でも起動役自体は動く)。
func ApplyHostLandlock(workDir string, dataDisks []string) error {
	// /dev はディレクトリ単位で許す (Landlock の path_beneath はファイル単位の
	// 規則を受け付けないため、/dev/null などを 1 つずつ挙げると EINVAL になる)。
	// /dev の下への書き込みは DAC (所有者・パーミッション) でも抑えられるので、
	// QEMU が開けるのは /dev/kvm・/dev/vhost-vsock・/dev/null など、もともと
	// 開ける権限のあるものだけになる。
	paths := []string{workDir,
		"/tmp", "/var/tmp", "/dev", "/dev/shm", "/run",
	}
	for _, d := range dataDisks {
		if d == "" {
			continue
		}
		if _, ok := IsSubpath(workDir, d); ok {
			continue // workDir の下は既に許している (tmp.img など)
		}
		// Landlock の path_beneath はファイルに直接規則を足すと EINVAL になる
		// ので、ファイルなら親ディレクトリを許す。1 つでも EINVAL で返すと
		// Run が Landlock 全体をスキップ (fail-open) してしまうため。
		if st, err := os.Stat(d); err == nil && !st.IsDir() {
			paths = append(paths, filepath.Dir(d))
		} else {
			paths = append(paths, d)
		}
	}
	return sandbox.ApplyLandlock(paths, false)
}

// stageEntry はホワイトリストとしてステージングするパスとその属性。
type stageEntry struct {
	realPath string
	relPath  string
	readOnly bool
	isDir    bool
}

// IsolateHome はホームディレクトリ ($HOME) をホワイトリスト化し、
// QEMU が必要とするパス (ベースイメージ、作業ディレクトリ、追加ディスク) 以外の
// すべてのユーザーファイルを不可視化する。
func IsolateHome(home string, readOnlyPaths []string, readWritePaths []string) error {
	if home == "" {
		return nil
	}
	home = filepath.Clean(home)

	var entries []stageEntry
	for _, p := range readOnlyPaths {
		if p == "" {
			continue
		}
		if rel, ok := IsSubpath(home, p); ok && rel != "." {
			st, err := os.Stat(p)
			if err == nil {
				entries = append(entries, stageEntry{
					realPath: filepath.Clean(p),
					relPath:  rel,
					readOnly: true,
					isDir:    st.IsDir(),
				})
			}
		}
	}
	for _, p := range readWritePaths {
		if p == "" {
			continue
		}
		if rel, ok := IsSubpath(home, p); ok && rel != "." {
			st, err := os.Stat(p)
			if err == nil {
				entries = append(entries, stageEntry{
					realPath: filepath.Clean(p),
					relPath:  rel,
					readOnly: false,
					isDir:    st.IsDir(),
				})
			}
		}
	}

	// 1. ホーム配下に残すパスが無い場合: ホーム全体に空の読み取り専用 tmpfs を被せる
	if len(entries) == 0 {
		if err := syscall.Mount("tmpfs", home, "tmpfs", syscall.MS_NODEV|syscall.MS_NOEXEC|syscall.MS_NOSUID|syscall.MS_RDONLY, ""); err != nil {
			return maskSensitiveFallback(home)
		}
		return nil
	}

	// 2. 残すパスがある場合: 一時ステージング領域へバインド退避してからホームを tmpfs で覆い、
	//    残すパスだけを mount --move で元の相対位置へ戻す。
	stageDir, err := os.MkdirTemp("", "quagent-hstage-*")
	if err != nil {
		return maskSensitiveFallback(home)
	}
	defer os.RemoveAll(stageDir)

	stagePaths := make([]string, len(entries))
	for i, ent := range entries {
		stageItem := filepath.Join(stageDir, fmt.Sprintf("s%d", i))
		if ent.isDir {
			if err := os.MkdirAll(stageItem, 0o755); err != nil {
				return maskSensitiveFallback(home)
			}
		} else {
			f, err := os.Create(stageItem)
			if err != nil {
				return maskSensitiveFallback(home)
			}
			_ = f.Close()
		}
		flags := uintptr(syscall.MS_BIND)
		if err := syscall.Mount(ent.realPath, stageItem, "", flags, ""); err != nil {
			return maskSensitiveFallback(home)
		}
		if ent.readOnly {
			_ = syscall.Mount("", stageItem, "", syscall.MS_REMOUNT|syscall.MS_BIND|syscall.MS_RDONLY, "")
		}
		stagePaths[i] = stageItem
	}

	// ホームディレクトリ全体に tmpfs を被せる
	if err := syscall.Mount("tmpfs", home, "tmpfs", syscall.MS_NODEV|syscall.MS_NOSUID, ""); err != nil {
		return maskSensitiveFallback(home)
	}

	// 各ステージングエントリをホーム配下の元の位置へ戻す
	for i, ent := range entries {
		targetPath := filepath.Join(home, ent.relPath)
		parentDir := filepath.Dir(targetPath)
		if err := os.MkdirAll(parentDir, 0o755); err != nil {
			log.Printf("[quagent:hostsandbox] mkdir %s 失敗: %v", parentDir, err)
			continue
		}
		if ent.isDir {
			if err := os.MkdirAll(targetPath, 0o755); err != nil {
				log.Printf("[quagent:hostsandbox] mkdir target %s 失敗: %v", targetPath, err)
				continue
			}
		} else {
			f, err := os.Create(targetPath)
			if err == nil {
				_ = f.Close()
			}
		}
		if err := syscall.Mount(stagePaths[i], targetPath, "", syscall.MS_MOVE, ""); err != nil {
			log.Printf("[quagent:hostsandbox] move mount %s -> %s 失敗: %v", stagePaths[i], targetPath, err)
		}
	}

	return nil
}

// maskSensitiveFallback はホーム全体の tmpfs 被せに失敗した場合に、
// 既知の機密ディレクトリを個別に空 tmpfs で被せる。
func maskSensitiveFallback(home string) error {
	for _, rel := range SensitiveDirCandidates {
		target := filepath.Join(home, rel)
		if st, err := os.Stat(target); err == nil && st.IsDir() {
			_ = syscall.Mount("tmpfs", target, "tmpfs", syscall.MS_NODEV|syscall.MS_NOEXEC|syscall.MS_NOSUID|syscall.MS_RDONLY, "")
		}
	}
	return nil
}

// Spec は netns.Spec の一部フィールド (json 互換)。
type Spec struct {
	WorkDir            string   `json:"work_dir"`
	BaseDisk           string   `json:"base_disk,omitempty"`
	DataDiskPaths      []string `json:"data_disk_paths,omitempty"`
	DisableHostSandbox bool     `json:"disable_host_sandbox,omitempty"`
	QemuArgv           []string `json:"qemu_argv"`
}

// Run は隠しサブコマンド `__host_sandbox` のエントリポイント。
// specPath を読み込み、QEMU プロセスのサンドボックス環境を整えてから QEMU を exec する。
func Run(specPath string) error {
	runtime.LockOSThread()

	b, err := os.ReadFile(specPath)
	if err != nil {
		return fmt.Errorf("spec の読み込みに失敗: %w", err)
	}
	var spec Spec
	if err := json.Unmarshal(b, &spec); err != nil {
		return fmt.Errorf("spec のパースに失敗: %w", err)
	}

	if len(spec.QemuArgv) == 0 {
		return errors.New("qemu_argv が空です")
	}

	target := spec.QemuArgv[0]
	if !strings.ContainsRune(target, '/') {
		if target, err = exec.LookPath(spec.QemuArgv[0]); err != nil {
			return fmt.Errorf("qemu コマンド %q が見つからない: %w", spec.QemuArgv[0], err)
		}
	}

	if spec.DisableHostSandbox {
		return syscall.Exec(target, spec.QemuArgv, os.Environ())
	}

	// 1. リソース制限 (RLIMIT_CORE = 0)
	if err := ApplyResourceLimits(); err != nil {
		log.Printf("[quagent:hostsandbox] リソース制限の設定をスキップ: %v", err)
	}

	// 2. ディレクトリの最小化・ホワイトリスト化 (Mount Namespace)
	home, _ := os.UserHomeDir()
	if home != "" {
		roPaths := []string{}
		if spec.BaseDisk != "" {
			roPaths = append(roPaths, spec.BaseDisk)
		}
		// imagesDir も読み取り許可に含める
		imagesDir := filepath.Join(home, ".local/share/quagent/images")
		roPaths = append(roPaths, imagesDir)

		rwPaths := []string{spec.WorkDir}
		rwPaths = append(rwPaths, spec.DataDiskPaths...)

		if err := IsolateHome(home, roPaths, rwPaths); err != nil {
			log.Printf("[quagent:hostsandbox] ホーム隔離フォールバック: %v", err)
		}
	}

	// 3. no_new_privs (権限昇格の恒久防止。Landlock の restrict_self より先に立てる)
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		log.Printf("[quagent:hostsandbox] no_new_privs 設定スキップ: %v", err)
	}

	// 4. ファイルシステム書き込み制限 (Landlock。no_new_privs の後にかける)
	if err := ApplyHostLandlock(spec.WorkDir, spec.DataDiskPaths); err != nil {
		log.Printf("[quagent:hostsandbox] Landlock 適用スキップ (未対応カーネル等): %v", err)
	}

	// 5. ホスト事前適用 Seccomp
	if err := ApplyHostSeccomp(); err != nil {
		log.Printf("[quagent:hostsandbox] ホスト Seccomp 適用スキップ: %v", err)
	}

	// 6. QEMU を起動
	return syscall.Exec(target, spec.QemuArgv, os.Environ())
}
