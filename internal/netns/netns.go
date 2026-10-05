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
	"sync"
	"syscall"
	"time"
)

// ChildCommand は再実行時の隠しサブコマンド名。
const ChildCommand = "__netns"

// Spec はランチャに渡す設定。
type Spec struct {
	WorkDir string `json:"work_dir"`
	// SSHPort が 0 でなければ host 127.0.0.1 で待ち受け、子 netns の同ポートへ中継する
	// (ssh を使うときだけ)。
	SSHPort int `json:"ssh_port,omitempty"`
	// DNS は上流のリゾルバ。子 netns 内の DNS サーバーが許可ドメインの問い合わせだけ転送する。
	DNS string `json:"dns"`
	// Allow は最初から期限なしで許可するドメインのパターン。
	Allow []string `json:"allow"`
	// InspectHTTPS は許可した行き先への TLS を終端し、平文の HTTP を親の内容
	// ガードにかけてから転送する。CACertPEM / CAKeyPEM はその使い捨て CA。
	InspectHTTPS bool   `json:"inspect_https,omitempty"`
	CACertPEM    string `json:"ca_cert_pem,omitempty"`
	CAKeyPEM     string `json:"ca_key_pem,omitempty"`
	// InspectLimit は 1 リクエストで点検のために読む本文の上限 (バイト)。
	InspectLimit int `json:"inspect_limit,omitempty"`
	// QemuArgv は子 netns 内で実行する qemu のコマンドライン。
	QemuArgv []string `json:"qemu_argv"`
}

// Launcher は host 側から見たランチャプロセス。
type Launcher struct {
	spec  Spec
	cmd   *exec.Cmd
	stdin io.WriteCloser
	done  chan error

	// Denied には許可外として名前解決を拒否したドメインが流れる (取りこぼしは捨てる)。
	Denied chan string
	// Blocked には透明プロキシが許可外の名前 (SNI/Host) で止めた Web 接続が流れる。
	Blocked chan string

	mu      sync.Mutex
	seq     int
	waiters map[int]chan struct{}

	// inspect は子が TLS 終端して取り出した HTTPS リクエストを点検する (親側)。
	// SetInspector で差す。nil のときは止める側 (点検できない)。
	inspect func(InspectRequest) error
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
	cmd.Stderr = logf
	events, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// 端末の Ctrl-C で巻き添えにならないよう別プロセスグループにし、後始末は親が握る。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	l := &Launcher{
		spec: spec, cmd: cmd, stdin: stdin, done: make(chan error, 1),
		Denied: make(chan string, 64), Blocked: make(chan string, 64), waiters: map[int]chan struct{}{},
	}
	evDone := make(chan struct{})
	go func() {
		defer close(evDone)
		dec := json.NewDecoder(events)
		for {
			var ev Event
			if err := dec.Decode(&ev); err != nil {
				return
			}
			l.handleEvent(ev)
		}
	}()
	go func() {
		<-evDone
		l.done <- cmd.Wait()
	}()
	return l, nil
}

func (l *Launcher) handleEvent(ev Event) {
	if ev.Applied != 0 {
		l.mu.Lock()
		if ch, ok := l.waiters[ev.Applied]; ok {
			close(ch)
			delete(l.waiters, ev.Applied)
		}
		l.mu.Unlock()
	}
	if ev.Denied != "" {
		select {
		case l.Denied <- ev.Denied:
		default:
		}
	}
	if ev.Blocked != "" {
		select {
		case l.Blocked <- ev.Blocked:
		default:
		}
	}
	if ev.Inspect != nil {
		// 点検はローカル LLM や人間の判断を待って長くかかるので、イベントの読み取り
		// (許可の反映待ち) を止めないよう別 goroutine で行う。
		go l.runInspect(ev.Inspect, ev.InspectID)
	}
}

// SetInspector は子が TLS 終端して取り出した HTTPS リクエストを点検する関数を設定する。
// 通すなら nil、止めるなら理由を返す。設定するまでは止める側に倒れる。
func (l *Launcher) SetInspector(fn func(InspectRequest) error) {
	l.mu.Lock()
	l.inspect = fn
	l.mu.Unlock()
}

// runInspect は 1 件の点検依頼を処理し、結果を子へ返す。
func (l *Launcher) runInspect(req *InspectRequest, id int) {
	l.mu.Lock()
	fn := l.inspect
	l.mu.Unlock()
	var err error
	if fn == nil {
		err = fmt.Errorf("内容ガードが設定されていない")
	} else {
		err = fn(*req)
	}
	reason := ""
	if err != nil {
		reason = err.Error()
	}
	_ = l.sendControl(control{InspectID: id, Allow: err == nil, Reason: reason})
}

// sendControl は 1 つの control を子へ送る (許可の差し替えと点検の返答で使う)。
func (l *Launcher) sendControl(c control) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err = l.stdin.Write(append(b, '\n'))
	return err
}

// SetGrants は許可の一覧を丸ごと差し替え、nft に反映されるまで待つ。
func (l *Launcher) SetGrants(gs []Grant) error {
	l.mu.Lock()
	l.seq++
	seq := l.seq
	ch := make(chan struct{})
	l.waiters[seq] = ch
	b, err := json.Marshal(control{Seq: seq, Grants: gs})
	if err == nil {
		_, err = l.stdin.Write(append(b, '\n'))
	}
	l.mu.Unlock()
	if err != nil {
		return fmt.Errorf("ランチャへの送信に失敗: %w", err)
	}
	select {
	case <-ch:
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("ランチャが許可の反映に応答しない")
	}
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
	l.mu.Lock()
	l.stdin.Close()
	l.mu.Unlock()
	select {
	case <-l.done:
		return
	case <-time.After(15 * time.Second):
	}
	// 応答しなければプロセスグループごと殺す。子は Pdeathsig で道連れになる。
	_ = syscall.Kill(-l.cmd.Process.Pid, syscall.SIGKILL)
	<-l.done
}
