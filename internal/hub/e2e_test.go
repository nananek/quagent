package hub

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestServeRegisterAndExchange は hub を実際に起動し、run の登録からワンタイム URL の
// 交換、Cookie 認可、セッション情報の取得までを通しで検証する。
func TestServeRegisterAndExchange(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "hub.sock")
	s := New(Config{Addr: "127.0.0.1:0", BaseURL: "http://127.0.0.1:0", Logger: func(string) {}})

	served := make(chan error, 1)
	go func() { served <- s.Serve(sock) }()
	t.Cleanup(s.Close)

	// 待ち受けが始まるまで待つ。
	deadline := time.Now().Add(3 * time.Second)
	for s.Addr() == "127.0.0.1:0" || s.Addr() == "" {
		if time.Now().After(deadline) {
			t.Fatal("待ち受けが始まらない")
		}
		time.Sleep(10 * time.Millisecond)
	}
	base := "http://" + s.Addr()

	reg, err := Register(sock, Request{
		ID: "r1", Name: "repo", Repo: "/repo", Agent: "opencode",
		Session: "quagent-r1", Console: "/tmp/c.sock",
	}, 2*time.Second)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer reg.Close()

	secret := reg.URL()[strings.LastIndex(reg.URL(), "/s/")+len("/s/"):]
	if secret == "" {
		t.Fatalf("URL からシークレットを取り出せない: %q", reg.URL())
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	// 交換して Cookie を得る。
	resp, err := client.Get(base + "/s/" + secret)
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
	if cookie == nil {
		t.Fatal("Cookie が無い")
	}

	// 認可付きでセッション情報が取れる。
	req, _ := http.NewRequest(http.MethodGet, base+"/api/session", nil)
	req.AddCookie(cookie)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/session は 200 のはず: %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	var info map[string]string
	if err := json.Unmarshal(b, &info); err != nil {
		t.Fatal(err)
	}
	if info["id"] != "r1" || info["agent"] != "opencode" {
		t.Errorf("セッション情報: %v", info)
	}

	// 登録が切れると Serve は動き続けたまま登録だけが消える。
	reg.Close()
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := s.reg.get("r1"); !ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("登録が解除されない")
}
