package main

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

// エージェントに伝える端末の配色の判定。エージェントの TUI は端末に前景・背景色を
// 問い合わせ (OSC 10/11) て自分の配色を決めるので、その答えを __attach 側で固定する
// (internal/guest/osc.go)。ここでは起動した端末の背景色から light / dark を見積もる。

// resolveTermTheme はエージェントに伝える端末の配色 (light / dark) を決める。
// opt が light / dark ならそれを、auto (既定) なら起動した端末の背景色から判定する。
// 判定できなければ空を返し、色問い合わせは従来どおり端末 (や tmux クライアント) に任せる。
func resolveTermTheme(opt string) string {
	switch opt {
	case "light", "dark":
		return opt
	case "", "auto":
	default:
		logf("--term-theme の値が不正 (%q)。auto として扱う", opt)
	}
	return detectTerminalTheme()
}

// detectTerminalTheme は端末の背景色から light / dark を推定する (分からなければ "")。
func detectTerminalTheme() string {
	// COLORFGBG は "<前景>;<背景>" の ANSI 色番号。背景が明るい側 (7 以上) なら light。
	if v := os.Getenv("COLORFGBG"); v != "" {
		parts := strings.Split(v, ";")
		if n, err := strconv.Atoi(parts[len(parts)-1]); err == nil {
			if n >= 7 {
				return "light"
			}
			return "dark"
		}
	}
	return queryTerminalTheme()
}

// queryTerminalTheme は端末に OSC 11 (背景色) を問い合わせて明暗を判定する。
// 応答が無い端末や端末でないときは空を返す。
func queryTerminalTheme() string {
	if !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		return ""
	}
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return ""
	}
	defer func() { _ = term.Restore(fd, old) }()
	if _, err := os.Stdout.WriteString("\x1b]11;?\x1b\\"); err != nil {
		return ""
	}
	if err := os.Stdin.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
		return ""
	}
	defer func() { _ = os.Stdin.SetReadDeadline(time.Time{}) }()
	var buf []byte
	tmp := make([]byte, 64)
	for len(buf) < 256 {
		n, err := os.Stdin.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if bytes.Contains(buf, []byte("\x1b\\")) || bytes.Contains(buf, []byte{0x07}) || err != nil {
			break
		}
	}
	r, g, b, ok := parseBackgroundColor(buf)
	if !ok {
		return ""
	}
	if 0.299*float64(r)+0.587*float64(g)+0.114*float64(b) > 128 {
		return "light"
	}
	return "dark"
}

// parseBackgroundColor は OSC 11 の応答 ("rgb:RRRR/GGGG/BBBB") を 8 bit へ直す。
func parseBackgroundColor(b []byte) (r, g, bl int, ok bool) {
	i := bytes.Index(b, []byte("rgb:"))
	if i < 0 {
		return 0, 0, 0, false
	}
	fields := strings.Split(string(b[i+4:]), "/")
	if len(fields) < 3 {
		return 0, 0, 0, false
	}
	vals := [3]int{}
	for j := 0; j < 3; j++ {
		s := strings.TrimRight(strings.TrimSpace(fields[j]), "\x1b\\\x07")
		v, err := strconv.ParseUint(s, 16, 32)
		if err != nil {
			return 0, 0, 0, false
		}
		switch len(s) {
		case 1:
			v <<= 4
		case 2:
		case 3:
			v >>= 4
		case 4:
			v >>= 8
		default:
			return 0, 0, 0, false
		}
		vals[j] = int(v)
	}
	return vals[0], vals[1], vals[2], true
}
