package console

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nananek/quagent/internal/access"
	"github.com/nananek/quagent/internal/headerpolicy"
	"github.com/nananek/quagent/internal/netns"

	tea "github.com/charmbracelet/bubbletea"
)

type noApply struct{}

func (noApply) SetGrants([]netns.Grant) error                         { return nil }
func (noApply) SetRelaxations(map[string]headerpolicy.HostRule) error { return nil }

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

func TestTUIModelDecisions(t *testing.T) {
	m := newModel("test.sock")

	// 1. 空のキューでの終了確認
	newM, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m = newM.(model)
	if !m.quiting {
		t.Fatalf("expected quiting=true")
	}
	// y で終了
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	select {
	case out := <-m.outChan:
		if out.Type != "quit" {
			t.Errorf("expected quit Msg, got %+v", out)
		}
	default:
		t.Fatal("expected quit Msg on outChan")
	}

	// 2. clip 要求
	m.quiting = false
	newM, _ = m.Update(socketMsg(Msg{Type: "clip", ID: 1, Text: "clip test"}))
	m = newM.(model)
	newM, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	m = newM.(model)
	select {
	case out := <-m.outChan:
		if out.Type != "clipdecide" || out.Status != access.Approved {
			t.Errorf("expected approved clipdecide, got %+v", out)
		}
	default:
		t.Fatal("expected clipdecide on outChan")
	}
	// clip の決着
	newM, _ = m.Update(socketMsg(Msg{Type: "clipsettled", ID: 1, Status: access.Approved}))
	m = newM.(model)

	// 3. prrequest 要求
	newM, _ = m.Update(socketMsg(Msg{Type: "prrequest", ID: 2, Title: "PR Title"}))
	m = newM.(model)
	newM, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	m = newM.(model)
	select {
	case out := <-m.outChan:
		if out.Type != "prdecide" || out.Status != access.Denied {
			t.Errorf("expected denied prdecide, got %+v", out)
		}
	default:
		t.Fatal("expected prdecide on outChan")
	}
	// prrequest の決着
	newM, _ = m.Update(socketMsg(Msg{Type: "prsettled", ID: 2, Status: access.Denied}))
	m = newM.(model)

	// 4. request 申請と決定 (1, 2, 3, d, q)
	newM, _ = m.Update(socketMsg(Msg{Type: "request", ID: 3, Domains: []string{"example.com"}}))
	m = newM.(model)

	newM, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}})
	m = newM.(model)
	select {
	case out := <-m.outChan:
		if out.Type != "decide" || out.Kind != access.Once {
			t.Errorf("expected decide Once, got %+v", out)
		}
	default:
		t.Fatal("expected decide on outChan")
	}
	// request 3 の決着
	newM, _ = m.Update(socketMsg(Msg{Type: "settled", ID: 3, Status: access.Approved, Kind: access.Once}))
	m = newM.(model)

	// 質問
	newM, _ = m.Update(socketMsg(Msg{Type: "request", ID: 4, Domains: []string{"example.com"}}))
	m = newM.(model)
	newM, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m = newM.(model)
	if !m.asking {
		t.Fatal("expected asking=true")
	}
	// 文字列入力
	for _, r := range "何に使うの?" {
		newM, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = newM.(model)
	}
	newM, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = newM.(model)
	select {
	case out := <-m.outChan:
		if out.Type != "decide" || out.Status != access.Question || out.Question != "何に使うの?" {
			t.Errorf("expected question decision, got %+v", out)
		}
	default:
		t.Fatal("expected question on outChan")
	}
}

