package proxycache

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNPMRejectsEncodedTraversal は、r.URL.Path (net/http が既に 1 回デコード済み) を
// 二重 percent-decode しないことを確かめる。%252e%252e が .. に化けて上流 URL へ
// 注入される回帰を防ぐ。
func TestNPMRejectsEncodedTraversal(t *testing.T) {
	storage, err := NewStorage(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	var upstreamURIs []string
	srv := NewServer(storage, nil, nil)
	srv.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		upstreamURIs = append(upstreamURIs, r.URL.String())
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader("{}")),
		}, nil
	})}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/npm/%252e%252e/%252e%252e/whatever", nil))

	for _, u := range upstreamURIs {
		if strings.Contains(u, "..") {
			t.Fatalf("上流 URL に .. が注入された: %s (二重 percent-decode)", u)
		}
	}
}
