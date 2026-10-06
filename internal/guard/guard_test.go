package guard

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/nananek/quagent/internal/config"
)

type fakeCompleter struct {
	mu     sync.Mutex
	calls  int
	system string
	user   string
	users  []string
	reply  string
	err    error
	// replyFn があれば reply/err より優先する。塊ごとに違う判定を返したいテスト用。
	replyFn func(system, user string) (string, error)
}

func (f *fakeCompleter) Complete(_ context.Context, system, user string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.system, f.user = system, user
	f.users = append(f.users, user)
	if f.replyFn != nil {
		return f.replyFn(system, user)
	}
	if f.err != nil {
		return "", f.err
	}
	return f.reply, nil
}

func (f *fakeCompleter) allUsers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.users...)
}

func (f *fakeCompleter) firstUser() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.users) == 0 {
		return ""
	}
	return f.users[0]
}

func (f *fakeCompleter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeCompleter) prompt() (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.system, f.user
}

// newTest は既定設定に c を当てた点検器と、その偽モデルを返す。
func newTest(t *testing.T, c config.Guard, reply string, modelErr error) (*Guard, *fakeCompleter) {
	t.Helper()
	s, err := resolve(c)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeCompleter{reply: reply, err: modelErr}
	return newWithCompleter(s, fake, nil), fake
}

func TestResolveDefaults(t *testing.T) {
	s, err := resolve(config.Guard{})
	if err != nil {
		t.Fatal(err)
	}
	if s.backend != "openai" || s.endpoint != defaultOpenAIEndpoint || s.model != defaultModel {
		t.Fatalf("既定が違う: %+v", s)
	}
	if s.mode != modeAsk || s.onError != "ask" || s.concurrency != 1 || s.maxBytes != defaultMaxBytes {
		t.Fatalf("既定が違う: %+v", s)
	}
	if s.maxChunks != defaultMaxChunks {
		t.Fatalf("max_chunks の既定が違う: %d", s.maxChunks)
	}
	if s.numCtx != defaultNumCtx {
		t.Fatalf("num_ctx の既定が違う: %d", s.numCtx)
	}
	if s, err := resolve(config.Guard{Backend: "ollama"}); err != nil || s.endpoint != defaultOllamaEndpoint {
		t.Errorf("ollama の既定 endpoint が違う: %+v, %v", s, err)
	}
	if _, err := resolve(config.Guard{Backend: "nope"}); err == nil {
		t.Error("不明な backend を受け付けた")
	}
	if _, err := resolve(config.Guard{Mode: "nope"}); err == nil {
		t.Error("不明な mode を受け付けた")
	}
	if _, err := resolve(config.Guard{OnError: "nope"}); err == nil {
		t.Error("不明な on_error を受け付けた")
	}
	if s, _ := resolve(config.Guard{MaxBytes: 1 << 20, NumCtx: MaxNumCtx}); s.maxBytes != MaxChunkBytes {
		t.Errorf("max_bytes が上限で切られていない: %d", s.maxBytes)
	}
	if s, _ := resolve(config.Guard{MaxChunks: 1 << 20}); s.maxChunks != MaxChunks {
		t.Errorf("max_chunks が上限で切られていない: %d", s.maxChunks)
	}
}

// 文脈長に収まらない max_bytes は切り下げる (LLM 側で黙って切られて点検漏れになるのを
// 防ぐ)。文脈が広ければ上限 (MaxChunkBytes) まで使える。
func TestResolveClampsMaxBytesToContext(t *testing.T) {
	s, err := resolve(config.Guard{NumCtx: MinNumCtx, MaxBytes: MaxChunkBytes})
	if err != nil {
		t.Fatal(err)
	}
	if want := maxBytesForCtx(MinNumCtx); s.maxBytes != want {
		t.Fatalf("max_bytes = %d, want %d", s.maxBytes, want)
	}
	s, err = resolve(config.Guard{NumCtx: MaxNumCtx, MaxBytes: MaxChunkBytes})
	if err != nil {
		t.Fatal(err)
	}
	if s.maxBytes != MaxChunkBytes {
		t.Fatalf("max_bytes = %d, want %d", s.maxBytes, MaxChunkBytes)
	}
}