func TestTUIModelOnMsg(t *testing.T) {
	m := newModel("test.sock")

	// 1. log メッセージ
	newM, _ := m.Update(socketMsg(Msg{Type: "log", Text: "some log"}))
	m = newM.(model)
	if len(m.logs) != 1 {
		t.Fatalf("expected 1 log, got %d", len(m.logs))
	}

	// 2. request 追加
	newM, _ = m.Update(socketMsg(Msg{Type: "request", ID: 10, Domains: []string{"test.com"}, Reason: "fetch"}))
	m = newM.(model)
	if len(m.pending) != 1 {
		t.Fatalf("expected 1 pending item, got %d", len(m.pending))
	}

	// 重複追加は無視される
	newM, _ = m.Update(socketMsg(Msg{Type: "request", ID: 10}))
	m = newM.(model)
	if len(m.pending) != 1 {
		t.Fatalf("expected duplicate to be ignored, got %d", len(m.pending))
	}

	// 3. settled でキューから削除され履歴に追加される
	newM, _ = m.Update(socketMsg(Msg{Type: "settled", ID: 10, Status: access.Approved}))
	m = newM.(model)
	if len(m.pending) != 0 {
		t.Fatalf("expected pending to be empty after settled, got %d", len(m.pending))
	}
	if len(m.history) != 1 {
		t.Fatalf("expected 1 history item, got %d", len(m.history))
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

func TestRelaxationConsoleIntegration(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "console-relax.sock")
	m, err := access.NewManager(noApply{}, filepath.Join(t.TempDir(), "always.json"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(m, sock)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	enc, dec := json.NewEncoder(c), json.NewDecoder(c)
	if err := enc.Encode(Msg{Type: "ui"}); err != nil {
		t.Fatal(err)
	}

	// 申請を提出
	relReq, err := m.SubmitRelaxation(access.Relaxation{
		Host:      "api.github.com",
		Headers:   []string{"Authorization"},
		Methods:   []string{"POST"},
		AllowBody: true,
	}, "Testing relaxation in console")
	if err != nil {
		t.Fatal(err)
	}

	// UI に relaxrequest が届くのを待つ
	var reqMsg Msg
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		var msg Msg
		if err := dec.Decode(&msg); err != nil {
			continue
		}
		if msg.Type == "relaxrequest" && msg.ID == relReq.ID {
			reqMsg = msg
			break
		}
	}
	if reqMsg.ID != relReq.ID || reqMsg.RelaxHost != "api.github.com" {
		t.Fatalf("expected relaxrequest for api.github.com, got: %+v", reqMsg)
	}

	// UI から relaxdecide (Approved, Once) を返す
	if err := enc.Encode(Msg{Type: "relaxdecide", ID: reqMsg.ID, Status: access.Approved, Kind: access.Once}); err != nil {
		t.Fatal(err)
	}

	// UI に relaxsettled が届くのを待つ
	var settledMsg Msg
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		var msg Msg
		if err := dec.Decode(&msg); err != nil {
			continue
		}
		if msg.Type == "relaxsettled" && msg.ID == relReq.ID {
			settledMsg = msg
			break
		}
	}
	if settledMsg.ID != relReq.ID || settledMsg.Status != access.Approved {
		t.Fatalf("expected relaxsettled approved, got: %+v", settledMsg)
	}
}

func TestTUIModelRelaxation(t *testing.T) {
	m := newModel("test.sock")

	// 1. relaxrequest の受信
	newM, _ := m.Update(socketMsg(Msg{
		Type:         "relaxrequest",
		ID:           42,
		RelaxHost:    "api.openai.com",
		RelaxHeaders: []string{"Authorization"},
		RelaxMethods: []string{"POST"},
		AllowBody:    true,
		Reason:       "call LLM",
		Deadline:     "12:00:00",
	}))
	m = newM.(model)
	if len(m.pending) != 1 {
		t.Fatalf("expected 1 in pending, got %d", len(m.pending))
	}

	// 2. "1" (今回のみ)
	newM, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}})
	m = newM.(model)
	select {
	case out := <-m.outChan:
		if out.Type != "relaxdecide" || out.Status != access.Approved || out.Kind != access.Once {
			t.Errorf("expected relaxdecide Approved Once, got %+v", out)
		}
	default:
		t.Fatal("expected relaxdecide on outChan")
	}

	// 3. relaxsettled の受信
	newM, _ = m.Update(socketMsg(Msg{Type: "relaxsettled", ID: 42, Status: access.Approved, Kind: access.Once}))
	m = newM.(model)
	if len(m.pending) != 0 {
		t.Fatalf("expected empty pending after settled, got %d", len(m.pending))
	}
	if len(m.history) != 1 {
		t.Fatalf("expected 1 history item, got %d", len(m.history))
	}

	// 4. "2", "d", "q"
	newM, _ = m.Update(socketMsg(Msg{Type: "relaxrequest", ID: 43, RelaxHost: "api.slack.com"}))
	m = newM.(model)
	newM, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	m = newM.(model)
	select {
	case out := <-m.outChan:
		if out.Type != "relaxdecide" || out.Kind != access.Session || out.ID != 43 {
			t.Errorf("expected session relaxation, got %+v", out)
		}
	default:
		t.Fatal("expected session relaxation on outChan")
	}
	newM, _ = m.Update(socketMsg(Msg{Type: "relaxsettled", ID: 43, Status: access.Approved, Kind: access.Session}))
	m = newM.(model)

	newM, _ = m.Update(socketMsg(Msg{Type: "relaxrequest", ID: 44, RelaxHost: "api.slack.com"}))
	m = newM.(model)
	newM, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = newM.(model)
	select {
	case out := <-m.outChan:
		if out.Type != "relaxdecide" || out.Status != access.Denied || out.ID != 44 {
			t.Errorf("expected denied relaxation, got %+v", out)
		}
	default:
		t.Fatal("expected denied relaxation on outChan")
	}
	newM, _ = m.Update(socketMsg(Msg{Type: "relaxsettled", ID: 44, Status: access.Denied}))
	m = newM.(model)

	newM, _ = m.Update(socketMsg(Msg{Type: "relaxrequest", ID: 45, RelaxHost: "api.slack.com"}))
	m = newM.(model)
	newM, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m = newM.(model)
	if !m.asking {
		t.Error("expected asking state")
	}
	for _, r := range "何のエンドポイント?" {
		newM, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = newM.(model)
	}
	newM, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = newM.(model)
	select {
	case out := <-m.outChan:
		if out.Type != "relaxdecide" || out.Status != access.Question || out.Question != "何のエンドポイント?" || out.ID != 45 {
			t.Errorf("expected question relaxation, got %+v", out)
		}
	default:
		t.Fatal("expected question relaxation on outChan")
	}
}

