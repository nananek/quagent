package authproxy

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/nananek/quagent/internal/config"
	"github.com/nananek/quagent/internal/guard"
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

func TestGuardBlocksBeforeUpstream(t *testing.T) {
	var reached []string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = append(reached, r.Method+" "+r.URL.Path) })
	rules, _ := parseRules([]string{"POST /messages"})
	var denied []string
	h := gate("p", "up.example", rules, next, log.New(io.Discard, "", 0), func(s string) { denied = append(denied, s) }, nil)

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
	}, log.New(io.Discard, "", 0), nil, nil)
	if err == nil {
		t.Fatal("不正な allow を受け付けた")
	}
}

// コンテンツガードが拒否すれば upstream へ届かず、通せば本文を保ったまま届く。
func TestGateContentGuard(t *testing.T) {
	action := "deny"
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{
				"content": `{"action":"` + action + `","reason":"秘密が漏れる","evidence":"hello"}`,
			}}},
		})
	}))
	defer model.Close()

	g, err := guard.New(config.Guard{Backend: "openai", Endpoint: model.URL, Model: "m", Mode: "deny"},
		log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	rules, _ := parseRules([]string{"POST /v1/chat/completions"})
	reached := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		b, _ := io.ReadAll(r.Body)
		if string(b) != "world" {
			t.Errorf("upstream に届いた本文が違う: %q", b)
		}
	})
	var denied []string
	h := gate("p", "up.example", rules, next, log.New(io.Discard, "", 0), func(s string) { denied = append(denied, s) }, g)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/llm/p/v1/chat/completions", strings.NewReader("hello")))
	if rec.Code != http.StatusForbidden || reached != 0 || len(denied) != 1 {
		t.Fatalf("拒否されなかった: code=%d reached=%d denied=%v", rec.Code, reached, denied)
	}

	action = "allow"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/llm/p/v1/chat/completions", strings.NewReader("world")))
	if rec.Code != http.StatusOK || reached != 1 {
		t.Fatalf("通らなかった: code=%d reached=%d body=%s", rec.Code, reached, rec.Body.String())
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
