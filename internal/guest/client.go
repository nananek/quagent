package guest

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mdlayher/vsock"
	"golang.org/x/term"
)

// ExitError はコマンドが 0 以外で終わったことを表す。
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("終了コード %d", e.Code) }

// Dial は VM (cid) の受け口に接続する。テストでは差し替える。
var Dial = func(cid uint32) (net.Conn, error) {
	return vsock.Dial(cid, Port, nil)
}

// Available は host で vsock (vhost-vsock) が使えるかを確かめる。
func Available() error {
	f, err := os.OpenFile("/dev/vhost-vsock", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("vsock が使えない (%v)。カーネルモジュール vhost_vsock を読み込む: sudo modprobe vhost_vsock "+
			"(カーネルを更新して再起動していなければ、再起動が要る)", err)
	}
	f.Close()
	return nil
}

// Exec は VM でコマンドを実行し、標準入出力をつなぐ。stdin が nil なら空。
func Exec(cid uint32, h Header, stdin io.Reader, stdout, stderr io.Writer) error {
	return ExecTimeout(cid, h, 0, stdin, stdout, stderr)
}

// ExecTimeout は Exec に時間制限 (0 なら無し) を付けたもの。VM が応答しなくても
// host の処理が止まり続けないようにする。
func ExecTimeout(cid uint32, h Header, timeout time.Duration, stdin io.Reader, stdout, stderr io.Writer) error {
	c, err := Dial(cid)
	if err != nil {
		return err
	}
	defer c.Close()
	if timeout > 0 {
		_ = c.SetDeadline(time.Now().Add(timeout))
	}
	return doExec(c, h, stdin, stdout, stderr, nil)
}

func doExec(c net.Conn, h Header, stdin io.Reader, stdout, stderr io.Writer, fw *frameWriter) error {
	if fw == nil {
		fw = &frameWriter{w: c}
	}
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if _, err := c.Write(append(b, '\n')); err != nil {
		return err
	}
	if stdin != nil {
		go func() {
			buf := make([]byte, 32*1024)
			for {
				n, err := stdin.Read(buf)
				if n > 0 {
					if fw.write(fStdin, buf[:n]) != nil {
						return
					}
				}
				if err != nil {
					if !h.TTY {
						_ = fw.write(fStdinEOF, nil)
					}
					return
				}
			}
		}()
	} else {
		_ = fw.write(fStdinEOF, nil)
	}
	for {
		t, p, err := readFrame(c)
		if err != nil {
			return fmt.Errorf("VM との接続が切れた: %w", err)
		}
		switch t {
		case fStdout:
			if stdout != nil {
				_, _ = stdout.Write(p)
			}
		case fStderr:
			if stderr != nil {
				_, _ = stderr.Write(p)
			}
		case fError:
			return fmt.Errorf("VM で起動できない: %s", p)
		case fExit:
			if len(p) != 4 {
				return fmt.Errorf("不正な終了コード")
			}
			if code := int(int32(binary.BigEndian.Uint32(p))); code != 0 {
				return &ExitError{Code: code}
			}
			return nil
		}
	}
}

// Interactive は端末を raw にして VM の端末つきコマンドにつなぐ (tmux のペイン用)。
// onClip が nil でなければ、VM の出力から OSC 52 (クリップボード操作) を抜き取って渡す
// (tmux や端末には届かない)。
func Interactive(cid uint32, argv []string, dir string, onClip func(ClipboardEvent)) error {
	c, err := Dial(cid)
	if err != nil {
		return err
	}
	defer c.Close()
	fd := int(os.Stdin.Fd())
	h := Header{Argv: argv, Dir: dir, TTY: true, Term: os.Getenv("TERM")}
	if cols, rows, err := term.GetSize(fd); err == nil {
		h.Rows, h.Cols = uint16(rows), uint16(cols)
	}
	if term.IsTerminal(fd) {
		old, err := term.MakeRaw(fd)
		if err != nil {
			return err
		}
		defer term.Restore(fd, old)
	}
	fw := &frameWriter{w: c}
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			if cols, rows, err := term.GetSize(fd); err == nil {
				var p [4]byte
				binary.BigEndian.PutUint16(p[0:2], uint16(rows))
				binary.BigEndian.PutUint16(p[2:4], uint16(cols))
				_ = fw.write(fResize, p[:])
			}
		}
	}()
	// onClip が無くても OSC 52 は抜き取って捨てる (host の端末に届かせない)
	err = doExec(c, h, os.Stdin, newOSCFilter(os.Stdout, onClip), nil, fw)
	var ee *ExitError
	if errors.As(err, &ee) {
		return nil // 端末の中身として終了コードは見せ終わっている
	}
	return err
}

// WaitReady は VM の受け口に接続できるまで待つ。fail が non-nil を返したら中断する。
func WaitReady(cid uint32, timeout time.Duration, fail func() error) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fail != nil {
			if err := fail(); err != nil {
				return err
			}
		}
		if c, err := Dial(cid); err == nil {
			c.Close()
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("%s 以内に VM の受け口 (vsock) に接続できなかった", timeout)
}