func TestTUIModelTabSwitching(t *testing.T) {
	m := newModel("test.sock")
	if m.activeTab != tabPending {
		t.Errorf("initial tab = %d, want tabPending", m.activeTab)
	}

	// Tab キーで tabLogs へ
	newM, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = newM.(model)
	if m.activeTab != tabLogs {
		t.Errorf("after tab = %d, want tabLogs", m.activeTab)
	}

	// Tab キーで tabHistory へ
	newM, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = newM.(model)
	if m.activeTab != tabHistory {
		t.Errorf("after tab = %d, want tabHistory", m.activeTab)
	}

	// Tab キーで tabPending へ一巡
	newM, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = newM.(model)
	if m.activeTab != tabPending {
		t.Errorf("after tab = %d, want tabPending", m.activeTab)
	}

	// Shift+Tab で tabHistory へ逆順
	newM, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = newM.(model)
	if m.activeTab != tabHistory {
		t.Errorf("after shift+tab = %d, want tabHistory", m.activeTab)
	}

	// 数字キー 1 で tabPending へジャンプ
	newM, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}})
	m = newM.(model)
	if m.activeTab != tabPending {
		t.Errorf("after '1' = %d, want tabPending", m.activeTab)
	}
}

func TestTUIModelView(t *testing.T) {
	m := newModel("test.sock")
	// 初期化前
	if view := m.View(); !strings.Contains(view, "初期化中") {
		t.Errorf("unready view = %q", view)
	}

	// WindowSizeMsg でリサイズ
	newM, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m = newM.(model)

	// 1. 空の承認待ちタブ
	view := m.View()
	if !strings.Contains(view, "承認待ち") || !strings.Contains(view, "承認待ちの申請はありません") {
		t.Errorf("empty pending view unexpected: %q", view)
	}

	// 2. 申請あり
	newM, _ = m.Update(socketMsg(Msg{Type: "request", ID: 1, Domains: []string{"example.com"}, Reason: "fetch"}))
	m = newM.(model)
	view = m.View()
	if !strings.Contains(view, "ドメイン接続申請 #1") || !strings.Contains(view, "example.com") {
		t.Errorf("pending view unexpected: %q", view)
	}

	// 3. ログタブ
	newM, _ = m.Update(socketMsg(Msg{Type: "log", Text: "DNS で拒否: test.org"}))
	m = newM.(model)
	m.activeTab = tabLogs
	view = m.View()
	if !strings.Contains(view, "DNS で拒否: test.org") {
		t.Errorf("logs view unexpected: %q", view)
	}

	// 4. 履歴タブ
	newM, _ = m.Update(socketMsg(Msg{Type: "settled", ID: 1, Status: access.Approved, Kind: access.Once}))
	m = newM.(model)
	m.activeTab = tabHistory
	view = m.View()
	if !strings.Contains(view, "決着履歴") || !strings.Contains(view, "example.com") {
		t.Errorf("history view unexpected: %q", view)
	}

	// 5. 終了確認モーダル
	m.quiting = true
	view = m.View()
	if !strings.Contains(view, "VM を破棄して終了しますか?") {
		t.Errorf("quit modal view unexpected: %q", view)
	}
}

