package mcpsrv

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type dummyPublisher struct{}

func (dummyPublisher) Publish(in prIn) (any, error) {
	return nil, nil
}

func TestHandlerCreatesServer(t *testing.T) {
	h := Handler(nil, nil, func(string) {})
	if h == nil {
		t.Fatal("Handler returned nil")
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	h.ServeHTTP(rec, req)
	// Server responds (GET might return 405 Method Not Allowed or 400 for SSE/stateless stream)
	if rec.Code == 0 {
		t.Fatal("response code was 0")
	}
}
