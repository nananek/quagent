// Package guard はプロキシを通る HTTP リクエストの中身をローカル LLM で点検する。
//
// ネットワークの許可制 (どのドメインへ出てよいか) は、許可したドメインへ秘密や
// 個人情報を持ち出す要求までは見抜けない。たとえば User-Agent にメールアドレスを
// 紛れ込ませても、行き先が許可済みなら通ってしまう。ここでは host が平文で見られる
// リクエスト (LLM 認証プロキシ) について、行き先・ヘッダ・本文の先頭をローカル LLM に
// 渡し、「外へ持ち出そうとしていないか」を判定させる。
//
// 判定は追加の一枚であって、許可制や承認コンソールの代わりではない。ローカル LLM は
// 間違えるので、疑わしいと判定したときは (既定で) その理由と中身を承認コンソールに
// 出し、人間が通すか止めるかを決める。点検できなかったときの扱いも OnError で選べる。
package guard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nananek/quagent/internal/config"
)

// Action はローカル LLM の判定。
type Action string

const (
	Allow Action = "allow"
	Deny  Action = "deny"
)

// Verdict は 1 つのリクエストに対する点検結果。
type Verdict struct {
	Action     Action
	Reason     string
	Categories []string
}

// Request は点検・承認に渡すリクエストの写し (本文は先頭の抜粋だけ)。
type Request struct {
	Provider string
	Method   string
	Host     string
	Path     string
	Query    string
	Headers  http.Header
	Body     []byte
	// BodyTruncated は本文が長く、抜粋より後ろを LLM に見せていないとき true。
	BodyTruncated bool
}

// URL は表示用の行き先を返す。
func (r Request) URL() string {
	if r.Query == "" {
		return r.Host + r.Path
	}
	return r.Host + r.Path + "?" + r.Query
}

