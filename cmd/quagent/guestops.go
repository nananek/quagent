package main

import (
	"bytes"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

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
}

// stream は VM でログインシェル経由で script を実行する (レシピが通した PATH が効く)。
func (g vmGuest) stream(script string, stdin io.Reader, stdout, stderr io.Writer) error {
	return guest.Exec(g.cid, guest.Header{Argv: []string{"bash", "-lc", script}}, stdin, stdout, stderr)
}

// sh は script を実行し、標準出力と標準エラーをまとめて返す。
func (g vmGuest) sh(script string, stdin io.Reader) ([]byte, error) {
	var out bytes.Buffer
	err := g.stream(script, stdin, &out, &out)
	return out.Bytes(), err
}

// writeFile は VM の path (~ 始まり可) に data を書く。
func (g vmGuest) writeFile(path string, data []byte) error {
	dir := path[:strings.LastIndex(path, "/")]
	if out, err := g.sh(fmt.Sprintf("mkdir -p %s && cat > %s", dir, path), bytes.NewReader(data)); err != nil {
		return fmt.Errorf("VM への書き込みに失敗 (%s): %v: %s", path, err, out)
	}
	return nil
}

// interactiveArgv は VM の script に端末つきでつなぐ host 側のコマンドを返す。
func (g vmGuest) interactiveArgv(script string) []string {
	return []string{g.self, attachCommand, strconv.FormatUint(uint64(g.cid), 10), "--", "bash", "-lc", script}
}

// gitURL は VM の /work を git で取り込むための URL (ext:: 転送で vsock を通す)。
// 使うときは -c protocol.ext.allow=always が要る。
func (g vmGuest) gitURL() string {
	esc := func(s string) string { return strings.NewReplacer("%", "%%", " ", "% ").Replace(s) }
	return fmt.Sprintf("ext::%s %s %d -- %%S /work", esc(g.self), execCommand, g.cid)
}

// cmdExec / cmdAttach は `quagent __exec|__attach CID -- argv...`。
func parseGuestArgs(args []string) (uint32, []string, error) {
	if len(args) < 3 || args[1] != "--" {
		return 0, nil, fmt.Errorf("usage: CID -- argv...")
	}
	cid, err := strconv.ParseUint(args[0], 10, 32)
	if err != nil {
		return 0, nil, fmt.Errorf("CID が不正: %q", args[0])
	}
	return uint32(cid), args[2:], nil
}

func cmdExec(args []string) error {
	cid, argv, err := parseGuestArgs(args)
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
	cid, argv, err := parseGuestArgs(args)
	if err != nil {
		return err
	}
	return guest.Interactive(cid, argv, "/work")
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
