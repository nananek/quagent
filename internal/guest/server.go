package guest

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"syscall"

	"github.com/creack/pty"
	"github.com/mdlayher/vsock"
	"github.com/nananek/quagent/internal/sandbox"
)

// SandboxPolicy は VM の中でコマンドにかける一枚 (seccomp / Landlock) の方針。
// Serve が設定ファイルから読み、handle が起動のたびに起動役へ包む。テストでは nil。
var SandboxPolicy *sandbox.Policy

// GuestDockerPort は guest 内で Docker レジストリキャッシュの中継を待ち受けるポート。
const GuestDockerPort = 5000

// Serve は VM 内で vsock を待ち受け、host からのコマンドを実行する (`quagent __guest`)。
// 実行するのはこのプロセスのユーザー (作業用の一般ユーザー) の権限。あわせて
// 127.0.0.1:relayPort への接続を host の窓口 (vsock の hostPort) へ中継する。
// hostDockerPort が 0 でなければ、127.0.0.1:GuestDockerPort への接続を host の
// Docker キャッシュ窓口 (vsock の hostDockerPort) へ中継する。
// SandboxPolicy が有効なら、host から来たコマンドを起動役 (`__sandbox`) 経由で起動し、
// 本人には外せない seccomp / Landlock をかける。
func Serve(relayPort int, hostPort uint32, hostDockerPort uint32) error {
	policy, err := sandbox.Load(sandbox.ConfigPath)
	if err != nil {
		return fmt.Errorf("sandbox の方針を読めない: %w", err)
	}
	SandboxPolicy = policy
	if policy.On() {
		deny, err := policy.DenyNumbers()
		if err != nil {
			return err
		}
		log.Printf("sandbox: 有効 mode=%s (syscall を %d 個拒否, landlock=%v)", policy.Mode, len(deny), policy.Landlock)
	} else if policy == nil {
		// host が置いたつもりで置けていない場合に、黙って無効にしない。
		log.Printf("sandbox: 方針ファイル %s が無いので無効", sandbox.ConfigPath)
	}
	l, err := vsock.Listen(Port, nil)
	if err != nil {
		return err
	}
	log.Printf("vsock :%d で待ち受け", Port)
	if hostPort != 0 {
		rl, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", relayPort))
		if err != nil {
			return err
		}
		log.Printf("127.0.0.1:%d -> host の窓口 (vsock :%d) を中継", relayPort, hostPort)
		go relay(rl, func() (net.Conn, error) { return vsock.Dial(vsock.Host, hostPort, nil) })
	}
	if hostDockerPort != 0 {
		dl, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", GuestDockerPort))
		if err == nil {
			log.Printf("127.0.0.1:%d -> host の Docker キャッシュ (vsock :%d) を中継", GuestDockerPort, hostDockerPort)
			go relaySimple(dl, func() (net.Conn, error) { return vsock.Dial(vsock.Host, hostDockerPort, nil) })
		} else {
			log.Printf("Docker キャッシュ中継の待ち受けに失敗: %v", err)
		}
	}
	return serve(&hostOnly{l})
}


// hostOnly は host (CID 2) 以外からの接続を切る。ほかの VM や、VM の中から自分自身への
// vsock の接続はここで落とす。
type hostOnly struct{ net.Listener }

func (l *hostOnly) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if a, ok := c.RemoteAddr().(*vsock.Addr); ok && a.ContextID == vsock.Host {
			return c, nil
		}
		log.Printf("vsock host 以外からの接続を拒否: remote=%v", c.RemoteAddr())
		c.Close()
	}
}

// relay は l への接続を dial 先へそのまま中継する (同時 64 本まで)。
func relay(l net.Listener, dial func() (net.Conn, error)) {
	sem := make(chan struct{}, 64)
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		leader := GetSessionPID()
		if leader <= 0 {
			log.Printf("relay: セッションリーダー未設定のため窓口接続を遮断 (remote=%v)", c.RemoteAddr())
			c.Close()
			continue
		}
		callerPID, err := CallerPID(c.RemoteAddr())
		if err != nil {
			log.Printf("relay: 接続元プロセスの特定に失敗したため拒否 (remote=%v): %v", c.RemoteAddr(), err)
			c.Close()
			continue
		}
		if !IsAllowedCaller(procDir, callerPID, leader) {
			log.Printf("relay: 未許可プロセス (PID %d) からの窓口接続を遮断 (sessionLeader=%d)", callerPID, leader)
			c.Close()
			continue
		}
		select {
		case sem <- struct{}{}:
		default:
			c.Close()
			continue
		}
		go func() {
			defer func() { <-sem }()
			defer c.Close()
			up, err := dial()
			if err != nil {
				return
			}
			defer up.Close()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(up, c); closeWrite(up); done <- struct{}{} }()
			go func() { _, _ = io.Copy(c, up); closeWrite(c); done <- struct{}{} }()
			<-done
			<-done
		}()
	}
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// relaySimple は l への接続を dial 先へそのまま中継する (同時 64 本まで、PID 検証なし)。
// rootless dockerd 等、エージェントツリー外のプロセスからの通信を中継するために使用する。
func relaySimple(l net.Listener, dial func() (net.Conn, error)) {
	sem := make(chan struct{}, 64)
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		select {
		case sem <- struct{}{}:
		default:
			c.Close()
			continue
		}
		go func() {
			defer func() { <-sem }()
			defer c.Close()
			up, err := dial()
			if err != nil {
				return
			}
			defer up.Close()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(up, c); closeWrite(up); done <- struct{}{} }()
			go func() { _, _ = io.Copy(c, up); closeWrite(c); done <- struct{}{} }()
			<-done
			<-done
		}()
	}
}


