package console

import (
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/nananek/quagent/internal/access"
)

// testServer は console.sock の代わりに 1 接続を受け、hello を確認してから
// onHello を呼ぶ。返り値の chan には受け取ったメッセージが流れる。
func testServer(t *testing.T, onHello func(enc *json.Encoder)) (string, <-chan Msg, func()) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "console.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan Msg, 8)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		dec, enc := json.NewDecoder(c), json.NewEncoder(c)
		var hello Msg
		if dec.Decode(&hello) != nil || hello.Type != "ui" {
			return
		}
		if onHello != nil {
			onHello(enc)
		}
		for {
			var m Msg
			if dec.Decode(&m) != nil {
				return
			}
			got <- m
		}
	}()
	return sock, got, func() { l.Close() }
}

func TestDialSendsUI(t *testing.T) {
	hellos := make(chan Msg, 1)
	sock := filepath.Join(t.TempDir(), "console.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		var m Msg
		if json.NewDecoder(c).Decode(&m) == nil {
			hellos <- m
		}
	}()
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case m := <-hellos:
		if m.Type != "ui" {
			t.Fatalf("最初のメッセージ: %+v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ui が届かない")
	}
}

func TestEventsReceivesPending(t *testing.T) {
	sock, _, stop := testServer(t, func(enc *json.Encoder) {
		_ = enc.Encode(Msg{Type: "request", ID: 7, Domains: []string{"pypi.org"}, Reason: "pip install"})
	})
	defer stop()
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case m := <-c.Events():
		if m.Type != "request" || m.ID != 7 || len(m.Domains) != 1 || m.Domains[0] != "pypi.org" {
			t.Fatalf("受信: %+v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("保留中の申請が届かない")
	}
}

func TestSendDecision(t *testing.T) {
	sock, got, stop := testServer(t, nil)
	defer stop()
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Send(Msg{Type: "decide", ID: 3, Status: access.Denied, Kind: access.Once}); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-got:
		if m.Type != "decide" || m.ID != 3 || m.Status != access.Denied || m.Kind != access.Once {
			t.Fatalf("判断: %+v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("判断が届かない")
	}
}

func TestSendQuestion(t *testing.T) {
	sock, got, stop := testServer(t, nil)
	defer stop()
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Send(Msg{Type: "decide", ID: 4, Status: access.Question, Question: "どのホスト?"}); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-got:
		if m.Status != access.Question || m.Question != "どのホスト?" {
			t.Fatalf("問い返し: %+v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("問い返しが届かない")
	}
}

func TestEventsClosesOnDisconnect(t *testing.T) {
	sock, _, stop := testServer(t, nil)
	defer stop()
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-c.Events():
		if ok {
			t.Fatal("閉じた接続からメッセージが来た")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("切断しても Events が閉じない")
	}
}

func TestSendAfterClose(t *testing.T) {
	sock, _, stop := testServer(t, nil)
	defer stop()
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Send(Msg{Type: "decide"}); err != ErrConnClosed {
		t.Fatalf("閉じた後の Send: %v", err)
	}
}
