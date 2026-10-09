package console

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
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

func TestClipboardPreview(t *testing.T) {
	// 1. 通常テキスト
	if got := preview([]byte("hello world")); got != "hello world" {
		t.Errorf("preview(hello) = %q, want 'hello world'", got)
	}

	// 2. 非UTF-8
	invalidUTF8 := []byte{0xff, 0xfe, 0xfd}
	if got := preview(invalidUTF8); got != "(テキストではないデータ 3 バイト)" {
		t.Errorf("preview(invalid) = %q", got)
	}

	// 3. 400文字超え
	longText := make([]rune, 500)
	for i := range longText {
		longText[i] = 'a'
	}
	got := preview([]byte(string(longText)))
	if len([]rune(got)) != 401 || got[len(got)-3:] != "…" { // 400 runes + "…" (1 rune)
		t.Errorf("preview(500 runes) length = %d runes, want 401 runes ending with …", len([]rune(got)))
	}
}

func TestCommandClipboard(t *testing.T) {
	// cat コマンドは入力をそのまま受け取って正常終了する
	sink := CommandClipboard([]string{"cat"})
	if err := sink([]byte("clipboard data")); err != nil {
		t.Fatalf("CommandClipboard cat failed: %v", err)
	}

	// 存在しないコマンドはエラー
	sinkFail := CommandClipboard([]string{"/nonexistent/command/quagent"})
	if err := sinkFail([]byte("test")); err == nil {
		t.Fatal("expected error for nonexistent command")
	}
}

func TestOSC52Clipboard(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "tty.txt")
	if err := os.WriteFile(tmp, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sink := OSC52Clipboard(tmp)
	if err := sink([]byte("hello")); err != nil {
		t.Fatalf("OSC52Clipboard failed: %v", err)
	}
}