// maxGuestConns は VM 内の受け口で同時に受け付けるコマンド接続の上限。
const maxGuestConns = 64

func serve(l net.Listener) error {
	env := baseEnv()
	sem := make(chan struct{}, maxGuestConns)
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		select {
		case sem <- struct{}{}:
			go func() {
				defer func() { <-sem }()
				handle(c, env)
			}()
		default:
			log.Printf("vsock 同時接続上限 (%d) 超過のため拒否: remote=%v", maxGuestConns, c.RemoteAddr())
			c.Close()
		}
	}
}

// baseEnv はログインに近い環境変数を作る (systemd から起動されるので最小限しか無い)。
func baseEnv() []string {
	vars := map[string]string{
		"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"LANG": "C.UTF-8",
	}
	if u, err := user.Current(); err == nil {
		vars["HOME"] = u.HomeDir
		vars["USER"] = u.Username
		vars["LOGNAME"] = u.Username
		vars["XDG_RUNTIME_DIR"] = "/run/user/" + u.Uid
	}
	vars["SHELL"] = "/bin/bash"
	// システムのロケール (Arch は /etc/locale.conf、Debian は /etc/default/locale)
	for _, path := range []string{"/etc/default/locale", "/etc/locale.conf"} {
		readEnvFile(path, vars, func(k string) bool { return k == "LANG" || strings.HasPrefix(k, "LC_") })
	}
	// /etc/environment (DOCKER_HOST など)
	readEnvFile("/etc/environment", vars, func(string) bool { return true })
	env := make([]string, 0, len(vars))
	for k, v := range vars {
		env = append(env, k+"="+v)
	}
	return env
}

// readEnvFile は KEY=VALUE の行が並ぶファイルから、want に当たる変数を vars に入れる。
func readEnvFile(path string, vars map[string]string, want func(string) bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") || !want(k) {
			continue
		}
		vars[k] = strings.Trim(v, `"'`)
	}
}

func handle(c net.Conn, env []string) {
	defer c.Close()
	br := bufio.NewReader(c)
	fw := &frameWriter{w: c}
	line, err := readLine(br, maxFrame)
	if err != nil {
		return
	}
	var h Header
	if err := json.Unmarshal(line, &h); err != nil || len(h.Argv) == 0 {
		_ = fw.write(fError, []byte("不正なヘッダ"))
		return
	}
	argv := h.Argv
	if SandboxPolicy.On() {
		self, err := os.Executable()
		if err != nil {
			_ = fw.write(fError, []byte("sandbox: 自分のパスを取れない: "+err.Error()))
			return
		}
		argv = sandbox.Wrap(self, argv)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = h.Dir
	cmd.Env = env
	if h.Dir == "" {
		cmd.Dir, _ = os.UserHomeDir()
	}
	if h.TTY {
		term := h.Term
		if term == "" {
			term = "xterm-256color"
		}
		cmd.Env = append(cmd.Env, "TERM="+term)
		runTTY(cmd, h, br, fw)
	} else {
		runPipe(cmd, br, fw)
	}
}

// readLine は改行までを読む。max バイトを超えたらエラー (改行の無い送りつけで膨らませない)。
func readLine(br *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(line)+len(chunk) > max {
			return nil, errors.New("ヘッダが長すぎる")
		}
		line = append(line, chunk...)
		if err != bufio.ErrBufferFull {
			return line, err
		}
	}
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	if err != nil {
		return 255
	}
	return 0
}

func sendExit(fw *frameWriter, code int) {
	var p [4]byte
	binary.BigEndian.PutUint32(p[:], uint32(int32(code)))
	_ = fw.write(fExit, p[:])
}

func runPipe(cmd *exec.Cmd, br *bufio.Reader, fw *frameWriter) {
	// 接続が切れたらプロセスグループごと止める
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = fw.write(fError, []byte(err.Error()))
		return
	}
	cmd.Stdout = frameStream{fw, fStdout}
	cmd.Stderr = frameStream{fw, fStderr}
	if err := cmd.Start(); err != nil {
		_ = fw.write(fError, []byte(err.Error()))
		return
	}
	go func() {
		for {
			t, p, err := readFrame(br)
			if err != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				return
			}
			switch t {
			case fStdin:
				_, _ = stdin.Write(p)
			case fStdinEOF:
				_ = stdin.Close()
			}
		}
	}()
	sendExit(fw, exitCode(cmd.Wait()))
}

func runTTY(cmd *exec.Cmd, h Header, br *bufio.Reader, fw *frameWriter) {
	size := &pty.Winsize{Rows: h.Rows, Cols: h.Cols}
	if size.Rows == 0 || size.Cols == 0 {
		size = &pty.Winsize{Rows: 24, Cols: 80}
	}
	f, err := pty.StartWithSize(cmd, size)
	if err != nil {
		_ = fw.write(fError, []byte(err.Error()))
		return
	}
	defer f.Close()
	SetSessionPID(cmd.Process.Pid)
	defer SetSessionPID(0)
	go func() {
		for {
			t, p, err := readFrame(br)
			if err != nil {
				// pty の session leader に HUP を送る (端末を閉じたのと同じ)
				_ = cmd.Process.Signal(syscall.SIGHUP)
				return
			}
			switch t {
			case fStdin:
				_, _ = f.Write(p)
			case fResize:
				if len(p) == 4 {
					_ = pty.Setsize(f, &pty.Winsize{
						Rows: binary.BigEndian.Uint16(p[0:2]),
						Cols: binary.BigEndian.Uint16(p[2:4]),
					})
				}
			}
		}
	}()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(frameStream{fw, fStdout}, f)
		close(done)
	}()
	err = cmd.Wait()
	<-done
	sendExit(fw, exitCode(err))
}
