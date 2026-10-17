package hub

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nananek/quagent/internal/console"
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

// TestBrowserVisible は再発行 URL をブラウザへ中継しないことを確かめる。
func TestBrowserVisible(t *testing.T) {
	if browserVisible(console.Msg{Type: "hubreissued", URL: "http://host/s/x"}) {
		t.Error("hubreissued がブラウザに見えている")
	}
	for _, typ := range []string{"request", "settled", "log", "clip", "prrequest"} {
		if !browserVisible(console.Msg{Type: typ}) {
			t.Errorf("%s は中継すべき", typ)
		}
	}
}

func TestIsDecision(t *testing.T) {
	for _, t2 := range []string{"decide", "relaxdecide", "prdecide", "dockerdecide", "batchdecide", "clipdecide"} {
		if !isDecision(t2) {
			t.Errorf("%s は判断として通すべき", t2)
		}
	}
	for _, t2 := range []string{"ui", "quit", "clipboard", "log", "hubreissue", ""} {
		if isDecision(t2) {
			t.Errorf("%s は通してはいけない", t2)
		}
	}
}

// TestSecurityHeaders は CSP などの防御ヘッダが全応答に付き、CSP が
// eval を許していないことを確認する。
func TestSecurityHeaders(t *testing.T) {
	s := newTestServer()
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/") // Cookie 無しで 401 でもヘッダは付く
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	csp := resp.Header.Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("CSP が無い")
	}
	for _, want := range []string{"script-src 'self'", "default-src 'none'", "frame-ancestors 'none'", "object-src 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP に %q が無い: %s", want, csp)
		}
	}
	if strings.Contains(csp, "'unsafe-eval'") {
		t.Errorf("CSP が unsafe-eval を許している: %s", csp)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options: %q", got)
	}
	if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy: %q", got)
	}
}

// TestAssetsHaveNoInlineCode は同梱 HTML にインラインの script/style が無いことを
// 確認する (CSP を緩めないための回帰ガード)。
func TestAssetsHaveNoInlineCode(t *testing.T) {
	b, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	if strings.Contains(html, "<style") {
		t.Error("index.html にインラインの <style> がある (style-src を緩める必要が出る)")
	}
	if strings.Contains(html, "<script>") || strings.Contains(html, "<script type") {
		t.Error("index.html にインラインの <script> がある (script-src 'self' で動かない)")
	}
}

// TestAppJSInitializesAfterState は端末の初期化が状態変数の宣言より前に走らないことを
// 確かめる (let の TDZ で端末が表示されなくなった回帰ガード)。
func TestAppJSInitializesAfterState(t *testing.T) {
	b, err := assetsFS.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	decl := strings.Index(s, "let termWS = null;")
	call := strings.Index(s, "initTerminal();")
	if decl < 0 {
		t.Fatal("app.js に termWS の宣言が無い")
	}
	if call < 0 {
		t.Fatal("app.js に initTerminal() の呼び出しが無い")
	}
	if decl > call {
		t.Error("initTerminal() が termWS の宣言より前に呼ばれている (TDZ で ReferenceError になる)")
	}
}

// TestAssetServing は同梱資産が正しいパスで配信されることを確認する
// (FileServer のプレフィックス取り違えの回帰ガード)。
func TestAssetServing(t *testing.T) {
	s := newTestServer()
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	for _, tc := range []struct{ path, ctype string }{
		{"/assets/app.css", "text/css"},
		{"/assets/app.js", "javascript"},
		{"/assets/xterm.js", "javascript"},
		{"/assets/xterm.css", "text/css"},
		{"/assets/addon-fit.js", "javascript"},
	} {
		resp, err := http.Get(ts.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		got := resp.Header.Get("Content-Type")
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s -> %d", tc.path, resp.StatusCode)
			continue
		}
		if !strings.Contains(got, tc.ctype) {
			t.Errorf("%s Content-Type = %q", tc.path, got)
		}
		if len(body) < 10 {
			t.Errorf("%s が小さすぎる: %d", tc.path, len(body))
		}
	}

	// 許可リストに無いもの (テンプレート・README) は配信しない。
	for _, p := range []string{"/assets/index.html", "/assets/README.md", "/assets/", "/assets/../go.mod"} {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s が配信されている: %d", p, resp.StatusCode)
		}
	}
}

// TestExchangeMethodAndOracle は交換が GET 限定で、失敗応答が一律であることを確認する。
func TestExchangeMethodAndOracle(t *testing.T) {
	s := newTestServer()
	sess, err := newSession("r1", "n", "/repo", "opencode", "quagent-r1", "/tmp/c.sock")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.reg.add(sess); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	// HEAD は安全メソッドなのでシークレットを消費しない。
	resp, err := http.Head(ts.URL + "/s/" + sess.secret)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("HEAD は 405 のはず: %d", resp.StatusCode)
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err = client.Get(ts.URL + "/s/" + sess.secret)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("HEAD の後に正規の GET ができない: %d", resp.StatusCode)
	}

	// 使用済みと未知の応答は区別できない。
	used := getBody(t, client, ts.URL+"/s/"+sess.secret)
	unknown := getBody(t, client, ts.URL+"/s/"+strings.Repeat("Z", 43))
	if used != unknown {
		t.Errorf("使用済みと未知で応答が異なる: %q vs %q", used, unknown)
	}
}

// TestExchangeFailuresDoNotBlockValid は失敗の連投が有効な交換を止めないことを確認する。
func TestExchangeFailuresDoNotBlockValid(t *testing.T) {
	s := newTestServer()
	sess, err := newSession("r1", "n", "/repo", "opencode", "quagent-r1", "/tmp/c.sock")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.reg.add(sess); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	for i := 0; i < 100; i++ {
		resp, err := http.Get(ts.URL + "/s/" + strings.Repeat("Y", 43))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(ts.URL + "/s/" + sess.secret)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("失敗の連投後に有効な交換が拒否された: %d", resp.StatusCode)
	}
}

func getBody(t *testing.T, c *http.Client, url string) string {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
