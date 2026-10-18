package hub

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestIsPaste(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want bool
	}{
		{"1 文字", []byte("a"), false},
		{"矢印キー", []byte("\x1b[A"), false},
		{"短い文字列", []byte("hello"), false},
		{"しきい値と同じ大きさ", bytes.Repeat([]byte("x"), pasteThreshold), true},
		{"ブラケットペースト", []byte("\x1b[200~hi\x1b[201~"), true},
	}
	for _, c := range cases {
		if got := isPaste(c.in); got != c.want {
			t.Errorf("%s: isPaste = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDecodeControl(t *testing.T) {
	cases := map[string]string{
		"plain": "plain",
		// tmux は非可印バイトと \ を 3 桁八進でエスケープする。
		`A\033[31mR\033[0mB\007C\134D\015\012`: "A\x1b[31mR\x1b[0mB\x07C\\D\r\n",
		"jp=日本語":                               "jp=日本語",   // 高位バイトはエスケープされない
		`trail\03`:                             `trail\03`, // 3 桁未満はそのまま
		`bad\089`:                              `bad\089`,  // 八進でない桁はそのまま
		`end\`:                                 `end\`,
	}
	for in, want := range cases {
		if got := string(decodeControl([]byte(in))); got != want {
			t.Errorf("decodeControl(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseOutput(t *testing.T) {
	pane, data, ok := parseOutput(`%output %0 hi\033there`)
	if !ok || pane != "%0" || string(data) != "hi\x1bthere" {
		t.Fatalf("parseOutput: pane=%q data=%q ok=%v", pane, data, ok)
	}
	if _, _, ok := parseOutput("%begin 1 2 3"); ok {
		t.Error("begin 行を output として扱ってしまった")
	}
	if _, _, ok := parseOutput("%output %0"); ok {
		t.Error("データの無い行を有効として扱ってしまった")
	}
}

// TestControlClient は実物の tmux で control mode クライアントを検証する。
// tmux が無い環境ではスキップする。
func TestControlClient(t *testing.T) {
	if _, err := exec.LookPath(tmuxBinary); err != nil {
		t.Skip("tmux が無いのでスキップ")
	}
	// 専用の tmux サーバーを使う (利用者のセッションに触らない)。
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	// TERM が無い環境 (CI など) でも tmux が動くようにしておく。
	if os.Getenv("TERM") == "" {
		t.Setenv("TERM", "xterm-256color")
	}
	session := "quagent-hubtest"

	if out, err := exec.Command(tmuxBinary, "new-session", "-d", "-s", session, "-x", "80", "-y", "24",
		"sh", "-c", `printf 'HELLO\033[31mRED\033[0m\n'; while read x; do printf 'GOT:%s\n' "$x"; done`).CombinedOutput(); err != nil {
		t.Fatalf("tmux new-session: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command(tmuxBinary, "kill-server").Run() })

	out := make(chan []byte, 64)
	cc, err := startControl(session, func(b []byte) {
		select {
		case out <- append([]byte(nil), b...):
		default:
		}
	})
	if err != nil {
		t.Fatalf("startControl: %v", err)
	}
	defer cc.Close()
	if cc.Pane() == "" {
		t.Fatal("ペインが取れていない")
	}

	// 初期画面には先に出た出力が残っている。
	screen, err := cc.Capture()
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if !strings.Contains(string(screen), "HELLO") {
		t.Errorf("初期画面に HELLO が無い: %q", screen)
	}
	if !bytes.Contains(screen, []byte("\x1b[31m")) {
		t.Errorf("色のエスケープが保持されていない: %q", screen)
	}

	// 入力を送ると、ペインがその行を処理して出力する。
	if err := cc.Send([]byte("hi\n")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitForOutput(out, "GOT:hi", 5*time.Second) {
		t.Fatal("入力がペインに届いていない")
	}

	if err := cc.Resize(100, 30); err != nil {
		t.Fatalf("Resize: %v", err)
	}
}

// TestControlClientPaste は実物の tmux でバッファ経由の貼り付け (Paste) を検証する。
// send-keys のキー注入を経由せず、バイト列がそのままペインへ届くことを確かめる。
// tmux が無い環境ではスキップする。
func TestControlClientPaste(t *testing.T) {
	if _, err := exec.LookPath(tmuxBinary); err != nil {
		t.Skip("tmux が無いのでスキップ")
	}
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	if os.Getenv("TERM") == "" {
		t.Setenv("TERM", "xterm-256color")
	}
	session := "quagent-hubtest-paste"

	// 改行とブラケットペーストのマーカーを含むバイト列。ペインは生モードで同じ
	// 長さだけ読み、16 進で表示する (エコーや端末の解釈を挟まない)。
	payload := []byte("line1\nline2\n\x1b[200~貼り付け\x1b[201~")
	script := fmt.Sprintf("stty raw -echo; printf 'READY\\n'; head -c %d | od -An -tx1; sleep 3", len(payload))
	if out, err := exec.Command(tmuxBinary, "new-session", "-d", "-s", session, "-x", "80", "-y", "24",
		"sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("tmux new-session: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command(tmuxBinary, "kill-server").Run() })

	out := make(chan []byte, 256)
	cc, err := startControl(session, func(b []byte) {
		select {
		case out <- append([]byte(nil), b...):
		default:
		}
	})
	if err != nil {
		t.Fatalf("startControl: %v", err)
	}
	defer cc.Close()

	// ペインが生モードに入る (READY を出す) のを待ってから貼り付ける。起動直後に
	// 出るため、control client の接続が間に合えば sink に、遅ければ初期画面に現れる。
	screen, err := cc.Capture()
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if !strings.Contains(string(screen), "READY") && !waitForOutput(out, "READY", 5*time.Second) {
		t.Fatal("ペインが READY にならない")
	}
	if err := cc.Paste(payload); err != nil {
		t.Fatalf("Paste: %v", err)
	}

	want := hex.EncodeToString(payload)
	deadline := time.After(5 * time.Second)
	var acc []byte
	for {
		select {
		case b := <-out:
			acc = append(acc, b...)
			if got := strings.Join(strings.Fields(string(acc)), ""); strings.Contains(got, want) {
				return
			}
		case <-deadline:
			got := strings.Join(strings.Fields(string(acc)), "")
			t.Fatalf("貼り付けたバイトが一致しない\n want: %s\n got:  %s", want, got)
		}
	}
}

func waitForOutput(ch <-chan []byte, want string, d time.Duration) bool {
	deadline := time.After(d)
	var acc []byte
	for {
		select {
		case b := <-ch:
			acc = append(acc, b...)
			if strings.Contains(string(acc), want) {
				return true
			}
		case <-deadline:
			return false
		}
	}
}
