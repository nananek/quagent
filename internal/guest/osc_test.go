package guest

import (
	"bytes"
	"encoding/base64"
	"io"
	"strings"
	"testing"
)

func runFilter(chunks ...string) (string, []ClipboardEvent) {
	var out bytes.Buffer
	var evs []ClipboardEvent
	f := newOSCFilter(&out, func(e ClipboardEvent) { evs = append(evs, e) })
	for _, c := range chunks {
		_, _ = f.Write([]byte(c))
	}
	return out.String(), evs
}

func TestOSCFilterExtractsClipboard(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte("secret"))
	out, evs := runFilter("before\x1b]52;c;" + b64 + "\x07after")
	if out != "beforeafter" {
		t.Fatalf("OSC 52 が流れた: %q", out)
	}
	if len(evs) != 1 || string(evs[0].Data) != "secret" {
		t.Fatalf("events=%+v", evs)
	}
}

func TestOSCFilterSplitAcrossWrites(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte("hello"))
	seq := "x\x1b]52;c;" + b64 + "\x1b\\y"
	var chunks []string
	for i := range len(seq) {
		chunks = append(chunks, seq[i:i+1]) // 1 バイトずつ
	}
	out, evs := runFilter(chunks...)
	if out != "xy" || len(evs) != 1 || string(evs[0].Data) != "hello" {
		t.Fatalf("out=%q events=%+v", out, evs)
	}
}

func TestOSCFilterQueryAndOthers(t *testing.T) {
	out, evs := runFilter("\x1b]52;c;?\x07\x1b]0;title\x07\x1b[31mred")
	if len(evs) != 1 || !evs[0].Query {
		t.Fatalf("読み出し要求として扱われない: %+v", evs)
	}
	if out != "\x1b]0;title\x07\x1b[31mred" {
		t.Fatalf("OSC 52 以外の出力が変わった: %q", out)
	}
}

func TestOSCFilterBounded(t *testing.T) {
	// 閉じない OSC を大量に流しても溜め込まない
	var out bytes.Buffer
	var evs []ClipboardEvent
	f := newOSCFilter(&out, func(e ClipboardEvent) { evs = append(evs, e) })
	_, _ = f.Write([]byte("\x1b]52;c;"))
	big := strings.Repeat("A", 1<<20)
	for range 4 {
		_, _ = f.Write([]byte(big))
	}
	if len(f.osc) > maxOSC {
		t.Fatalf("上限を超えて溜めた: %d", len(f.osc))
	}
	_, _ = f.Write([]byte("\x07ok"))
	if out.String() != "ok" || len(evs) != 1 || !evs[0].TooLarge {
		t.Fatalf("out=%q events=%+v", out.String(), evs)
	}
}

func TestOSCFilterAnswersColorQuery(t *testing.T) {
	var out bytes.Buffer
	var injected []byte
	f := newOSCFilter(&out, nil)
	f.colors = TerminalColorsForScheme("light")
	f.inject = func(b []byte) { injected = append(injected, b...) }
	_, _ = f.Write([]byte("a\x1b]11;?\x1b\\b"))
	if out.String() != "ab" {
		t.Fatalf("色問い合わせが端末へ流れた: %q", out.String())
	}
	want := "\x1b]11;rgb:f9f9/f8f8/f5f5\x1b\\"
	if string(injected) != want {
		t.Fatalf("injected = %q, want %q", injected, want)
	}
}

func TestOSCFilterColorQueryWithoutSchemePassesThrough(t *testing.T) {
	// colors が nil なら従来どおり素通しする (tmux クライアントに任せる)。
	out, _ := runFilter("\x1b]11;?\x1b\\")
	if out != "\x1b]11;?\x1b\\" {
		t.Fatalf("colors 無しで素通しされない: %q", out)
	}
}

func TestOSCFilterColorSetPassesThrough(t *testing.T) {
	var out bytes.Buffer
	f := newOSCFilter(&out, nil)
	f.colors = TerminalColorsForScheme("dark")
	f.inject = func([]byte) { t.Error("色の設定は注入しない") }
	_, _ = f.Write([]byte("\x1b]11;rgb:0000/0000/0000\x1b\\"))
	if !strings.Contains(out.String(), "11;rgb") {
		t.Fatalf("色設定が素通しされない: %q", out.String())
	}
}

func TestReplyColorQuery(t *testing.T) {
	c := TerminalColorsForScheme("dark")
	if c == nil {
		t.Fatal("dark の配色が無い")
	}
	if _, ok := c.replyColorQuery("10;?"); !ok {
		t.Error("OSC 10 の問い合わせに答えていない")
	}
	if _, ok := c.replyColorQuery("12;?"); !ok {
		t.Error("OSC 12 の問い合わせに答えていない")
	}
	if _, ok := c.replyColorQuery("52;c;?"); ok {
		t.Error("OSC 52 を色問い合わせとして扱った")
	}
	if _, ok := c.replyColorQuery("11"); ok {
		t.Error("値の無い OSC 11 を問い合わせとして扱った")
	}
	if TerminalColorsForScheme("") != nil || TerminalColorsForScheme("bogus") != nil {
		t.Error("未知の配色は nil のはず")
	}
}

func TestMergeInputInjects(t *testing.T) {
	in, inject := mergeInput(strings.NewReader("hello"))
	buf := make([]byte, 5)
	if _, err := io.ReadFull(in, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "hello" {
		t.Fatalf("got %q", buf)
	}
	inject([]byte("world"))
	got := make([]byte, 5)
	if _, err := io.ReadFull(in, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "world" {
		t.Fatalf("差し込みが読めない: %q", got)
	}
}

func TestStripClipboardDropsOSC52(t *testing.T) {
	var out bytes.Buffer
	w := StripClipboard(&out)
	_, _ = w.Write([]byte("a\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte("x")) + "\x07b"))
	if out.String() != "ab" {
		t.Fatalf("out = %q", out.String())
	}
}
