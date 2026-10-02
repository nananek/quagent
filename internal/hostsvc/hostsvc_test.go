package hostsvc

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTokenRequired(t *testing.T) {
	s, err := New("unused")
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
