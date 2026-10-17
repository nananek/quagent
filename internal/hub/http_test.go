package hub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestExchangeAndAuth はワンタイム URL の交換と Cookie 認可、Host 検査を検証する。
func TestExchangeAndAuth(t *testing.T) {
	s := newTestServer()
	sess, err := newSession("r1", "name", "/repo", "opencode", "quagent-r1", "/tmp/c.sock")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.reg.add(sess); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	// Cookie 無しでは 401。
	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Cookie 無しの / は 401 のはず: %d", resp.StatusCode)
	}

	// ワンタイム URL は 302 と Cookie を返す。
	resp, err = client.Get(ts.URL + "/s/" + sess.secret)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("交換は 302 のはず: %d", resp.StatusCode)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly {
		t.Fatalf("Cookie が発行されていない: %+v", resp.Cookies())
	}

	// 同じ URL の二度目は無効。
	resp, err = client.Get(ts.URL + "/s/" + sess.secret)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("使用済み URL は 404 のはず: %d", resp.StatusCode)
	}

	// 存在しないシークレットも 404。
	resp, err = client.Get(ts.URL + "/s/deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未知のシークレットは 404 のはず: %d", resp.StatusCode)
	}

	// Cookie があれば 200。
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	req.AddCookie(cookie)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 1024)
	n, _ := resp.Body.Read(body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Cookie 付きの / は 200 のはず: %d", resp.StatusCode)
	}
	if !strings.Contains(string(body[:n]), "quagent") {
		t.Errorf("HTML に quagent が無い: %q", string(body[:n]))
	}

	// 余所の Host は 403 (DNS リバインディング対策)。
	req2, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	req2.Host = "evil.example.com"
	req2.AddCookie(cookie)
	resp, err = client.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("余所の Host は 403 のはず: %d", resp.StatusCode)
	}

	// 未知の Cookie は 401。
	req3, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	req3.AddCookie(&http.Cookie{Name: cookieName, Value: "bogus"})
	resp, err = client.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("不正な Cookie は 401 のはず: %d", resp.StatusCode)
	}
}

func TestIsDecision(t *testing.T) {
	for _, t2 := range []string{"decide", "relaxdecide", "prdecide", "dockerdecide", "batchdecide", "clipdecide"} {
		if !isDecision(t2) {
			t.Errorf("%s は判断として通すべき", t2)
		}
	}
	for _, t2 := range []string{"ui", "quit", "clipboard", "log", ""} {
		if isDecision(t2) {
			t.Errorf("%s は通してはいけない", t2)
		}
	}
}
