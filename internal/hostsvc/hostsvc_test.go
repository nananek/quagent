package hostsvc

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mdlayher/vsock"
)

func TestTokenRequired(t *testing.T) {
	s, err := New(3)
	if err != nil {
		t.Fatal(err)
	}
	s.Mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	h := s.handler()
	cases := []struct {
		path, header, value string
		want                int
	}{
		{"/healthz", "", "", 200},
		{"/mcp", "", "", 401},
		{"/mcp", "Authorization", "Bearer wrong", 401},
		{"/mcp", "Authorization", "Bearer " + s.Token, 204},
		{"/mcp", "X-Api-Key", s.Token, 204},
		{"/mcp", "Authorization", s.Token, 204}, // Bearer なしでも同じ値なら通す
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", c.path, nil)
		if c.header != "" {
			req.Header.Set(c.header, c.value)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s %s=%q: got %d want %d", c.path, c.header, c.value, rec.Code, c.want)
		}
	}
}

func TestExtraTokens(t *testing.T) {
	s, err := New(3)
	if err != nil {
		t.Fatal(err)
	}
	s.ExtraTokens = []string{"seed-token"}
	s.Mux.HandleFunc("/llm/antigravity", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	h := s.handler()
	cases2 := []struct {
		name, value string
		want        int
	}{
		{"window", "Bearer " + s.Token, 204},
		{"seed", "Bearer seed-token", 204},
		{"wrong", "Bearer wrong", 401},
	}
	for _, c := range cases2 {
		req := httptest.NewRequest("GET", "/llm/antigravity", nil)
		req.Header.Set("Authorization", c.value)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: got %d want %d", c.name, rec.Code, c.want)
		}
	}
}

func TestAuditRecordsAllRequests(t *testing.T) {
	s, err := New(3)
	if err != nil {
		t.Fatal(err)
	}
	s.Mux.HandleFunc("/llm/x", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello"))
	})
	var events []AuditEvent
	s.Audit = func(e AuditEvent) { events = append(events, e) }
	h := s.handler()

	// 合言葉が無い試し (401) も記録されること
	req := httptest.NewRequest("POST", "/llm/x", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	// 正しい合言葉つき (200 とバイト数)
	req = httptest.NewRequest("POST", "/llm/x", nil)
	req.Header.Set("Authorization", "Bearer "+s.Token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if len(events) != 2 {
		t.Fatalf("events=%d want 2: %+v", len(events), events)
	}
	if events[0].Path != "/llm/x" || events[0].Status != 401 {
		t.Fatalf("401 が記録されていない: %+v", events[0])
	}
	if events[1].Status != 200 || events[1].Bytes != int64(len("hello")) || events[1].Method != "POST" {
		t.Fatalf("200 の記録が変: %+v", events[1])
	}
}

// TestAuditLineEscapesControlChars は、VM が決められる path に改行・制御文字を
// 混ぜて host.log に偽の行を足せないことを確かめる。
func TestAuditLineEscapesControlChars(t *testing.T) {
	e := AuditEvent{
		Method: "GET",
		Path:   "/ok\nfake audit: GET /evil -> 200",
		Status: 200,
		Bytes:  1,
		Took:   1500 * time.Microsecond,
	}
	line := e.LogLine()
	if !strings.HasPrefix(line, "audit: ") {
		t.Fatalf("接頭辞が無い: %q", line)
	}
	if strings.ContainsAny(line, "\n\r") {
		t.Fatalf("制御文字が生のまま出ている: %q", line)
	}
	if !strings.Contains(line, `\n`) {
		t.Fatalf("改行がエスケープされていない: %q", line)
	}
}

// hijackableWriter は http.Hijacker を実装した ResponseWriter (Unwrap の確認用)。
type hijackableWriter struct {
	http.ResponseWriter
	hijacked bool
}

func (w *hijackableWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijacked = true
	return nil, nil, nil
}

// TestAuditWriterUnwrap は、包んだ auditWriter 越しでも http.ResponseController が
// 元の ResponseWriter の追加機能 (Hijacker など) に届くことを確かめる。
func TestAuditWriterUnwrap(t *testing.T) {
	base := &hijackableWriter{ResponseWriter: httptest.NewRecorder()}
	w := &auditWriter{ResponseWriter: base}
	if _, _, err := http.NewResponseController(w).Hijack(); err != nil {
		t.Fatalf("包み越しに元の ResponseWriter へ届かない: %v", err)
	}
	if !base.hijacked {
		t.Fatal("元の ResponseWriter が呼ばれていない")
	}
}

func TestGuestOrigin(t *testing.T) {
	if got := GuestOrigin(); got != "http://quagent.host:7070" {
		t.Errorf("GuestOrigin() = %q, want 'http://quagent.host:7070'", got)
	}
}

func TestServerHandler(t *testing.T) {
	s, err := New(3)
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	if h == nil {
		t.Fatal("Handler() returned nil")
	}
	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("expected 200 from /healthz via Handler(), got %d", rec.Code)
	}
}

