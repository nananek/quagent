package hostsvc

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