// HeaderLines はヘッダを "名前: 値" の行にして返す (表示用。1 つの値は MaxHeaderBytes まで)。
func (r Request) HeaderLines() []string {
	names := make([]string, 0, len(r.Headers))
	for name := range r.Headers {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []string
	for _, name := range names {
		for _, v := range r.Headers[name] {
			out = append(out, name+": "+clipBytes(v, MaxHeaderBytes))
		}
	}
	return out
}

// Completer は点検に使うローカル LLM。system / user のプロンプトから応答本文を返す。
type Completer interface {
	Complete(ctx context.Context, system, user string) (string, error)
}

// Reviewer は疑わしいリクエスト (または点検できなかったリクエスト) を人間に諮る。
// 通すなら nil、止めるなら理由を返す。
type Reviewer func(ctx context.Context, req Request, reason string) error

// 点検の既定値。
const (
	// llama.cpp の llama-server は既定で 127.0.0.1:8080 に立つ。
	defaultOpenAIEndpoint = "http://127.0.0.1:8080"
	defaultOllamaEndpoint = "http://127.0.0.1:11434"
	defaultModel          = "qwen2.5-3b-instruct"
	defaultTimeout        = 30 * time.Second
	defaultMaxBytes       = 8 << 10
	// MaxInspect は 1 リクエストで LLM に見せる本文の上限 (設定がこれより大きくても切る)。
	MaxInspect = 32 << 10
	// MaxHeaderBytes はヘッダ 1 つの値を LLM と承認コンソールに見せる上限。
	MaxHeaderBytes = 1024
	// NumCtx はローカル LLM に渡す文脈長。3B 級なら 6GB の VRAM に収まる。
	NumCtx = 8 << 10

	// 1 分に承認コンソールへ出せる内容ガードの確認の件数。溢れた分は拒否する。
	asksPerMinute = 12

	cacheMax = 512
	cacheTTL = 10 * time.Minute
)

type mode int

const (
	modeAsk mode = iota
	modeDeny
	modeAdvisory
)

type settings struct {
	backend     string
	endpoint    string
	model       string
	timeout     time.Duration
	maxBytes    int
	mode        mode
	onError     string // "deny" / "allow" / "ask"
	concurrency int
}

// Guard は設定とローカル LLM をまとめ、リクエストを点検する。
type Guard struct {
	s        settings
	complete Completer
	review   Reviewer
	log      *log.Logger

	sem    chan struct{} // ローカル LLM を同時に叩く数
	askSem chan struct{} // 承認コンソールに同時に出す確認の数

	mu        sync.Mutex
	cache     map[string]entry
	order     []string
	askWindow time.Time
	askShown  int
}

type entry struct {
	v      Verdict
	hasV   bool
	allow  bool
	hasDec bool
	reason string
	at     time.Time
}

// New は設定から点検器を作る。Enabled が false でも作れる (CLI から試すため)。
func New(c config.Guard, logger *log.Logger) (*Guard, error) {
	s, err := resolve(c)
	if err != nil {
		return nil, err
	}
	var comp Completer
	switch s.backend {
	case "ollama":
		comp = &Ollama{Endpoint: s.endpoint, Model: s.model}
	case "openai":
		comp = &OpenAI{Endpoint: s.endpoint, Model: s.model}
	default:
		return nil, fmt.Errorf("guard.backend は openai / ollama のどれか: %q", s.backend)
	}
	return newWithCompleter(s, comp, logger), nil
}

func newWithCompleter(s settings, comp Completer, logger *log.Logger) *Guard {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Guard{
		s: s, complete: comp, log: logger,
		sem:    make(chan struct{}, s.concurrency),
		askSem: make(chan struct{}, 1),
		cache:  map[string]entry{},
	}
}

// String は設定の要約を返す (起動ログ用)。
func (g *Guard) String() string {
	mode := "ask"
	switch g.s.mode {
	case modeDeny:
		mode = "deny"
	case modeAdvisory:
		mode = "advisory"
	}
	return fmt.Sprintf("%s の %s (mode=%s, on_error=%s, max_bytes=%d, concurrency=%d)",
		g.s.backend, g.s.model, mode, g.s.onError, g.s.maxBytes, g.s.concurrency)
}

// SetReviewer は疑わしいリクエストを諮る先 (承認コンソール) を設定する。
func (g *Guard) SetReviewer(fn Reviewer) { g.review = fn }

// resolve は設定に既定値を当て、値の妥当性を確かめる。
func resolve(c config.Guard) (settings, error) {
	s := settings{
		backend:     strings.ToLower(strings.TrimSpace(c.Backend)),
		endpoint:    strings.TrimRight(strings.TrimSpace(c.Endpoint), "/"),
		model:       strings.TrimSpace(c.Model),
		timeout:     time.Duration(c.TimeoutSeconds) * time.Second,
		maxBytes:    c.MaxBytes,
		concurrency: c.Concurrency,
	}
	switch s.backend {
	case "":
		s.backend = "openai"
	case "ollama", "openai":
	default:
		return settings{}, fmt.Errorf("guard.backend は openai / ollama のどれか: %q", c.Backend)
	}
	if s.endpoint == "" {
		if s.backend == "ollama" {
			s.endpoint = defaultOllamaEndpoint
		} else {
			s.endpoint = defaultOpenAIEndpoint
		}
	}
	if u, err := url.Parse(s.endpoint); err != nil || u.Scheme == "" || u.Host == "" {
		return settings{}, fmt.Errorf("guard.endpoint が URL として不正: %q", c.Endpoint)
	}
	if s.model == "" {
		s.model = defaultModel
	}
	if s.timeout <= 0 {
		s.timeout = defaultTimeout
	}
	if s.maxBytes <= 0 {
		s.maxBytes = defaultMaxBytes
	}
	if s.maxBytes > MaxInspect {
		s.maxBytes = MaxInspect
	}
	if s.concurrency <= 0 {
		s.concurrency = 1
	}
	switch strings.ToLower(strings.TrimSpace(c.Mode)) {
	case "", "ask":
		s.mode = modeAsk
	case "deny":
		s.mode = modeDeny
	case "advisory":
		s.mode = modeAdvisory
	default:
		return settings{}, fmt.Errorf("guard.mode は ask / deny / advisory のどれか: %q", c.Mode)
	}
	switch strings.ToLower(strings.TrimSpace(c.OnError)) {
	case "", "ask":
		s.onError = "ask"
	case "deny":
		s.onError = "deny"
	case "allow":
		s.onError = "allow"
	default:
		return settings{}, fmt.Errorf("guard.on_error は ask / deny / allow のどれか: %q", c.OnError)
	}
	return s, nil
}

// Check はリクエストを点検し、通すなら nil、止めるなら理由を返す。人間の判断 (Reviewer)、
// Mode、OnError を当てはめた最終判断はここで行う。
func (g *Guard) Check(ctx context.Context, req Request) error {
	key := g.cacheKey(req)
	if allow, reason, ok := g.decision(key); ok {
		if allow {
			return nil
		}
		return fmt.Errorf("%s", reason)
	}
	v, err := g.inspect(ctx, key, req)
	if err != nil {
		return g.onError(ctx, req, err)
	}
	if v.Action != Deny {
		return nil
	}
	return g.onSuspect(ctx, key, req, verdictReason(v))
}

// Inspect は点検の判定だけを返す (Reviewer / Mode / OnError は適用しない)。CLI 用。
func (g *Guard) Inspect(ctx context.Context, req Request) (Verdict, error) {
	return g.inspect(ctx, g.cacheKey(req), req)
}

func (g *Guard) inspect(ctx context.Context, key string, req Request) (Verdict, error) {
	if v, ok := g.cachedVerdict(key); ok {
		return v, nil
	}
	if len(req.Body) > g.s.maxBytes {
		req.BodyTruncated = true
	}
	req.Body = truncateBytes(req.Body, g.s.maxBytes)
	ctx, cancel := context.WithTimeout(ctx, g.s.timeout)
	defer cancel()
	select {
	case g.sem <- struct{}{}:
		defer func() { <-g.sem }()
	case <-ctx.Done():
		return Verdict{}, ctx.Err()
	}
	system, user := buildPrompt(req)
	out, err := g.complete.Complete(ctx, system, user)
	if err != nil {
		return Verdict{}, err
	}
	v, err := parseVerdict(out)
	if err != nil {
		return Verdict{}, err
	}
	g.rememberVerdict(key, v)
	return v, nil
}

// onSuspect は「疑わしい」と判定されたリクエストの扱いを決める。
func (g *Guard) onSuspect(ctx context.Context, key string, req Request, reason string) error {
	switch g.s.mode {
	case modeAdvisory:
		g.logf("疑わしいが通した (advisory): %s", reason)
		return nil
	case modeDeny:
		return fmt.Errorf("%s", reason)
	}
	if g.review == nil {
		g.logf("承認者に諮れないので止めた: %s", reason)
		return fmt.Errorf("%s (承認者に諮れないので止めた)", reason)
	}
	if !g.allowAsk() {
		g.logf("内容ガードの確認が多すぎるので止めた: %s", reason)
		return fmt.Errorf("%s (確認が多すぎるので止めた)", reason)
	}
	if err := g.reviewWithSem(ctx, req, reason); err != nil {
		g.rememberDecision(key, false, reason)
		return fmt.Errorf("承認されなかったので止めた: %s", reason)
	}
	g.logf("承認者が通した: %s", reason)
	g.rememberDecision(key, true, reason)
	return nil
}

// onError は点検できなかったときの扱いを決める。
func (g *Guard) onError(ctx context.Context, req Request, cause error) error {
	switch g.s.onError {
	case "allow":
		g.logf("点検できないが通した: %v", cause)
		return nil
	case "ask":
		if g.review == nil {
			return fmt.Errorf("内容を点検できず、諮る先も無いので止めた: %w", cause)
		}
		reason := "内容を点検できなかった: " + cleanReason(cause.Error())
		if !g.allowAsk() {
			return fmt.Errorf("内容を点検できない (確認も多すぎる) ので止めた: %w", cause)
		}
		if err := g.reviewWithSem(ctx, req, reason); err != nil {
			return fmt.Errorf("内容を点検できず、承認もされなかったので止めた: %w", cause)
		}
		g.logf("点検できなかったが承認者が通した: %v", cause)
		return nil
	default: // deny
		g.logf("点検できないので止めた: %v", cause)
		return fmt.Errorf("内容を点検できなかったので止めた: %w", cause)
	}
}

// reviewWithSem は承認コンソールへの確認を 1 件ずつに絞って諮る。
func (g *Guard) reviewWithSem(ctx context.Context, req Request, reason string) error {
	select {
	case g.askSem <- struct{}{}:
		defer func() { <-g.askSem }()
	case <-ctx.Done():
		return ctx.Err()
	}
	return g.review(ctx, req, reason)
}

// allowAsk は 1 分あたりの確認の件数を抑える (連打で承認コンソールを埋めない)。
func (g *Guard) allowAsk() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if now.Sub(g.askWindow) >= time.Minute {
		g.askWindow, g.askShown = now, 0
	}
	g.askShown++
	return g.askShown <= asksPerMinute
}

