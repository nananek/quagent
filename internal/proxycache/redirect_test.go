package proxycache

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestUpstreamRedirectToInternalRejected は、upstream のリダイレクトが内部アドレスへ
// 向かう場合に追従しないことを確かめる (SSRF 防止)。公開ホスト (CDN) への
// リダイレクトはこれまでどおり通る。
func TestUpstreamRedirectToInternalRejected(t *testing.T) {
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("SECRET-INTERNAL-BODY"))
	}))
	defer internal.Close()

	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"t","expires_in":300}`))
			return
		}
		http.Redirect(w, r, internal.URL+"/metadata", http.StatusFound)
	}))
	defer redir.Close()

	u := NewBaseUpstream("test", redir.URL, redir.URL+"/token", "svc", nil)
	resp, err := u.GetManifest(context.Background(), "library/x", "latest", http.Header{})
	if err != nil {
		return // 追従を拒否できていればエラーでよい
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(b), "SECRET-INTERNAL-BODY") {
		t.Fatalf("内部アドレスへリダイレクトを追従した: %q", string(b))
	}
}
