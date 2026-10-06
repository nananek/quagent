package mcpsrv

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nananek/quagent/internal/config"
	"github.com/nananek/quagent/internal/guard"
)

// newGuard は action を返す偽のローカル LLM を使うコンテンツガードを作る。
func newGuard(t *testing.T, action string) *guard.Guard {
	t.Helper()
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{
				"content": `{"action":"` + action + `","reason":"理由","evidence":"秘密"}`,
			}}},
		})
	}))
	t.Cleanup(model.Close)
	g, err := guard.New(config.Guard{Backend: "openai", Endpoint: model.URL, Model: "m", Mode: "deny"},
		log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// PR のタイトル・本文もコンテンツガードに通る (deny なら止まる、allow なら通る)。
func TestGuardPR(t *testing.T) {
	in := prIn{Branch: "feature", Title: "t", Body: "秘密をここに書く"}
	if err := guardPR(context.Background(), newGuard(t, "deny"), in); err == nil {
		t.Error("deny の PR を通した")
	}
	if err := guardPR(context.Background(), newGuard(t, "allow"), in); err != nil {
		t.Errorf("allow の PR を止めた: %v", err)
	}
}

// ガードが無効 (nil) なら何もしない。
func TestGuardPRDisabled(t *testing.T) {
	if err := guardPR(context.Background(), nil, prIn{Branch: "b", Title: "t"}); err != nil {
		t.Errorf("nil のガードで止めた: %v", err)
	}
}
