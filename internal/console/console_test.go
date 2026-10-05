package console

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/nananek/quagent/internal/access"
	"github.com/nananek/quagent/internal/netns"
)

type noApply struct{}

func (noApply) SetGrants([]netns.Grant) error { return nil }

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"pip install":                 "pip install",
		"a\x1b[2J\x1b]52;c;aGk=\x07b": `a\u001b[2J\u001b]52;c;aGk=\u0007b`,
		"line1\nline2":                "line1\nline2",
		"\u202eevil":                  `\u202eevil`,
		"\u009b31m":                   `\u009b31m`,
		"日本語の理由":                      "日本語の理由",
		"bad\xffutf8":                 `bad\ufffdutf8`,
	}
	for in, want := range cases {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

// AskPR は承認コンソールの y/n で決着し、承認なら nil、拒否ならエラーを返す。
func TestAskPR(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "console.sock")
	m, err := access.NewManager(noApply{}, filepath.Join(t.TempDir(), "always.json"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(m, sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	enc, dec := json.NewEncoder(c), json.NewDecoder(c)
	if err := enc.Encode(Msg{Type: "ui"}); err != nil {
		t.Fatal(err)
	}

	// ID は単調に増える。UI の接続と申請の送信が競合すると同じ申請が二度
	// 届くことがある (実 UI も onMsg で重複を捨てる) ので、処理済みの ID は読み飛ばす。
	lastID := 0
	ask := func(info PRInfo) (chan error, Msg) {
		errCh := make(chan error, 1)
		go func() { errCh <- s.AskPR(info) }()
		for {
			_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
			var msg Msg
			if err := dec.Decode(&msg); err != nil {
				t.Fatal(err)
			}
			if msg.Type == "prrequest" && msg.ID > lastID {
				lastID = msg.ID
				return errCh, msg
			}
		}
	}
	wait := func(errCh chan error) error {
		select {
		case err := <-errCh:
			return err
		case <-time.After(3 * time.Second):
			t.Fatal("AskPR が返らない")
			return nil
		}
	}

	errCh, req := ask(PRInfo{Branch: "feature", Base: "main", Title: "t", Body: "b"})
	if req.Branch != "feature" || req.Base != "main" || req.Title != "t" {
		t.Fatalf("承認に渡す内容が違う: %+v", req)
	}
	if err := enc.Encode(Msg{Type: "prdecide", ID: req.ID, Status: access.Approved}); err != nil {
		t.Fatal(err)
	}
	if err := wait(errCh); err != nil {
		t.Fatalf("承認したのに %v", err)
	}

	// 拒否はエラーになり、push はされない
	errCh, req = ask(PRInfo{Branch: "feature", Base: "main", Title: "t"})
	if err := enc.Encode(Msg{Type: "prdecide", ID: req.ID, Status: access.Denied}); err != nil {
		t.Fatal(err)
	}
	if err := wait(errCh); err == nil {
		t.Fatal("拒否したのに nil が返った")
	}
}

// AskGuard は内容ガードの確認を承認コンソールに流し、y/n で決着する。
func TestAskGuard(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "console.sock")
	m, err := access.NewManager(noApply{}, filepath.Join(t.TempDir(), "always.json"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(m, sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	enc, dec := json.NewEncoder(c), json.NewDecoder(c)
	if err := enc.Encode(Msg{Type: "ui"}); err != nil {
		t.Fatal(err)
	}

	lastID := 0
	ask := func(info GuardInfo) (chan error, Msg) {
		errCh := make(chan error, 1)
		go func() { errCh <- s.AskGuard(context.Background(), info) }()
		for {
			_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
			var msg Msg
			if err := dec.Decode(&msg); err != nil {
				t.Fatal(err)
			}
			if msg.Type == "guardrequest" && msg.ID > lastID {
				lastID = msg.ID
				return errCh, msg
			}
		}
	}
	wait := func(errCh chan error) error {
		select {
		case err := <-errCh:
			return err
		case <-time.After(3 * time.Second):
			t.Fatal("AskGuard が返らない")
			return nil
		}
	}

	info := GuardInfo{Provider: "p", Method: "POST", URL: "h/p", Reason: "メールが漏れる",
		Headers: []string{"User-Agent: leak"}, Body: "body"}
	errCh, req := ask(info)
	if req.Provider != "p" || req.Method != "POST" || req.URL != "h/p" || req.Reason != "メールが漏れる" ||
		len(req.Headers) != 1 || req.Body != "body" {
		t.Fatalf("承認に渡す内容が違う: %+v", req)
	}
	if err := enc.Encode(Msg{Type: "guarddecide", ID: req.ID, Status: access.Approved}); err != nil {
		t.Fatal(err)
	}
	if err := wait(errCh); err != nil {
		t.Fatalf("通したのに %v", err)
	}

	errCh, req = ask(GuardInfo{Provider: "p", Method: "POST", URL: "h/p", Reason: "x"})
	if err := enc.Encode(Msg{Type: "guarddecide", ID: req.ID, Status: access.Denied}); err != nil {
		t.Fatal(err)
	}
	if err := wait(errCh); err == nil {
		t.Fatal("止めたのに nil が返った")
	}
}
