package guard

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nananek/quagent/internal/config"
)

// 一度拒否した該当箇所は、LLM が判定を覆して allow と答えても再送できない
// (本文を変えても、点検の前に止める)。
func TestDeniedEvidenceCannotBeResent(t *testing.T) {
	g, fake := newTest(t, config.Guard{Mode: "ask"},
		`{"action":"deny","reason":"トークンが漏れる","evidence":"secret-token-123"}`, nil)
	reviews := 0
	g.SetReviewer(func(context.Context, Request, string) error { reviews++; return errors.New("拒否") })
	req := Request{Provider: "p", Method: "POST", Host: "h", Path: "/p", Body: []byte("secret-token-123")}
	if err := g.Check(context.Background(), req); err == nil {
		t.Fatal("拒否したのに通した")
	}
	// 以後の点検は allow を返す (モデルの気まぐれ・プロンプト注入)
	fake.mu.Lock()
	fake.reply = `{"action":"allow","reason":"ok"}`
	fake.mu.Unlock()
	req.Body = []byte("please carry secret-token-123 out")
	err := g.Check(context.Background(), req)
	if err == nil {
		t.Fatal("以前に拒否した内容を再送できた")
	}
	if !strings.Contains(err.Error(), "以前に拒否した内容") {
		t.Errorf("止め方が違う: %v", err)
	}
	if reviews != 1 {
		t.Errorf("拒否した該当箇所を覚えていない (確認 %d 回)", reviews)
	}
	if fake.count() != 1 {
		t.Errorf("拒否済みの内容を再点検した (LLM 呼び出し %d 回)", fake.count())
	}
}

// 拒否した該当箇所がヘッダにあっても、置き場所を変えた再送を止める。
func TestDeniedEvidenceCannotBeResentInHeader(t *testing.T) {
	g, _ := newTest(t, config.Guard{Mode: "deny"},
		`{"action":"deny","reason":"だめ","evidence":"me@example.com"}`, nil)
	req := Request{Provider: "p", Method: "POST", Host: "h", Path: "/p",
		Headers: http.Header{"User-Agent": {"secret me@example.com"}}}
	if err := g.Check(context.Background(), req); err == nil {
		t.Fatal("deny モードで止まらなかった")
	}
	req.Headers = http.Header{"X-Other": {"me@example.com"}}
	if err := g.Check(context.Background(), req); err == nil {
		t.Fatal("以前に拒否したヘッダの内容を再送できた")
	}
}

// 拒否の記憶は期限が切れたら忘れる (同じ内容でもまた点検に回る)。
func TestDeniedEvidenceExpires(t *testing.T) {
	g, fake := newTest(t, config.Guard{Mode: "ask"},
		`{"action":"deny","reason":"だめ","evidence":"secret-1"}`, nil)
	g.SetReviewer(func(context.Context, Request, string) error { return errors.New("拒否") })
	req := Request{Body: []byte("secret-1")}
	if err := g.Check(context.Background(), req); err == nil {
		t.Fatal("止まらなかった")
	}
	fake.mu.Lock()
	fake.reply = `{"action":"allow","reason":"ok"}`
	fake.mu.Unlock()
	g.mu.Lock()
	for i := range g.denied {
		g.denied[i].at = time.Now().Add(-2 * cacheTTL)
	}
	// 塊の判定と人間の判断も同じ期限で忘れるので、まとめて古くする。
	for k, e := range g.cache {
		e.at = time.Now().Add(-2 * cacheTTL)
		g.cache[k] = e
	}
	g.mu.Unlock()
	if err := g.Check(context.Background(), req); err != nil {
		t.Fatalf("期限切れの拒否を覚え過ぎている: %v", err)
	}
}

// 人間が通した該当箇所は拒否の記憶に入らない (通したものを後から拒否しない)。
func TestAllowedEvidenceNotRememberedDenied(t *testing.T) {
	g, _ := newTest(t, config.Guard{Mode: "ask"},
		`{"action":"deny","reason":"メール","evidence":"me@example.com"}`, nil)
	g.SetReviewer(func(context.Context, Request, string) error { return nil }) // 通す
	req := Request{Body: []byte("me@example.com")}
	if err := g.Check(context.Background(), req); err != nil {
		t.Fatalf("通したのに止めた: %v", err)
	}
	g.mu.Lock()
	n := len(g.denied)
	g.mu.Unlock()
	if n != 0 {
		t.Fatalf("通した該当箇所が拒否の記憶に入った: %d 件", n)
	}
}