type flushableRecorder struct {
	*httptest.ResponseRecorder
	flushed bool
}

func (f *flushableRecorder) Flush() {
	f.flushed = true
}

func TestAuditWriterFlush(t *testing.T) {
	base := &flushableRecorder{ResponseRecorder: httptest.NewRecorder()}
	w := &auditWriter{ResponseWriter: base}
	w.Flush()
	if !base.flushed {
		t.Error("expected base flusher to be called")
	}
}

func TestServerStopNil(t *testing.T) {
	s, err := New(3)
	if err != nil {
		t.Fatal(err)
	}
	// s.srv が nil の状態で Stop を呼んでもパニックしない
	s.Stop()
}

func TestReleaseConnCloseOnce(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()

	releases := 0
	rc := &releaseConn{
		Conn: client,
		release: func() {
			releases++
		},
	}

	if err := rc.Close(); err != nil {
		t.Fatalf("first close failed: %v", err)
	}
	// 2回目のクローズ
	_ = rc.Close()

	if releases != 1 {
		t.Errorf("expected release to be called exactly once, got %d", releases)
	}
}

type mockVsockConn struct {
	net.Conn
	addr *vsock.Addr
}

func (c *mockVsockConn) RemoteAddr() net.Addr { return c.addr }

type mockListener struct {
	conns chan net.Conn
}

func (m *mockListener) Accept() (net.Conn, error) {
	c, ok := <-m.conns
	if !ok {
		return nil, net.ErrClosed
	}
	return c, nil
}

func (m *mockListener) Close() error   { close(m.conns); return nil }
func (m *mockListener) Addr() net.Addr { return &vsock.Addr{} }

func TestGuardListener(t *testing.T) {
	ml := &mockListener{conns: make(chan net.Conn, 5)}
	var rejected []string
	gl := &guardListener{
		Listener: ml,
		cid:      10,
		sem:      make(chan struct{}, 1), // 同時接続上限 1
		onReject: func(reason string, remote net.Addr) {
			rejected = append(rejected, reason)
		},
	}

	p1, p2 := net.Pipe()
	p3, p4 := net.Pipe()
	p5, p6 := net.Pipe()
	p7, p8 := net.Pipe()

	defer p1.Close()
	defer p3.Close()
	defer p5.Close()
	defer p7.Close()

	// 0. vsock 以外のアドレス (!ok) -> スキップされて破棄
	ml.conns <- p8
	// 1. 別の CID (5) -> スキップされて破棄
	ml.conns <- &mockVsockConn{Conn: p2, addr: &vsock.Addr{ContextID: 5}}
	// 2. 正しい CID (10) -> 接続成功
	ml.conns <- &mockVsockConn{Conn: p4, addr: &vsock.Addr{ContextID: 10}}

	c, err := gl.Accept()
	if err != nil {
		t.Fatalf("Accept() error: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil conn")
	}

	// 3. セマフォ上限に達しているため、次の接続 (CID 10) は拒否されてCloseされる
	// 別スレッドで Close させて Accept を終了可能にする
	ml.conns <- &mockVsockConn{Conn: p6, addr: &vsock.Addr{ContextID: 10}}
	go func() {
		time.Sleep(50 * time.Millisecond)
		ml.Close()
	}()
	_, _ = gl.Accept()

	// c をクローズするとセマフォが解放される
	_ = c.Close()

	if len(rejected) != 3 {
		t.Fatalf("expected 3 reject events, got %d: %v", len(rejected), rejected)
	}
	if !strings.Contains(rejected[0], "不正な CID") {
		t.Errorf("reject[0] = %q, want CID mismatch for non-vsock addr", rejected[0])
	}
	if !strings.Contains(rejected[1], "不正な CID") {
		t.Errorf("reject[1] = %q, want CID mismatch", rejected[1])
	}
	if !strings.Contains(rejected[2], "同時接続上限") {
		t.Errorf("reject[2] = %q, want max conns exceeded", rejected[2])
	}
}

func TestGuardListenerNilOnReject(t *testing.T) {
	ml := &mockListener{conns: make(chan net.Conn, 5)}
	gl := &guardListener{
		Listener: ml,
		cid:      10,
		sem:      make(chan struct{}, 1),
	}

	p1, p2 := net.Pipe()
	p3, p4 := net.Pipe()
	p5, p6 := net.Pipe()
	defer p1.Close()
	defer p3.Close()
	defer p5.Close()

	// 1. vsock 以外のアドレス (!ok) -> onReject=nil でも安全に切断
	ml.conns <- p2
	// 2. 別の CID (5) -> onReject=nil でも安全に切断
	ml.conns <- &mockVsockConn{Conn: p4, addr: &vsock.Addr{ContextID: 5}}
	// 3. 正しい CID (10) -> 受付成功
	ml.conns <- &mockVsockConn{Conn: p6, addr: &vsock.Addr{ContextID: 10}}

	c, err := gl.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
}
