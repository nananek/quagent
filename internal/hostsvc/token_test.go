package hostsvc

import (
	"net/http/httptest"
	"testing"
)

// 空の合言葉は一致とみなさない (ConstantTimeCompare("","") で認証が素通りするのを防ぐ)。
func TestAuthorizedRejectsEmptyToken(t *testing.T) {
	s, err := New(2)
	if err != nil {
		t.Fatal(err)
	}
	s.ExtraTokens = []string{""}
	if s.authorized(httptest.NewRequest("GET", "/x", nil)) {
		t.Fatal("認証ヘッダ無しで通った")
	}
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "Bearer ")
	if s.authorized(req) {
		t.Fatal("空の Bearer で通った")
	}
	// 正しい合言葉は通る。
	ok := httptest.NewRequest("GET", "/x", nil)
	ok.Header.Set("Authorization", "Bearer "+s.Token)
	if !s.authorized(ok) {
		t.Fatal("正しい合言葉を拒否した")
	}
}
