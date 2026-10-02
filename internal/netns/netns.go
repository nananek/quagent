// Package netns は qemu を host 側 egress 制限つきの unprivileged netns で動かす。
//
// quagent 自身を `unshare -Urm` で再実行し (user + mount ns)、その中に子 netns を
// 作って qemu を入れる。qemu が外へ張る socket は子 netns の nftables を通る。
// ルールは qemu の外側にあるので guest の root からは見えず、改変もできない。
//
//	host ── slirp4netns add_hostfwd ──► 子 netns (qemu hostfwd) ──► guest:22
//	        qemu ── tap0 ──► slirp4netns ──► host (uplink)
//	        └ nftables (子 netns): DNS と allow set 以外を reject
package netns

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// ChildCommand は再実行時の隠しサブコマンド名。
const ChildCommand = "__netns"

// Spec はランチャに渡す設定。
type Spec struct {
	WorkDir string `json:"work_dir"`
	// SSHPort は host 127.0.0.1 で待ち受け、子 netns の同ポートへ中継する。
	SSHPort int `json:"ssh_port"`
	// DNS は host の実 IPv4 リゾルバ。netns と guest の両方がこれを使う。
	DNS string `json:"dns"`
	// Allow は egress を許すドメイン (TCP/UDP 443)。
	Allow []string `json:"allow"`
	// QemuArgv は子 netns 内で実行する qemu のコマンドライン。
	QemuArgv []string `json:"qemu_argv"`
}

// Launcher は host 側から見たランチャプロセス。
type Launcher struct {
	spec  Spec
	cmd   *exec.Cmd
	stdin io.WriteCloser
	done  chan error
}

func (s Spec) file(name string) string { return filepath.Join(s.WorkDir, name) }

// Start はランチャを起動する。ログは WorkDir/launcher.log に出る。
func Start(spec Spec) (*Launcher, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	specPath := spec.file("netns.json")
	b, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(specPath, b, 0o600); err != nil {
		return nil, err
	}
	logf, err := os.Create(spec.file("launcher.log"))
	if err != nil {
		return nil, err
	}
	defer logf.Close()

	cmd := exec.Command("unshare", "-Urm", self, ChildCommand, specPath)
	cmd.Stdout = logf
	cmd.Stderr = logf
	// 端末の Ctrl-C で巻き添えにならないよう別プロセスグループにし、後始末は親が握る。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	l := &Launcher{spec: spec, cmd: cmd, stdin: stdin, done: make(chan error, 1)}
	go func() { l.done <- cmd.Wait() }()
	return l, nil
}

// Failed はランチャが既に終了していればその理由を返す。
func (l *Launcher) Failed() error {
	select {
	case err := <-l.done:
		l.done <- err
		return fmt.Errorf("VM ランチャが終了した (%v)。ログ: %s", err, l.spec.file("launcher.log"))
	default:
		return nil
	}
}

// WaitReady は qemu が起動するまで待つ。
func (l *Launcher) WaitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(l.spec.file("ready")); err == nil {
			return nil
		}
		if err := l.Failed(); err != nil {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("VM ランチャが %s 以内に準備できなかった。ログ: %s", timeout, l.spec.file("launcher.log"))
}

// Stop はランチャに終了を伝え (stdin を閉じる)、qemu ごと止める。
func (l *Launcher) Stop() {
	l.stdin.Close()
	select {
	case <-l.done:
		return
	case <-time.After(15 * time.Second):
	}
	// 応答しなければプロセスグループごと殺す。子は Pdeathsig で道連れになる。
	_ = syscall.Kill(-l.cmd.Process.Pid, syscall.SIGKILL)
	<-l.done
}