// Warm はモデルを VRAM に載せるため、起動直後に 1 回だけ空打ちする。
// 最初の本番リクエストがモデルの読み込み待ちで時間切れになるのを避ける。
// (sem は取らない。Ollama 側が同じモデルの要求を直列化する。)
func (g *Guard) Warm(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	system, user := buildPrompt(Request{Provider: "warmup", Method: "GET", Host: "localhost", Path: "/"})
	if _, err := g.complete.Complete(ctx, system, user); err != nil {
		g.logf("ローカル LLM を温められなかった (最初の点検が遅くなる): %v", err)
		return
	}
	g.logf("ローカル LLM (%s / %s) を読み込んだ", g.s.backend, g.s.model)
}

// logf は logger が nil のときは捨てる。
func (g *Guard) logf(format string, a ...any) {
	if g.log != nil {
		g.log.Printf(format, a...)
	}
}

// cacheKey は点検対象の同一性を表すハッシュを返す。本文は先頭 maxBytes だけ見る。
func (g *Guard) cacheKey(req Request) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s\x00", req.Provider, req.Method, req.Host, req.Path, req.Query)
	names := make([]string, 0, len(req.Headers))
	for name := range req.Headers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(h, "%s:", name)
		for _, v := range req.Headers[name] {
			fmt.Fprintf(h, "%s\x1f", clipBytes(v, MaxHeaderBytes))
		}
		fmt.Fprint(h, "\x1e")
	}
	h.Write([]byte{0})
	h.Write(truncateBytes(req.Body, g.s.maxBytes))
	return hex.EncodeToString(h.Sum(nil))
}

