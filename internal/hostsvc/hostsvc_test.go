package hostsvc

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