func TestParseVerdict(t *testing.T) {
	for _, c := range []struct {
		in   string
		want Action
		ok   bool
	}{
		{`{"action":"allow","reason":"ok"}`, Allow, true},
		{`{"action":"DENY","reason":"鍵","categories":["secret"]}`, Deny, true},
		{"```json\n{\"action\":\"allow\",\"reason\":\"x\"}\n```", Allow, true},
		{"前置き {\"action\":\"deny\",\"reason\":\"y\"} 後書き", Deny, true},
		{`{"reason":"no action"}`, "", false},
		{`{"action":"maybe","reason":"?"}`, "", false},
		{`not json`, "", false},
	} {
		v, err := parseVerdict(c.in)
		if c.ok != (err == nil) {
			t.Errorf("parseVerdict(%q) err=%v", c.in, err)
			continue
		}
		if c.ok && v.Action != c.want {
			t.Errorf("parseVerdict(%q) action=%q want %q", c.in, v.Action, c.want)
		}
	}
}

func TestParseVerdictEvidence(t *testing.T) {
	v, err := parseVerdict(`{"action":"deny","reason":"漏れる","evidence":"me@example.com\n","categories":["pii"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if v.Evidence != "me@example.com" {
		t.Errorf("evidence = %q, want %q", v.Evidence, "me@example.com")
	}
	// allow のときは空のままでよい
	if v, err := parseVerdict(`{"action":"allow","reason":"ok"}`); err != nil || v.Evidence != "" {
		t.Errorf("evidence = %q, err = %v", v.Evidence, err)
	}
	// 制御文字は落とし、長さを抑える
	long := `{"action":"deny","reason":"x","evidence":"a` + strings.Repeat("b", 2000) + `"}`
	v, err = parseVerdict(long)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(v.Evidence, '\n') || len(v.Evidence) > 1000 {
		t.Errorf("evidence が整えられていない: %d バイト", len(v.Evidence))
	}
}

func TestFirstJSONObjectIgnoresBracesInStrings(t *testing.T) {
	got, ok := firstJSONObject(`x {"a":"}","b":{"c":1}} y`)
	if !ok || got != `{"a":"}","b":{"c":1}}` {
		t.Fatalf("firstJSONObject = %q, %v", got, ok)
	}
}

func TestInspectSendsHeadersAndBody(t *testing.T) {
	g, fake := newTest(t, config.Guard{}, `{"action":"allow","reason":"ok"}`, nil)
	req := Request{
		Provider: "p", Method: "POST", Host: "api.example.com", Path: "/v1/chat",
		Headers: http.Header{"User-Agent": {"curl/8 me@example.com"}},
		Body:    []byte("hello prompt"),
	}
	v, err := g.Inspect(context.Background(), req)
	if err != nil || v.Action != Allow {
		t.Fatalf("v=%+v err=%v", v, err)
	}
	_, user := fake.prompt()
	for _, want := range []string{"POST", "api.example.com/v1/chat", "User-Agent: curl/8 me@example.com", "hello prompt"} {
		if !strings.Contains(user, want) {
			t.Errorf("プロンプトに %q が無い:\n%s", want, user)
		}
	}
}

func TestInspectTruncatesBody(t *testing.T) {
	g, fake := newTest(t, config.Guard{MaxBytes: 16}, `{"action":"allow","reason":"ok"}`, nil)
	body := strings.Repeat("A", 100) + "SECRET"
	if _, err := g.Inspect(context.Background(), Request{Body: []byte(body)}); err != nil {
		t.Fatal(err)
	}
	_, user := fake.prompt()
	if strings.Contains(user, "SECRET") {
		t.Error("上限を超えた本文がモデルに渡っている")
	}
	if !strings.Contains(user, "(本文は先頭のみ)") {
		t.Error("切り詰めたことがプロンプトに出ていない")
	}
}

func TestCheckDenyAsksReviewerAndCachesDecision(t *testing.T) {
	g, fake := newTest(t, config.Guard{Mode: "ask"}, `{"action":"deny","reason":"メールが漏れる","evidence":"me@example.com","categories":["pii"]}`, nil)
	reviews := 0
	g.SetReviewer(func(_ context.Context, req Request, reason string) error {
		reviews++
		if !strings.Contains(reason, "メールが漏れる") || !strings.Contains(reason, "pii") {
			t.Errorf("理由が承認者に伝わっていない: %q", reason)
		}
		if req.Evidence != "me@example.com" {
			t.Errorf("該当箇所が承認者に伝わっていない: %q", req.Evidence)
		}
		if req.URL() != "h/p" {
			t.Errorf("URL が違う: %q", req.URL())
		}
		return nil // 人間が通した
	})
	req := Request{Provider: "p", Method: "POST", Host: "h", Path: "/p", Body: []byte("x")}
	if err := g.Check(context.Background(), req); err != nil {
		t.Fatalf("承認されたのに止めた: %v", err)
	}
	// 同じ内容は判断を覚えていて、モデルも人間ももう呼ばない
	if err := g.Check(context.Background(), req); err != nil {
		t.Fatalf("承認済みなのに止めた: %v", err)
	}
	if reviews != 1 {
		t.Errorf("承認コンソールへの確認が %d 回", reviews)
	}
	if fake.count() != 1 {
		t.Errorf("モデルを %d 回呼んだ (判定は覚えるはず)", fake.count())
	}
}

func TestCheckReviewerRejects(t *testing.T) {
	g, _ := newTest(t, config.Guard{Mode: "ask"}, `{"action":"deny","reason":"だめ","evidence":"secret-1"}`, nil)
	g.SetReviewer(func(context.Context, Request, string) error { return errors.New("拒否") })
	err := g.Check(context.Background(), Request{Body: []byte("x secret-1")})
	if err == nil || !strings.Contains(err.Error(), "承認されなかった") {
		t.Fatalf("止まらなかった: %v", err)
	}
}

func TestCheckModes(t *testing.T) {
	// deny: 人間に聞かずに止める
	g, _ := newTest(t, config.Guard{Mode: "deny"}, `{"action":"deny","reason":"だめ","evidence":"secret-1"}`, nil)
	asked := false
	g.SetReviewer(func(context.Context, Request, string) error { asked = true; return nil })
	if err := g.Check(context.Background(), Request{Body: []byte("x secret-1")}); err == nil {
		t.Error("deny モードで止まらなかった")
	}
	if asked {
		t.Error("deny モードなのに承認者に聞いた")
	}
	// advisory: 通すがモデルは呼ぶ
	g, fake := newTest(t, config.Guard{Mode: "advisory"}, `{"action":"deny","reason":"だめ","evidence":"secret-1"}`, nil)
	if err := g.Check(context.Background(), Request{Body: []byte("x secret-1")}); err != nil {
		t.Errorf("advisory で止めた: %v", err)
	}
	if fake.count() != 1 {
		t.Error("advisory でモデルを呼んでいない")
	}
}

// 具体的な該当箇所 (evidence) を指せない deny は漠然とした疑いにすぎないので通す。
// 承認者にも出さない (素の VM では秘密はまれなので、止めるのは引用できたときだけ)。
func TestCheckIgnoresDenyWithoutEvidence(t *testing.T) {
	t.Run("ask", func(t *testing.T) {
		g, _ := newTest(t, config.Guard{Mode: "ask"}, `{"action":"deny","reason":"なにか怪しい"}`, nil)
		g.SetReviewer(func(context.Context, Request, string) error {
			t.Error("具体的な該当箇所が無いのに承認者に聞いた")
			return nil
		})
		if err := g.Check(context.Background(), Request{Body: []byte("hello")}); err != nil {
			t.Fatalf("具体的な指摘が無いのに止めた: %v", err)
		}
	})
	t.Run("deny", func(t *testing.T) {
		g, _ := newTest(t, config.Guard{Mode: "deny"}, `{"action":"deny","reason":"なにか怪しい"}`, nil)
		if err := g.Check(context.Background(), Request{Body: []byte("hello")}); err != nil {
			t.Fatalf("evidence の無い deny で止めた: %v", err)
		}
	})
}

func TestCheckOnError(t *testing.T) {
	t.Run("ask", func(t *testing.T) {
		g, _ := newTest(t, config.Guard{OnError: "ask"}, "", errors.New("接続できない"))
		reviews := 0
		g.SetReviewer(func(_ context.Context, _ Request, reason string) error {
			reviews++
			if !strings.Contains(reason, "点検できなかった") {
				t.Errorf("理由が違う: %q", reason)
			}
			return nil
		})
		if err := g.Check(context.Background(), Request{Body: []byte("x")}); err != nil {
			t.Fatalf("承認されたのに止めた: %v", err)
		}
		if reviews != 1 {
			t.Errorf("確認 %d 回", reviews)
		}
	})
	t.Run("deny", func(t *testing.T) {
		g, _ := newTest(t, config.Guard{OnError: "deny"}, "", errors.New("接続できない"))
		if err := g.Check(context.Background(), Request{Body: []byte("x")}); err == nil {
			t.Error("点検できないのに止まらなかった")
		}
	})
	t.Run("allow", func(t *testing.T) {
		g, _ := newTest(t, config.Guard{OnError: "allow"}, "", errors.New("接続できない"))
		if err := g.Check(context.Background(), Request{Body: []byte("x")}); err != nil {
			t.Errorf("allow なのに止めた: %v", err)
		}
	})
}

func TestCheckAllowDoesNotAsk(t *testing.T) {
	g, _ := newTest(t, config.Guard{Mode: "ask"}, `{"action":"allow","reason":"ok"}`, nil)
	g.SetReviewer(func(context.Context, Request, string) error {
		t.Error("allow なのに承認者に聞いた")
		return nil
	})
	if err := g.Check(context.Background(), Request{Body: []byte("x")}); err != nil {
		t.Fatalf("allow なのに止めた: %v", err)
	}
}

func TestAllowRateLimit(t *testing.T) {
	g, _ := newTest(t, config.Guard{}, "", nil)
	for i := 0; i < asksPerMinute; i++ {
		if !g.allowAsk() {
			t.Fatalf("%d 回目で制限に引っかかった", i+1)
		}
	}
	if g.allowAsk() {
		t.Error("上限を超えても確認を許した")
	}
}

func TestPeekBodyRestoresRest(t *testing.T) {
	full := strings.Repeat("a", 100) + "TAIL"
	r, _ := http.NewRequest(http.MethodPost, "http://x/", strings.NewReader(full))
	head, truncated, err := PeekBody(r, 100)
	if err != nil || !truncated || string(head) != strings.Repeat("a", 100) {
		t.Fatalf("head=%d trunc=%v err=%v", len(head), truncated, err)
	}
	rest, _ := io.ReadAll(r.Body)
	if string(rest) != full {
		t.Fatalf("本文全体が保たれていない: %q", rest)
	}
	// 本文が短いときは truncated=false
	r2, _ := http.NewRequest(http.MethodPost, "http://x/", strings.NewReader("short"))
	head, truncated, err = PeekBody(r2, 100)
	if err != nil || truncated || string(head) != "short" {
		t.Fatalf("head=%q trunc=%v err=%v", head, truncated, err)
	}
}

func TestClipBytesKeepsUTF8(t *testing.T) {
	s := "日本語"
	if got := clipBytes(s, 4); got != "日" {
		t.Errorf("clipBytes = %q", got)
	}
	if got := clipBytes(s, 100); got != s {
		t.Errorf("clipBytes が短い文字列を変えた: %q", got)
	}
}

func TestCleanReasonStripsControl(t *testing.T) {
	if got := cleanReason("a\nb\x00c"); got != "a bc" {
		t.Errorf("cleanReason = %q", got)
	}
	long := strings.Repeat("x", 1000)
	if got := cleanReason(long); len(got) > 300 {
		t.Errorf("cleanReason が長すぎる: %d", len(got))
	}
}

// 判断の鍵は LLM に見せる先頭 max_bytes だけでなく本文全体を見る (後ろが違う
// リクエストに人間の判断を流用しない)。
func TestCacheKeyCoversFullBody(t *testing.T) {
	g, _ := newTest(t, config.Guard{MaxBytes: 4}, `{"action":"allow","reason":"ok"}`, nil)
	k1 := g.cacheKey(Request{Body: []byte("AAAAone")})
	k2 := g.cacheKey(Request{Body: []byte("AAAAtwo")})
	if k1 == k2 {
		t.Error("先頭だけ同じ本文の鍵が同じになった")
	}
}

// 判断の鍵は各ヘッダの先頭 1024 バイトだけではない。
func TestCacheKeyCoversFullHeader(t *testing.T) {
	g, _ := newTest(t, config.Guard{}, `{"action":"allow","reason":"ok"}`, nil)
	long := strings.Repeat("x", MaxHeaderBytes)
	k1 := g.cacheKey(Request{Headers: http.Header{"User-Agent": {long + "one"}}})
	k2 := g.cacheKey(Request{Headers: http.Header{"User-Agent": {long + "two"}}})
	if k1 == k2 {
		t.Error("先頭だけ同じヘッダの鍵が同じになった")
	}
}

// 塊の境目にまたがる短い秘密も、どれかの塊に丸ごと入って点検される。
func TestSplitChunksOverlapKeepsSecret(t *testing.T) {
	g, fake := newTest(t, config.Guard{MaxBytes: 16}, `{"action":"allow","reason":"ok"}`, nil)
	// 秘密が 16 バイトの境目をまたぐ位置に置く (重なりが無いと分断される)
	body := strings.Repeat("A", 14) + "SECRET"
	if _, err := g.Inspect(context.Background(), Request{Body: []byte(body)}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(fake.allUsers(), "\n")
	if !strings.Contains(joined, "SECRET") {
		t.Error("境目にまたがる秘密がどの塊にも丸ごと入っていない")
	}
}

// 分割数と、塊ごとに点検していることを確かめる。
func TestInspectSplitsBodyIntoChunks(t *testing.T) {
	g, fake := newTest(t, config.Guard{MaxBytes: 8, MaxChunks: 4}, `{"action":"allow","reason":"ok"}`, nil)
	// 塊ごとに内容が違うようにする (同じ内容の塊は 1 回にまとめられるため)
	body := make([]byte, 100)
	for i := range body {
		body[i] = byte('a' + i%26)
	}
	if _, err := g.Inspect(context.Background(), Request{Body: body}); err != nil {
		t.Fatal(err)
	}
	if fake.count() != 4 {
		t.Fatalf("点検回数 = %d, want 4", fake.count())
	}
	if !strings.Contains(fake.firstUser(), "part: 1/4") {
		t.Errorf("塊の番号がプロンプトに出ていない")
	}
}

// 一度許可した該当箇所 (LLM が機密だと思った部分) は、本文が伸びても二度は訊かない。
func TestCheckReusesDecisionByEvidence(t *testing.T) {
	g, fake := newTest(t, config.Guard{Mode: "ask"},
		`{"action":"deny","reason":"メールが漏れる","evidence":"me@example.com","categories":["pii"]}`, nil)
	reviews := 0
	g.SetReviewer(func(_ context.Context, _ Request, _ string) error { reviews++; return nil })
	base := "連絡先は me@example.com です"
	if err := g.Check(context.Background(), Request{Provider: "p", Method: "POST", Host: "h", Path: "/p", Body: []byte(base)}); err != nil {
		t.Fatalf("承認されたのに止めた: %v", err)
	}
	// 会話履歴が伸びた本文。同じ該当箇所を含む。
	grown := base + " その後のやり取り。"
	if err := g.Check(context.Background(), Request{Provider: "p", Method: "POST", Host: "h", Path: "/p", Body: []byte(grown)}); err != nil {
		t.Fatalf("承認済みの該当箇所なのに止めた: %v", err)
	}
	if reviews != 1 {
		t.Errorf("同じ該当箇所を %d 回訊いた (一度でよい)", reviews)
	}
	if fake.count() < 2 {
		t.Errorf("本文が違うので点検はやり直すはず: %d 回", fake.count())
	}
}

// 一度止めた該当箇所は、本文が変わっても訊き直さずに止める。
func TestCheckReusesDenialByEvidence(t *testing.T) {
	g, _ := newTest(t, config.Guard{Mode: "ask"},
		`{"action":"deny","reason":"だめ","evidence":"secret-token-123"}`, nil)
	reviews := 0
	g.SetReviewer(func(context.Context, Request, string) error { reviews++; return errors.New("拒否") })
	req := Request{Provider: "p", Method: "POST", Host: "h", Path: "/p", Body: []byte("x secret-token-123")}
	if err := g.Check(context.Background(), req); err == nil {
		t.Fatal("拒否したのに通した")
	}
	req.Body = []byte("x secret-token-123 追記")
	if err := g.Check(context.Background(), req); err == nil {
		t.Fatal("止めた該当箇所なのに通した")
	}
	if reviews != 1 {
		t.Errorf("一度止めた該当箇所を %d 回訊いた", reviews)
	}
}

// 承認コンソールには本文全体ではなく、該当箇所の周辺だけを見せる。
func TestCheckShowsEvidenceWindow(t *testing.T) {
	evidence := "me@example.com"
	body := []byte(strings.Repeat("A", 5000) + evidence + strings.Repeat("B", 5000))
	g, _ := newTest(t, config.Guard{},
		`{"action":"deny","reason":"メール","evidence":"me@example.com"}`, nil)
	var shown []byte
	g.SetReviewer(func(_ context.Context, req Request, _ string) error {
		shown = append([]byte(nil), req.Body...)
		return nil
	})
	if err := g.Check(context.Background(), Request{Provider: "p", Method: "POST", Host: "h", Path: "/p", Body: body}); err != nil {
		t.Fatalf("承認されたのに止めた: %v", err)
	}
	if !strings.Contains(string(shown), evidence) {
		t.Error("該当箇所が承認者に見えていない")
	}
	if len(shown) >= len(body) {
		t.Errorf("本文全体が承認者に渡っている: %d >= %d", len(shown), len(body))
	}
	if !bytes.Contains(shown, []byte("AAAAA")) || !bytes.Contains(shown, []byte("BBBBB")) {
		t.Error("該当箇所の前後の文脈が無い")
	}
}

// 会話履歴のように本文が伸びても、一度点検した塊はローカル LLM に送り直さない。
func TestInspectReusesInspectedChunks(t *testing.T) {
	g, fake := newTest(t, config.Guard{MaxBytes: 8, MaxChunks: 64}, `{"action":"allow","reason":"ok"}`, nil)
	body1 := []byte("abcdefghijklmnopqrst") // 20 バイト
	if _, err := g.Inspect(context.Background(), Request{Body: body1}); err != nil {
		t.Fatal(err)
	}
	first := fake.count()
	if first != 4 {
		t.Fatalf("初回の点検回数 = %d, want 4", first)
	}
	body2 := append(append([]byte(nil), body1...), "uvwx"...)
	if _, err := g.Inspect(context.Background(), Request{Body: body2}); err != nil {
		t.Fatal(err)
	}
	if got := fake.count() - first; got != 1 {
		t.Errorf("伸びた本文で %d 塊を点検した (新しい 1 塊だけのはず)", got)
	}
}

// Content-Type が JSON の本文は、jq のように字下げしてモデルに見せる。
func TestInspectPrettyPrintsJSONBody(t *testing.T) {
	g, fake := newTest(t, config.Guard{}, `{"action":"allow","reason":"ok"}`, nil)
	req := Request{
		Provider: "p", Method: "POST", Host: "api.example.com", Path: "/v1/chat",
		Headers: http.Header{"Content-Type": {"application/json"}},
		Body:    []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`),
	}
	if _, err := g.Inspect(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	_, user := fake.prompt()
	if !strings.Contains(user, "\n  \"model\": \"x\"") || !strings.Contains(user, "\n      \"role\": \"user\"") {
		t.Errorf("JSON が字下げされていない:\n%s", user)
	}
	if strings.Contains(user, `{"model":"x"`) {
		t.Errorf("minify された本文がそのまま渡っている:\n%s", user)
	}
}

