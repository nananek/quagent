package guest

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
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
)

// Serve は VM 内で vsock を待ち受け、host からのコマンドを実行する (`quagent __guest`)。
// 実行するのはこのプロセスのユーザー (作業用の一般ユーザー) の権限。
func Serve() error {
	l, err := vsock.Listen(Port, nil)
	if err != nil {
		return err
	}
	log.Printf("vsock :%d で待ち受け", Port)
	return serve(l)
}

func serve(l net.Listener) error {
	env := baseEnv()
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go handle(c, env)
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
	// /etc/environment (DOCKER_HOST など)
	if b, err := os.ReadFile("/etc/environment"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			k, v, ok := strings.Cut(line, "=")
			if !ok || strings.HasPrefix(line, "#") {
				continue
			}
			vars[k] = strings.Trim(v, `"'`)
		}
	}
	env := make([]string, 0, len(vars))
	for k, v := range vars {
		env = append(env, k+"="+v)
	}
	return env
}

func handle(c net.Conn, env []string) {
	defer c.Close()
	br := bufio.NewReader(c)
	fw := &frameWriter{w: c}
	line, err := br.ReadBytes('\n')
	if err != nil {
		return
	}
	var h Header
	if err := json.Unmarshal(line, &h); err != nil || len(h.Argv) == 0 {
		_ = fw.write(fError, []byte("不正なヘッダ"))
		return
	}
	cmd := exec.Command(h.Argv[0], h.Argv[1:]...)
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
