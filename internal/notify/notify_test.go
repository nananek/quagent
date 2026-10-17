package notify

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewDisabled(t *testing.T) {
	if New("") != nil {
		t.Fatal("空の URL では nil を返すべき")
	}
	if New("   ") != nil {
		t.Fatal("空白だけの URL では nil を返すべき")
	}
	var n *Notifier
	n.Send(Message{Title: "x"}) // nil でも落ちない
	n.Close()
	if n.Enabled() {
		t.Fatal("nil は無効のはず")
	}
}

func TestSendPostsSanitized(t *testing.T) {
	type got struct {
		payload discordPayload
		ctype   string
	}
	ch := make(chan got, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p discordPayload
		_ = json.NewDecoder(r.Body).Decode(&p)
		ch <- got{payload: p, ctype: r.Header.Get("Content-Type")}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	n := New(srv.URL)
	if n == nil {
		t.Fatal("URL があれば nil でないはず")
	}
	n.interval = 0
	defer n.Close()

	// 制御文字 (画面偽装) を含む本文を送る。
	n.Send(Message{Title: "承認待ち", Body: "evil\x1b[2Jbody", URL: "http://example/s/abc"})

	select {
	case g := <-ch:
		if g.ctype != "application/json" {
			t.Errorf("Content-Type: %q", g.ctype)
		}
		if len(g.payload.Embeds) != 1 {
			t.Fatalf("embeds: %d", len(g.payload.Embeds))
		}
		e := g.payload.Embeds[0]
		if e.Title != "承認待ち" {
			t.Errorf("title: %q", e.Title)
		}
		if strings.ContainsRune(e.Description, 0x1b) {
			t.Errorf("制御文字が残っている: %q", e.Description)
		}
		if !strings.Contains(e.Description, `\u001b`) {
			t.Errorf("制御文字が \\u 表記になっていない: %q", e.Description)
		}
		if e.URL != "http://example/s/abc" {
			t.Errorf("url: %q", e.URL)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("webhook が呼ばれなかった")
	}
}

func TestSendDoesNotBlockWhenQueueFull(t *testing.T) {
	// 送信先が応答しない間も Send はブロックしない (捨てる)。
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer func() { close(block); srv.Close() }()

	n := New(srv.URL)
	n.interval = 0
	defer n.Close()

	done := make(chan struct{})
	go func() {
		for i := 0; i < queueSize*4; i++ {
			n.Send(Message{Title: "x"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Send がブロックした")
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("abcd", 10); got != "abcd" {
		t.Errorf("短い文字列はそのまま: %q", got)
	}
	got := truncateRunes("あいうえお", 3)
	if got != "あいう…" {
		t.Errorf("切り詰め: %q", got)
	}
}
