package hub

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
	"time"
)

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
