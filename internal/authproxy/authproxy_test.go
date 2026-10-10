package authproxy

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/nananek/quagent/internal/config"
)

func TestAllowedRules(t *testing.T) {
	rules, err := parseRules(DefaultAllow)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		method, path string
		want         bool
	}{
		{"POST", "/v1/messages", true},
		{"POST", "/chat/completions", true},
		{"GET", "/v1/models", true},
		{"GET", "/v1/models/claude-x", true},
		{"HEAD", "/api/hello", true},
		{"POST", "/v1beta/models/gemini-3.8-flash:streamGenerateContent", true},
		{"POST", "/v1beta/models/gemini-3.8-flash:generateContent", true},
		{"GET", "/v1beta/models", true},
		{"GET", "/v1beta/models/gemini-3.8-flash", true},
		{"GET", "/v1/messages", false},
		{"DELETE", "/v1/files/f", false},
		{"GET", "/v1/files", false},
		{"POST", "/v1/messages/batches", false},
		{"POST", "/v1/messagesx", false},
	} {
		if got := allowed(rules, c.method, c.path); got != c.want {
			t.Errorf("%s %s = %v", c.method, c.path, got)
		}
	}
	if _, err := parseRules([]string{"post /x"}); err == nil {
		t.Error("小文字のメソッドを受け付けた")
	}
	if _, err := parseRules([]string{"POST x"}); err == nil {
		t.Error("/ で始まらないパスを受け付けた")
	}
}

func TestGateBlocksDisallowed(t *testing.T) {
	var reached []string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = append(reached, r.Method+" "+r.URL.Path) })
	rules, _ := parseRules([]string{"POST /messages"})
	var denied []string
	h := gate("p", rules, next, log.New(io.Discard, "", 0), func(s string) { denied = append(denied, s) })

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/llm/p/files/all", nil))
	if rec.Code != http.StatusForbidden || len(reached) != 0 || len(denied) != 1 {
		t.Fatalf("code=%d reached=%v denied=%v", rec.Code, reached, denied)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/llm/p/messages?beta=true", nil))
	if len(reached) != 1 {
		t.Fatalf("許可した操作が届かない: code=%d", rec.Code)
	}
}

func TestRegisterRejectsBadAllow(t *testing.T) {
	_, err := Register(http.NewServeMux(), map[string]config.Provider{
		"p": {Upstream: "https://example.com/v1", SecretEnv: "QUAGENT_TEST_UNSET", Allow: []string{"bad"}},
	}, log.New(io.Discard, "", 0), nil)
	if err == nil {
		t.Fatal("不正な allow を受け付けた")
	}
}

func TestHandlerOnResponse(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer up.Close()
	uu, _ := url.Parse(up.URL)
	var got []string
	h := handler("p", uu, "Authorization", func() (string, error) { return "Bearer x", nil },
		log.New(io.Discard, "", 0),
		func(id, method, path string, status int) { got = append(got, id+" "+method+" "+path) })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/llm/p/v1internal:streamGenerateContent", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if len(got) != 1 || got[0] != "p POST /v1internal:streamGenerateContent" {
		t.Fatalf("onResponse が呼ばれない: %v", got)
	}
	// nil でも動く
	h2 := handler("p", uu, "Authorization", func() (string, error) { return "Bearer x", nil },
		log.New(io.Discard, "", 0), nil)
	rec = httptest.NewRecorder()
	h2.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/llm/p/v1internal:streamGenerateContent", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestGuestBaseURL(t *testing.T) {
	if got := GuestBaseURL("http://quagent.host:7070", "gemini"); got != "http://quagent.host:7070/llm/gemini" {
		t.Errorf("GuestBaseURL = %q, want 'http://quagent.host:7070/llm/gemini'", got)
	}
}

func TestRegisterSecretError(t *testing.T) {
	mux := http.NewServeMux()
	// 秘密の取り出し方法が未指定
	_, err := Register(mux, map[string]config.Provider{
		"test": {Upstream: "https://api.example.com"},
	}, log.New(io.Discard, "", 0), nil)
	if err == nil {
		t.Fatal("expected error when secret cannot be retrieved")
	}
}