func (g *Guard) cachedVerdict(key string) (Verdict, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.cache[key]
	if !ok || !e.hasV || time.Since(e.at) > cacheTTL {
		return Verdict{}, false
	}
	return e.v, true
}

func (g *Guard) rememberVerdict(key string, v Verdict) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.cache[key]
	e.v, e.hasV, e.at = v, true, time.Now()
	g.store(key, e)
}

func (g *Guard) decision(key string) (allow bool, reason string, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, found := g.cache[key]
	if !found || !e.hasDec || time.Since(e.at) > cacheTTL {
		return false, "", false
	}
	return e.allow, e.reason, true
}

func (g *Guard) rememberDecision(key string, allow bool, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.cache[key]
	e.allow, e.hasDec, e.reason, e.at = allow, true, reason, time.Now()
	g.store(key, e)
}

// store は entry を入れ、上限を超えたら古いものから捨てる (g.mu を持って呼ぶ)。
func (g *Guard) store(key string, e entry) {
	if _, ok := g.cache[key]; !ok {
		g.order = append(g.order, key)
		for len(g.order) > cacheMax {
			delete(g.cache, g.order[0])
			g.order = g.order[1:]
		}
	}
	g.cache[key] = e
}

// RequestFrom は *http.Request から点検用の写しを作る (本文は呼び出し側が読んだ抜粋)。
func RequestFrom(r *http.Request, provider string, body []byte, truncated bool) Request {
	return Request{
		Provider:      provider,
		Method:        r.Method,
		Host:          r.Host,
		Path:          r.URL.Path,
		Query:         r.URL.RawQuery,
		Headers:       r.Header,
		Body:          body,
		BodyTruncated: truncated,
	}
}

// PeekBody は r.Body の先頭 n バイトを読み、残りを保ったまま r.Body を差し替える。
// n より長い本文は truncated=true を返す (LLM には抜粋しか渡さない)。
func PeekBody(r *http.Request, n int) (body []byte, truncated bool, err error) {
	if r.Body == nil || n <= 0 {
		return nil, false, nil
	}
	if n > MaxInspect {
		n = MaxInspect
	}
	buf := make([]byte, n)
	read, err := io.ReadFull(r.Body, buf)
	switch err {
	case nil:
		// ちょうど n バイト以上読めた。残りはあるかもしれない。prefix と残りをつないで
		// 元の本文 (全体) をそのまま後段に読ませる。
		rest := r.Body
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(buf), rest), rest}
		return buf, true, nil
	case io.EOF:
		r.Body = http.NoBody
		return nil, false, nil
	case io.ErrUnexpectedEOF:
		// n より短く、全部読めた。prefix が本文のすべて。
		body = buf[:read]
		r.Body = io.NopCloser(bytes.NewReader(body))
		return body, false, nil
	default:
		return nil, false, err
	}
}