func TestTUIModelMouseTabSwitching(t *testing.T) {
	m := newModel("test.sock")
	newM, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m = newM.(model)

	if m.activeTab != tabPending {
		t.Fatalf("expected initial tab tabPending, got %d", m.activeTab)
	}

	// タブ2 (ログ) の位置 (X: 40, Y: 0) をクリック
	newM, _ = m.Update(tea.MouseMsg{X: 40, Y: 0, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = newM.(model)
	if m.activeTab != tabLogs {
		t.Fatalf("expected tabLogs after click, got %d", m.activeTab)
	}

	// タブ3 (履歴) の位置 (X: 52, Y: 0) をクリック
	newM, _ = m.Update(tea.MouseMsg{X: 52, Y: 0, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = newM.(model)
	if m.activeTab != tabHistory {
		t.Fatalf("expected tabHistory after click, got %d", m.activeTab)
	}

	// タブ1 (承認待ち) の位置 (X: 28, Y: 0) をクリック
	newM, _ = m.Update(tea.MouseMsg{X: 28, Y: 0, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = newM.(model)
	if m.activeTab != tabPending {
		t.Fatalf("expected tabPending after click, got %d", m.activeTab)
	}
}

func TestTUIModelMouseDecisions(t *testing.T) {
	m := newModel("test.sock")
	newM, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m = newM.(model)

	// 1. request 申請に対するマウスクリック ([1] 今回のみ)
	newM, _ = m.Update(socketMsg(Msg{Type: "request", ID: 10, Domains: []string{"example.com"}}))
	m = newM.(model)

	newM, _ = m.Update(tea.MouseMsg{X: 10, Y: 5, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = newM.(model)
	select {
	case out := <-m.outChan:
		if out.Type != "decide" || out.Kind != access.Once {
			t.Errorf("expected decide Once on mouse click, got %+v", out)
		}
	default:
		t.Fatal("expected decide on outChan after mouse click")
	}

	// 決着
	newM, _ = m.Update(socketMsg(Msg{Type: "settled", ID: 10, Status: access.Approved, Kind: access.Once}))
	m = newM.(model)

	// 2. clip 要求に対するマウスクリック ([y] コピー許可)
	newM, _ = m.Update(socketMsg(Msg{Type: "clip", ID: 11, Text: "clip test"}))
	m = newM.(model)

	newM, _ = m.Update(tea.MouseMsg{X: 10, Y: 5, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = newM.(model)
	select {
	case out := <-m.outChan:
		if out.Type != "clipdecide" || out.Status != access.Approved {
			t.Errorf("expected clipdecide Approved on mouse click, got %+v", out)
		}
	default:
		t.Fatal("expected clipdecide on outChan after mouse click")
	}
}

func TestTUIModelMouseScrollAndModals(t *testing.T) {
	m := newModel("test.sock")
	newM, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m = newM.(model)

	// ログタブに切り替えてスクロール
	m.activeTab = tabLogs
	newM, _ = m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelUp})
	m = newM.(model)
	if m.logAutoScroll {
		t.Error("expected logAutoScroll=false after wheel up")
	}

	// フッターで [?] ヘルプをクリック (X: 80, Y: 23)
	newM, _ = m.Update(tea.MouseMsg{X: 80, Y: 23, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = newM.(model)
	if !m.showHelp {
		t.Error("expected showHelp=true after clicking help in footer")
	}

	// 画面クリックでヘルプを閉じる
	newM, _ = m.Update(tea.MouseMsg{X: 50, Y: 10, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = newM.(model)
	if m.showHelp {
		t.Error("expected showHelp=false after clicking screen")
	}

	// フッターで [Q] 終了をクリック (X: 95, Y: 23)
	newM, _ = m.Update(tea.MouseMsg{X: 95, Y: 23, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = newM.(model)
	if !m.quiting {
		t.Error("expected quiting=true after clicking quit in footer")
	}

	// キャンセル (右半分をクリック)
	newM, _ = m.Update(tea.MouseMsg{X: 80, Y: 10, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = newM.(model)
	if m.quiting {
		t.Error("expected quiting=false after clicking cancel on quit modal")
	}
}