func TestRegisterSuccess(t *testing.T) {
	t.Setenv("TEST_AUTHPROXY_KEY", "secret-key")
	mux := http.NewServeMux()
	ids, err := Register(mux, map[string]config.Provider{
		"provider1": {
			Upstream:  "https://api.example.com",
			SecretEnv: "TEST_AUTHPROXY_KEY",
		},
	}, log.New(io.Discard, "", 0), nil)
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	if len(ids) != 1 || ids[0] != "provider1" {
		t.Errorf("registered ids = %v, want ['provider1']", ids)
	}
}

func TestRegisterDynamicBadUpstream(t *testing.T) {
	mux := http.NewServeMux()
	// http:// は拒否される (https でなければならない)
	err := RegisterDynamic(mux, "test", "http://insecure.example.com", "Authorization",
		func() (string, error) { return "tok", nil }, nil, log.New(io.Discard, "", 0), nil, nil)
	if err == nil {
		t.Fatal("expected error for non-https upstream")
	}

	// ホスト名なし
	err = RegisterDynamic(mux, "test", "https://", "Authorization",
		func() (string, error) { return "tok", nil }, nil, log.New(io.Discard, "", 0), nil, nil)
	if err == nil {
		t.Fatal("expected error for empty host upstream")
	}
}

func TestZenAliasesAndDeduplication(t *testing.T) {
	t.Setenv("TEST_AUTHPROXY_KEY", "secret-key")
	mux := http.NewServeMux()
	var denied []string
	ids, err := Register(mux, map[string]config.Provider{
		"opencode": {
			Upstream:  "https://opencode.ai/zen/v1",
			SecretEnv: "TEST_AUTHPROXY_KEY",
		},
		"opencode-go": {
			Upstream:  "https://opencode.ai/zen/go/v1",
			SecretEnv: "TEST_AUTHPROXY_KEY",
		},
	}, log.New(io.Discard, "", 0), func(s string) {
		denied = append(denied, s)
	})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 ids, got %v", ids)
	}

	// 不正な操作 (DELETE) を送ると、/zen/ は opencode、/zen/go/ は opencode-go のゲートで 403 になる
	recZen := httptest.NewRecorder()
	mux.ServeHTTP(recZen, httptest.NewRequest(http.MethodDelete, "/zen/files/all", nil))
	if recZen.Code != http.StatusForbidden {
		t.Errorf("/zen alias expected 403, got %d", recZen.Code)
	}

	recGo := httptest.NewRecorder()
	mux.ServeHTTP(recGo, httptest.NewRequest(http.MethodDelete, "/zen/go/files/all", nil))
	if recGo.Code != http.StatusForbidden {
		t.Errorf("/zen/go alias expected 403, got %d", recGo.Code)
	}

	if len(denied) != 2 || denied[0] != "opencode DELETE \"/files/all\"" || denied[1] != "opencode-go DELETE \"/files/all\"" {
		t.Errorf("unexpected denied log: %v", denied)
	}

	// handler の v1 重複除去を検証
	upURL, _ := url.Parse("https://opencode.ai/zen/go/v1")
	h := handler("opencode-go", upURL, "Authorization", func() (string, error) { return "Bearer tok", nil },
		log.New(io.Discard, "", 0), nil)

	// /v1/chat/completions を送っても /zen/go/v1/chat/completions (重複なし) になることを確認
	rp, ok := h.(*httputil.ReverseProxy)
	if !ok {
		t.Fatal("handler is not *httputil.ReverseProxy")
	}
	req := httptest.NewRequest(http.MethodPost, "/llm/opencode-go/v1/chat/completions", nil)
	prReq := &httputil.ProxyRequest{
		In:  req,
		Out: req.Clone(req.Context()),
	}
	rp.Rewrite(prReq)
	if prReq.Out.URL.Path != "/zen/go/v1/chat/completions" {
		t.Errorf("deduplicated path = %q, want '/zen/go/v1/chat/completions'", prReq.Out.URL.Path)
	}
}