// JSON でない本文 (先頭が { でも JSON として読めない) はそのまま渡す。
func TestInspectLeavesNonJSONBody(t *testing.T) {
	g, fake := newTest(t, config.Guard{}, `{"action":"allow","reason":"ok"}`, nil)
	body := []byte("not json { but braces")
	if _, err := g.Inspect(context.Background(), Request{Body: body}); err != nil {
		t.Fatal(err)
	}
	_, user := fake.prompt()
	if !strings.Contains(user, string(body)) {
		t.Errorf("本文が変わった:\n%s", user)
	}
}

// 字下げした形で指摘された該当箇所 (コロンの後の空白を含む) でも、minify された
// 本文の再送を止められる (照合は元の本文と字下げした本文の両方で行う)。
func TestDeniedEvidenceMatchesPresentedJSON(t *testing.T) {
	g, _ := newTest(t, config.Guard{Mode: "deny"},
		`{"action":"deny","reason":"漏れる","evidence":"\"email\": \"me@example.com\""}`, nil)
	req := Request{Provider: "p", Method: "POST", Host: "h", Path: "/p",
		Headers: http.Header{"Content-Type": {"application/json"}},
		Body:    []byte(`{"email":"me@example.com"}`)}
	if err := g.Check(context.Background(), req); err == nil {
		t.Fatal("deny モードで止まらなかった")
	}
	if err := g.Check(context.Background(), req); err == nil {
		t.Fatal("字下げ形で指摘された該当箇所の再送を止められていない")
	}
}

