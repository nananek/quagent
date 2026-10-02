package guest

import (
	"bytes"
	"encoding/base64"
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

func TestStripClipboardDropsOSC52(t *testing.T) {
	var out bytes.Buffer
	w := StripClipboard(&out)
	_, _ = w.Write([]byte("a\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte("x")) + "\x07b"))
	if out.String() != "ab" {
		t.Fatalf("out = %q", out.String())
	}
}
