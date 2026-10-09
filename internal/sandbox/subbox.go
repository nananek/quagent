package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// MaskSensitivePaths は指定ディレクトリ (home) 配下の paths を不可視化する。
// ディレクトリは空の tmpfs でマウントし、通常ファイルは /dev/null をバインドマウントする。
func MaskSensitivePaths(home string, paths []string) error {
	if home == "" {
		return nil
	}
	// マウント名前空間が親と共有 (MS_SHARED) されている場合、
	// 非特権でのマウント伝播が拒否されるため、マウント名前空間をプライベート化する
	_ = syscall.Mount("none", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, "")

	for _, rel := range paths {
		if rel == "" {
			continue
		}
		target := filepath.Join(home, rel)
		st, err := os.Stat(target)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("パスの確認に失敗 (%s): %w", target, err)
		}
		if st.IsDir() {
			if err := syscall.Mount("tmpfs", target, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, ""); err != nil {
				return fmt.Errorf("tmpfs のマウントに失敗 (%s): %w", target, err)
			}
		} else {
			if err := syscall.Mount("/dev/null", target, "", syscall.MS_BIND|syscall.MS_RDONLY, ""); err != nil {
				return fmt.Errorf("ファイルのマスキングに失敗 (%s): %w", target, err)
			}
		}
	}
	return nil
}

// RunSubbox はサブプロセスのファイルシステム不可視化を行いコマンドを起動する。
// ConfigPath の方針を参照する。
func RunSubbox(argv []string) error {
	return RunSubboxWith(argv, ConfigPath)
}

// RunSubboxWith は方針のパスを指定できる RunSubbox (テスト用)。
func RunSubboxWith(argv []string, path string) error {
	if len(argv) > 0 && argv[0] == "--" {
		argv = argv[1:]
	}
	if len(argv) == 0 {
		return errors.New("起動するコマンドが無い")
	}

	// 1. 名前空間内にまだ入っていない場合: ユーザー & マウント名前空間を新設して再実行
	if os.Getenv("QUAGENT_SUBBOX_NS") != "1" {
		self, err := os.Executable()
		if err != nil {
			return fmt.Errorf("自分のパスを取得できない: %w", err)
		}
		subboxArgs := append([]string{LauncherCommandSubbox, "--"}, argv...)
		cmd := exec.Command(self, subboxArgs...)
		cmd.Env = append(os.Environ(), "QUAGENT_SUBBOX_NS=1", "QUAGENT_SUBBOX_ACTIVE=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
			UidMappings: []syscall.SysProcIDMap{
				{ContainerID: 0, HostID: os.Getuid(), Size: 1},
			},
			GidMappings: []syscall.SysProcIDMap{
				{ContainerID: 0, HostID: os.Getgid(), Size: 1},
			},
		}
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				os.Exit(ee.ExitCode())
			}
			return err
		}
		return nil
	}

	// 2. 名前空間内: 機密パスを不可視化し、方針 (seccomp/Landlock) をかけて exec
	p, err := Load(path)
	if err != nil {
		return err
	}
	if p != nil && p.On() {
		if p.MaskAgentConfigOn() {
			home, err := os.UserHomeDir()
			if err == nil && home != "" {
				_ = MaskSensitivePaths(home, SensitiveAgentConfigPaths)
			}
		}
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
