package console

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
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

// compactExcerpt は該当箇所を中心に画面に収まる範囲だけを返す。
func TestCompactExcerpt(t *testing.T) {
	lines := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	lines[50] = "the secret me@example.com here"
	got := compactExcerpt(strings.Join(lines, "\n"), "me@example.com", 12, 200)
	if !strings.Contains(got, "me@example.com") {
		t.Errorf("該当箇所が抜粋に無い:\n%s", got)
	}
	if strings.Count(got, "\n") > 13 {
		t.Errorf("行数が多い (%d):\n%s", strings.Count(got, "\n"), got)
	}
	if !strings.Contains(got, "line 45") {
		t.Errorf("該当箇所の前後の行が無い:\n%s", got)
	}
	if strings.Contains(got, "line 0\n") || strings.Contains(got, "line 99") {
		t.Errorf("該当箇所から遠い行まで出している:\n%s", got)
	}
	// 長い 1 行は該当箇所の前後が見えるように横に切る。
	long := strings.Repeat("A", 5000) + "secret-token" + strings.Repeat("B", 5000)
	got = compactExcerpt(long, "secret-token", 12, 200)
	if !strings.Contains(got, "secret-token") {
		t.Errorf("長い行で該当箇所が見えない")
	}
	if n := len([]rune(got)); n > 210 {
		t.Errorf("長い行が切られていない: %d ルーン", n)
	}
	// 該当箇所が無ければ先頭から。
	got = compactExcerpt(strings.Join(lines, "\n"), "", 3, 200)
	if !strings.HasPrefix(got, "line 0\nline 1\nline 2") || !strings.Contains(got, "…") {
		t.Errorf("先頭の抜粋が違う:\n%s", got)
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
		Evidence: "me@example.com", Headers: []string{"User-Agent: leak"}, Body: "body"}
	errCh, req := ask(info)
	if req.Provider != "p" || req.Method != "POST" || req.URL != "h/p" || req.Reason != "メールが漏れる" ||
		req.Evidence != "me@example.com" || len(req.Headers) != 1 || req.Body != "body" {
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
