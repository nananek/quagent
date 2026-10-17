package hub

import (
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/nananek/quagent/internal/access"
	"github.com/nananek/quagent/internal/console"
)

// TestConsoleBridge は console.sock を模した相手と中継ができることを検証する。
func TestConsoleBridge(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "console.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	decisions := make(chan console.Msg, 4)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		dec, enc := json.NewDecoder(c), json.NewEncoder(c)
		var hello console.Msg
		if dec.Decode(&hello) != nil || hello.Type != "ui" {
			return
		}
		// 承認要求を 1 件送る。
		_ = enc.Encode(console.Msg{Type: "request", ID: 7, Domains: []string{"example.com"}, Reason: "test"})
		for {
			var d console.Msg
			if dec.Decode(&d) != nil {
				return
			}
			decisions <- d
		}
	}()

	msgs := make(chan console.Msg, 4)
	b := newConsoleBridge(sock, func(m console.Msg) { msgs <- m })
	defer b.Close()

	select {
	case m := <-msgs:
		if m.Type != "request" || m.ID != 7 || len(m.Domains) != 1 {
			t.Fatalf("受信: %+v", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("承認要求が中継されない")
	}

	// ブラウザの判断が console.sock へ流れる。
	if err := b.Send(console.Msg{Type: "decide", ID: 7, Status: access.Approved, Kind: access.Once}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case d := <-decisions:
		if d.Type != "decide" || d.ID != 7 || d.Status != access.Approved {
			t.Fatalf("判断: %+v", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("判断が届かない")
	}
}

func TestApprovalSummary(t *testing.T) {
	cases := []struct {
		msg   console.Msg
		ok    bool
		title string
	}{
		{console.Msg{Type: "request", Domains: []string{"a.com"}}, true, "接続先の申請"},
		{console.Msg{Type: "relaxrequest"}, true, "ヘッダ緩和の申請"},
		{console.Msg{Type: "prrequest", Branch: "f", Base: "main"}, true, "PR 作成の承認"},
		{console.Msg{Type: "dockerrequest"}, true, "Docker イメージの取得"},
		{console.Msg{Type: "batchrequest"}, true, "パッケージ取得の承認"},
		{console.Msg{Type: "clip"}, true, "クリップボードへの書き込み"},
		{console.Msg{Type: "settled", ID: 1}, false, ""},
		{console.Msg{Type: "log", Text: "x"}, false, ""},
	}
	for _, c := range cases {
		title, _, ok := approvalSummary(c.msg)
		if ok != c.ok || title != c.title {
			t.Errorf("approvalSummary(%q) = (%q, %v), want (%q, %v)", c.msg.Type, title, ok, c.title, c.ok)
		}
	}
}

func TestMarkNotifiedDedupes(t *testing.T) {
	s := newTestServer()
	if !s.markNotified("k") {
		t.Fatal("1 回目は通知するはず")
	}
	if s.markNotified("k") {
		t.Fatal("2 回目は抑制されるはず")
	}
	if !s.markNotified("k2") {
		t.Fatal("別のキーは通知するはず")
	}
}