// 塊のどれかが deny でも、後ろの塊まで点検する (途中で打ち切ると、後ろの秘密が
// モデルにも人間にも見えないまま本文全体が転送されてしまう)。
func TestInspectInspectsAllChunksAfterDeny(t *testing.T) {
	g, fake := newTest(t, config.Guard{MaxBytes: 8, MaxChunks: 64},
		`{"action":"deny","reason":"だめ","evidence":"zz"}`, nil)
	body := make([]byte, 40)
	for i := range body {
		body[i] = byte('a' + i%26)
	}
	wantChunks, _ := splitChunks(body, 8, 64)
	v, err := g.Inspect(context.Background(), Request{Body: body})
	if err != nil {
		t.Fatal(err)
	}
	if v.Action != Deny {
		t.Fatalf("action = %q, want deny", v.Action)
	}
	if len(v.flags) != len(wantChunks) {
		t.Fatalf("flags = %d, want %d (全塊を点検するはず)", len(v.flags), len(wantChunks))
	}
	if fake.count() != len(wantChunks) {
		t.Fatalf("点検回数 = %d, want %d (deny で打ち切っている)", fake.count(), len(wantChunks))
	}
}

// deny した塊が複数あるとき、承認者には全部の該当箇所の周辺を見せる。先頭の塊だけで
// deny を起こして後ろの塊を隠し、承認させて持ち出す攻撃を防ぐ。
func TestCheckShowsAllDeniedChunks(t *testing.T) {
	g, fake := newTest(t, config.Guard{MaxBytes: 16, MaxChunks: 16}, "", nil)
	fake.replyFn = func(_, user string) (string, error) {
		switch {
		case strings.Contains(user, "SECRET"):
			return `{"action":"deny","reason":"秘密","evidence":"SECRET"}`, nil
		case strings.Contains(user, "DECOY"):
			return `{"action":"deny","reason":"デコイ","evidence":"DECOY"}`, nil
		default:
			return `{"action":"allow","reason":"ok"}`, nil
		}
	}
	var shown []byte
	g.SetReviewer(func(_ context.Context, req Request, _ string) error {
		shown = append([]byte(nil), req.Body...)
		return nil
	})
	body := []byte("DECOY" + strings.Repeat("x", 40) + "SECRET")
	if err := g.Check(context.Background(), Request{Provider: "p", Method: "POST", Host: "h", Path: "/p", Body: body}); err != nil {
		t.Fatalf("承認されたのに止めた: %v", err)
	}
	if !strings.Contains(string(shown), "DECOY") || !strings.Contains(string(shown), "SECRET") {
		t.Fatalf("deny した塊の後半が承認者に見えていない: %q", shown)
	}
}