func TestServerClipboardHandling(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "console-clip.sock")
	m, err := access.NewManager(noApply{}, filepath.Join(t.TempDir(), "always.json"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(m, sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	var copiedData []byte
	s.Clipboard = func(b []byte) error {
		copiedData = b
		return nil
	}

	// 1. 警告ログ (Text != "")
	s.onClipboard(Msg{Text: "read request blocked"})

	// 2. 大きすぎるデータ (>64KiB)
	tooBig := make([]byte, 65<<10)
	s.onClipboard(Msg{Data: tooBig})
	if s.clip.pending != nil {
		t.Fatal("pending should be nil for too big clipboard data")
	}

	// 3. 正常な書き込み要求
	s.onClipboard(Msg{Data: []byte("test copy")})
	if s.clip.pending == nil {
		t.Fatal("expected pending clip request")
	}
	clipID := s.clip.pending.id

	// 4. すぐ次の要求が来た場合 (dropped)
	s.onClipboard(Msg{Data: []byte("dropped copy")})
	if s.clip.dropped != 1 {
		t.Errorf("expected dropped count 1, got %d", s.clip.dropped)
	}

	// 5. 承認
	s.clipDecide(clipID, true)
	if string(copiedData) != "test copy" {
		t.Errorf("copiedData = %q, want 'test copy'", string(copiedData))
	}
	if s.clip.pending != nil {
		t.Fatal("pending should be nil after settle")
	}

	// 6. 再度要求して拒否
	s.clip.last = time.Time{} // interval をリセット
	s.onClipboard(Msg{Data: []byte("reject copy")})
	if s.clip.pending == nil {
		t.Fatal("expected pending clip request")
	}
	clipID2 := s.clip.pending.id
	s.clipDecide(clipID2, false)
	if s.clip.pending != nil {
		t.Fatal("pending should be nil after settle")
	}
}

func TestServerBacklog(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "console-backlog.sock")
	m, err := access.NewManager(noApply{}, filepath.Join(t.TempDir(), "always.json"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(m, sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	// クライアント未接続時の Log
	s.Log("initial test log")
	s.mu.Lock()
	backlogCount := len(s.backlog)
	s.mu.Unlock()
	if backlogCount != 1 {
		t.Errorf("expected backlog count 1, got %d", backlogCount)
	}
}

func TestConsoleClient(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "console-client.sock")
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
	defer c.Close()

	enc := json.NewEncoder(c)
	dec := json.NewDecoder(c)

	if err := enc.Encode(Msg{Type: "ui"}); err != nil {
		t.Fatal(err)
	}

	// サーバーからログを送信すると受信できること
	s.Log("test message")
	var msg Msg
	if err := dec.Decode(&msg); err != nil {
		t.Fatal(err)
	}
	if msg.Type != "log" || !strings.HasSuffix(msg.Text, "test message") {
		t.Fatalf("unexpected received msg: %+v", msg)
	}
}

func TestClipboardSinkFailure(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "console-clip-fail.sock")
	m, err := access.NewManager(noApply{}, filepath.Join(t.TempDir(), "always.json"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(m, sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	// sink が失敗する場合
	s.Clipboard = func([]byte) error {
		return fmt.Errorf("copy failed")
	}

	s.onClipboard(Msg{Data: []byte("fail test")})
	if s.clip.pending == nil {
		t.Fatal("expected pending clip request")
	}
	clipID := s.clip.pending.id
	s.clipDecide(clipID, true)

	if s.clip.pending != nil {
		t.Fatal("pending should be nil after settle")
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("short", 10); got != "short" {
		t.Errorf("truncateRunes(short) = %q, want short", got)
	}
	if got := truncateRunes("1234567890", 5); got != "12345…" {
		t.Errorf("truncateRunes(1234567890, 5) = %q, want 12345…", got)
	}
}

func TestClientUIPromptAndOnLine(t *testing.T) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	ui := &clientUI{enc: enc}

	// 1. 空のキュー
	if p := ui.prompt(); p != "" {
		t.Errorf("empty queue prompt = %q, want empty", p)
	}

	// 2. quit フロー
	if done := ui.onLine("quit"); done || !ui.quiting {
		t.Fatalf("onLine(quit) expected quiting=true, done=false")
	}
	buf.Reset()
	if done := ui.onLine("y"); !done {
		t.Fatalf("onLine(y) expected done=true")
	}

	// 3. clip 要求
	ui.quiting = false
	ui.queue = []Msg{{Type: "clip", ID: 1, Text: "clip test"}}
	if !strings.Contains(ui.prompt(), "[y] コピーする") {
		t.Errorf("clip prompt = %q", ui.prompt())
	}
	buf.Reset()
	ui.onLine("y")
	var m Msg
	_ = json.Unmarshal(buf.Bytes(), &m)
	if m.Type != "clipdecide" || m.Status != access.Approved {
		t.Errorf("expected approved clipdecide, got %+v", m)
	}

	// 4. prrequest 要求
	ui.queue = []Msg{{Type: "prrequest", ID: 2, Title: "PR Title"}}
	if !strings.Contains(ui.prompt(), "[y] 承認して PR を作る") {
		t.Errorf("prrequest prompt = %q", ui.prompt())
	}
	buf.Reset()
	ui.onLine("n")
	_ = json.Unmarshal(buf.Bytes(), &m)
	if m.Type != "prdecide" || m.Status != access.Denied {
		t.Errorf("expected denied prdecide, got %+v", m)
	}

	// 5. request 申請と決定 (1, 2, 3, d, q)
	ui.queue = []Msg{{Type: "request", ID: 3, Domains: []string{"example.com"}}}
	buf.Reset()
	ui.onLine("1")
	_ = json.Unmarshal(buf.Bytes(), &m)
	if m.Type != "decide" || m.Kind != access.Once {
		t.Errorf("expected decide Once, got %+v", m)
	}

	buf.Reset()
	ui.onLine("2")
	_ = json.Unmarshal(buf.Bytes(), &m)
	if m.Type != "decide" || m.Kind != access.Session {
		t.Errorf("expected decide Session, got %+v", m)
	}

	buf.Reset()
	ui.onLine("3")
	_ = json.Unmarshal(buf.Bytes(), &m)
	if m.Type != "decide" || m.Kind != access.Always {
		t.Errorf("expected decide Always, got %+v", m)
	}

	buf.Reset()
	ui.onLine("d")
	_ = json.Unmarshal(buf.Bytes(), &m)
	if m.Type != "decide" || m.Status != access.Denied {
		t.Errorf("expected decide Denied, got %+v", m)
	}

	// 質問
	ui.onLine("q")
	if !ui.asking || !strings.Contains(ui.prompt(), "エージェントへの質問") {
		t.Errorf("asking state or prompt unexpected: %q", ui.prompt())
	}
	buf.Reset()
	ui.onLine("何に使うの?")
	_ = json.Unmarshal(buf.Bytes(), &m)
	if m.Type != "decide" || m.Status != access.Question || m.Question != "何に使うの?" {
		t.Errorf("expected question decision, got %+v", m)
	}
}

func TestClientUIOnMsg(t *testing.T) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	ui := &clientUI{enc: enc}

	// 1. log メッセージ
	ui.onMsg(Msg{Type: "log", Text: "some log"})

	// 2. request 追加
	ui.onMsg(Msg{Type: "request", ID: 10, Domains: []string{"test.com"}, Reason: "fetch"})
	if len(ui.queue) != 1 {
		t.Fatalf("expected 1 item in queue, got %d", len(ui.queue))
	}

	// 重複追加は無視される
	ui.onMsg(Msg{Type: "request", ID: 10})
	if len(ui.queue) != 1 {
		t.Fatalf("expected duplicate to be ignored, got %d", len(ui.queue))
	}

	// 3. settled でキューから削除される
	ui.onMsg(Msg{Type: "settled", ID: 10, Status: access.Approved})
	if len(ui.queue) != 0 {
		t.Fatalf("expected queue to be empty after settled, got %d", len(ui.queue))
	}
}

func TestServerTriggerQuit(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "console-quit.sock")
	m, err := access.NewManager(noApply{}, filepath.Join(t.TempDir(), "always.json"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(m, sock)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.TriggerQuit()
	select {
	case <-s.Quit:
		// OK
	default:
		t.Fatal("expected s.Quit channel to be closed by TriggerQuit")
	}

	// 2 回呼んでも panic しない
	s.TriggerQuit()
}