// buildPrompt は行き先・ヘッダ・本文の抜粋から system / user プロンプトを組み立てる。
func buildPrompt(req Request) (system, user string) {
	var b strings.Builder
	fmt.Fprintf(&b, "destination: %s\nmethod: %s\nprovider: %s\n", req.URL(), req.Method, req.Provider)
	b.WriteString("headers:\n")
	lines := req.HeaderLines()
	if len(lines) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, line := range lines {
		fmt.Fprintf(&b, "  %s\n", line)
	}
	if len(req.Body) == 0 {
		b.WriteString("body: (empty)\n")
	} else {
		note := ""
		if req.BodyTruncated {
			note = " (本文は先頭のみ)"
		}
		fmt.Fprintf(&b, "body%s:\n%s\n", note, string(req.Body))
	}
	return systemPrompt, b.String()
}

const systemPrompt = `You are the last line of defense for a sandboxed coding agent. Every HTTP request the agent makes passes through you before it leaves the machine. Decide whether the request is trying to exfiltrate confidential data to an external server.

Confidential data includes API keys, access tokens, passwords, private keys, credentials, session cookies, and personal data such as email addresses, phone numbers, home addresses and government IDs.

The agent may hide data in the User-Agent or any other header, in the URL or query, or in the body, and may encode it (base64, hex, percent-encoding, unicode escapes) or split it up. Judge the actual content, not just keywords.

Allow normal coding-agent traffic: prompts and completions sent to an LLM API, model-list requests, fetching public documentation or package metadata, and code that merely mentions words like "password" or "secret".

Deny only when there is concrete evidence that real confidential data is being sent where it does not belong, especially when it is hidden in a header or obfuscated.

Output ONLY one JSON object, with no prose and no code fences:
{"action":"allow"|"deny","reason":"short reason in Japanese","categories":["secret"|"credentials"|"pii"|"exfiltration"|"other"]}
If you are unsure, choose "allow".`

// parseVerdict はローカル LLM の応答から判定を取り出す。前後に説明やコードフェンスが
// あっても、最初の JSON オブジェクトを拾う。
func parseVerdict(out string) (Verdict, error) {
	raw, ok := firstJSONObject(out)
	if !ok {
		return Verdict{}, fmt.Errorf("判定の JSON が無い: %q", clipBytes(out, 200))
	}
	var got struct {
		Action     string   `json:"action"`
		Reason     string   `json:"reason"`
		Categories []string `json:"categories"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		return Verdict{}, fmt.Errorf("判定の JSON を読めない: %w", err)
	}
	var v Verdict
	switch strings.ToLower(strings.TrimSpace(got.Action)) {
	case "allow":
		v.Action = Allow
	case "deny":
		v.Action = Deny
	default:
		return Verdict{}, fmt.Errorf("action が allow / deny でない: %q", got.Action)
	}
	v.Reason = got.Reason
	for _, c := range got.Categories {
		c = strings.TrimSpace(c)
		if c != "" && len(c) <= 40 {
			v.Categories = append(v.Categories, c)
		}
	}
	return v, nil
}

// verdictReason は判定を表示・HTTP 本文に出せる 1 行の理由にする。
func verdictReason(v Verdict) string {
	reason := cleanReason(v.Reason)
	if reason == "" {
		reason = "機密情報の持ち出しが疑われる内容"
	}
	if len(v.Categories) > 0 {
		reason += " [" + strings.Join(v.Categories, ", ") + "]"
	}
	return reason
}

// firstJSONObject は s の中の最初の { から対応する } までを返す。
func firstJSONObject(s string) (string, bool) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "", false
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case inStr && esc:
			esc = false
		case inStr && c == '\\':
			esc = true
		case c == '"':
			inStr = !inStr
		case !inStr && c == '{':
			depth++
		case !inStr && c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}

// cleanReason は LLM の理由を表示・HTTP 本文に出せる形に整える (制御文字を落とし、
// 長さを抑える)。改行は空白にする。
func cleanReason(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f:
			// 制御文字は落とす
		default:
			b.WriteRune(r)
		}
		if b.Len() >= 300 {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

// truncateBytes は b を最大 n バイトに切り詰める。
func truncateBytes(b []byte, n int) []byte {
	if n <= 0 || len(b) <= n {
		return b
	}
	return b[:n]
}

// clipBytes は s をバイト数 n まで (UTF-8 の境界で) 切り詰める。
func clipBytes(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