// 一度承認した該当箇所があっても、同じ本文に新しく指摘された該当箇所があれば訊き直す
// (承認済みの該当箇所を 1 つ混ぜて、新しい指摘を素通りさせない)。
func TestCheckAsksAgainForNewEvidence(t *testing.T) {
	g, fake := newTest(t, config.Guard{MaxBytes: 16, MaxChunks: 16}, "", nil)
	fake.replyFn = func(_, user string) (string, error) {
		switch {
		case strings.Contains(user, "ALPHA"):
			return `{"action":"deny","reason":"A","evidence":"ALPHA"}`, nil
		case strings.Contains(user, "BRAVO"):
			return `{"action":"deny","reason":"B","evidence":"BRAVO"}`, nil
		default:
			return `{"action":"allow","reason":"ok"}`, nil
		}
	}
	reviews := 0
	g.SetReviewer(func(_ context.Context, _ Request, _ string) error { reviews++; return nil })

	if err := g.Check(context.Background(), Request{
		Provider: "p", Method: "POST", Host: "h", Path: "/p", Body: []byte("ALPHA"),
	}); err != nil {
		t.Fatalf("承認されたのに止めた: %v", err)
	}
	if reviews != 1 {
		t.Fatalf("承認回数 = %d, want 1", reviews)
	}
	// ALPHA (承認済み) と BRAVO (新規) を両方含む本文。BRAVO は訊き直すはず。
	body := []byte("ALPHA" + strings.Repeat("x", 40) + "BRAVO")
	if err := g.Check(context.Background(), Request{
		Provider: "p", Method: "POST", Host: "h", Path: "/p", Body: body,
	}); err != nil {
		t.Fatalf("承認されたのに止めた: %v", err)
	}
	if reviews != 2 {
		t.Fatalf("新しい該当箇所を訊き直していない: 承認回数 = %d, want 2", reviews)
	}
}

