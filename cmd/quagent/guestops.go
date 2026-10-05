package main

import (
	"bytes"
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nananek/quagent/internal/console"
	"github.com/nananek/quagent/internal/guest"
)

// 隠しサブコマンド: VM の受け口 (VM 内)、VM のコマンドへの接続 (host 側)。
const (
	guestCommand  = "__guest"
	execCommand   = "__exec"   // 端末なし。git の ext:: 転送などに使う
	attachCommand = "__attach" // 端末つき。tmux のペインに使う
)

// vmGuest は host から vsock で VM を操作する。
type vmGuest struct {
	cid  uint32
	self string // quagent 自身 (__exec / __attach を呼ぶ)
	// consoleSock は承認コンソールの socket (__attach が OSC 52 を渡す先)。
	consoleSock string
}

// stream は VM でログインシェル経由で script を実行する (レシピが通した PATH が効く)。
func (g vmGuest) stream(script string, stdin io.Reader, stdout, stderr io.Writer) error {
	return guest.Exec(g.cid, guest.Header{Argv: []string{"bash", "-lc", script}}, stdin, stdout, stderr)
}

// 準備のためのコマンド (sh) の上限。VM が無限に出力したり応答しなかったりしても
// host のメモリや処理を使い潰されないように。
const (
	shTimeout   = 10 * time.Minute
	maxShOutput = 1 << 20
)

// sh は script を実行し、標準出力と標準エラーをまとめて返す (上限を超えた分は捨てる)。
func (g vmGuest) sh(script string, stdin io.Reader) ([]byte, error) {
	out := &cappedBuffer{max: maxShOutput}
	err := guest.ExecTimeout(g.cid, guest.Header{Argv: []string{"bash", "-lc", script}}, shTimeout, stdin, out, out)
	return out.Bytes(), err
}

// cappedBuffer は max バイトまでだけ溜める io.Writer (超えた分は読み捨てる)。
type cappedBuffer struct {
	bytes.Buffer
	max int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room > 0 {
		b.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// writeFile は VM の path (~ 始まり可) に data を書く。
func (g vmGuest) writeFile(path string, data []byte) error {
	i := strings.LastIndex(path, "/")
	if i < 0 {
		return fmt.Errorf("書き込み先が不正: %q", path)
	}
	dir := path[:i]
	if out, err := g.sh(fmt.Sprintf("mkdir -p %s && cat > %s", dir, path), bytes.NewReader(data)); err != nil {
		return fmt.Errorf("VM への書き込みに失敗 (%s): %v: %s", path, err, out)
	}
	return nil
}

// interactiveArgv は VM の script に端末つきでつなぐ host 側のコマンドを返す。
func (g vmGuest) interactiveArgv(script string) []string {
	return []string{g.self, attachCommand, strconv.FormatUint(uint64(g.cid), 10),
		"--console", g.consoleSock, "--", "bash", "-lc", script}
}

// gitURL は VM の /work を git で取り込むための URL (ext:: 転送で vsock を通す)。
// 使うときは -c protocol.ext.allow=always が要る。
func (g vmGuest) gitURL() string {
	esc := func(s string) string { return strings.NewReplacer("%", "%%", " ", "% ").Replace(s) }
	return fmt.Sprintf("ext::%s %s %d -- %%S /work", esc(g.self), execCommand, g.cid)
}

// cmdExec / cmdAttach は `quagent __exec|__attach CID [--console SOCK] -- argv...`。
func parseGuestArgs(args []string) (cid uint32, console string, argv []string, err error) {
	if len(args) < 1 {
		return 0, "", nil, fmt.Errorf("usage: CID [--console SOCK] -- argv...")
	}
	n, err := strconv.ParseUint(args[0], 10, 32)
	if err != nil {
		return 0, "", nil, fmt.Errorf("CID が不正: %q", args[0])
	}
	rest := args[1:]
	if len(rest) >= 2 && rest[0] == "--console" {
		console, rest = rest[1], rest[2:]
	}
	if len(rest) < 2 || rest[0] != "--" {
		return 0, "", nil, fmt.Errorf("usage: CID [--console SOCK] -- argv...")
	}
	return uint32(n), console, rest[1:], nil
}

func cmdExec(args []string) error {
	cid, _, argv, err := parseGuestArgs(args)
	if err != nil {
		return err
	}
	err = guest.Exec(cid, guest.Header{Argv: argv}, os.Stdin, os.Stdout, os.Stderr)
	var ee *guest.ExitError
	if errors.As(err, &ee) {
		os.Exit(ee.Code)
	}
	return err
}

func cmdAttach(args []string) error {
	cid, sock, argv, err := parseGuestArgs(args)
	if err != nil {
		return err
	}
	var onClip func(guest.ClipboardEvent)
	if sock != "" {
		onClip = clipboardRelay(sock)
	}
	return guest.Interactive(cid, argv, "/work", onClip)
}

// clipboardRelay は VM が出した OSC 52 を承認コンソールに渡す。出力を止めないよう
// 送るのは別 goroutine で、送り待ちが溜まっていれば捨てる (本体側でも数を絞る)。
func clipboardRelay(sock string) func(guest.ClipboardEvent) {
	ch := make(chan console.Msg, 1)
	go func() {
		var enc *json.Encoder
		for m := range ch {
			if enc == nil {
				c, err := net.Dial("unix", sock)
				if err != nil {
					continue
				}
				enc = json.NewEncoder(c)
			}
			if enc.Encode(m) != nil {
				enc = nil
			}
		}
	}()
	return func(e guest.ClipboardEvent) {
		m := console.Msg{Type: "clipboard"}
		switch {
		case e.Query:
			m.Text = "VM がクリップボードの読み出しを要求した (常に拒否)"
		case e.TooLarge:
			m.Text = fmt.Sprintf("書き込み要求が大きすぎるので拒否 (上限 %d KiB)", guest.MaxClipboard>>10)
		case e.Invalid:
			m.Text = "壊れた OSC 52 を捨てた"
		default:
			m.Data = e.Data
		}
		select {
		case ch <- m:
		default:
		}
	}
}

// checkStatic は VM に持ち込めるよう quagent が静的リンクかを確かめる。
func checkStatic(path string) error {
	f, err := elf.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return fmt.Errorf("quagent が動的リンクでビルドされている (VM に持ち込めない)。CGO_ENABLED=0 go build でビルドし直す")
		}
	}
	return nil
}