// 本文に実在しない evidence は人間の判断の鍵にしない。幻覚やプロンプト注入で
// 返させた文字列を混ぜて、別の本文の承認を流用させない。
func TestCheckDoesNotKeyDecisionOnFabricatedEvidence(t *testing.T) {
	g, fake := newTest(t, config.Guard{}, "", nil)
	fake.replyFn = func(_, _ string) (string, error) {
		return `{"action":"deny","reason":"疑惑","evidence":"FABRICATED-NOT-IN-BODY"}`, nil
	}
	reviews := 0
	g.SetReviewer(func(_ context.Context, _ Request, _ string) error { reviews++; return nil })

	if err := g.Check(context.Background(), Request{
		Provider: "p", Method: "POST", Host: "h", Path: "/p", Body: []byte("first body"),
	}); err != nil {
		t.Fatalf("承認されたのに止めた: %v", err)
	}
	if err := g.Check(context.Background(), Request{
		Provider: "p", Method: "POST", Host: "h", Path: "/p", Body: []byte("second body with SECRET"),
	}); err != nil {
		t.Fatalf("承認されたのに止めた: %v", err)
	}
	if reviews != 2 {
		t.Fatalf("実在しない evidence で承認が流用された: 承認回数 = %d, want 2", reviews)
	}
}

// structured output のスキーマに evidence が無いと、strict なサーバーではモデルが
// 引用を返せず、deny が必ず allow に落ちてしまう。
func TestVerdictSchemaIncludesEvidence(t *testing.T) {
	props, ok := verdictSchema["properties"].(map[string]any)
	if !ok {
		t.Fatal("verdictSchema に properties が無い")
	}
	if _, ok := props["evidence"]; !ok {
		t.Fatal("verdictSchema に evidence が無い")
	}
}
