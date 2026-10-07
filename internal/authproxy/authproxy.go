// Package authproxy は guest の LLM API 呼び出しに host 側で秘密を付けて転送する。
// 秘密は host のプロセスから出ず、guest には API の到達経路も与えない。
package authproxy

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"

	"github.com/nananek/quagent/internal/config"
	"github.com/nananek/quagent/internal/guard"
)

// Prefix は窓口上のルートの前置き。guest からは <窓口>/llm/<id>/... で使う。
const Prefix = "/llm/"

// guest が送ってきても捨てる認証系ヘッダ。
var strippedHeaders = []string{"Authorization", "X-Api-Key", "Api-Key", "X-Goog-Api-Key", "Proxy-Authorization", "Cookie"}

// DefaultAllow は allow を設定しない provider で転送する操作 (推論とモデル一覧だけ)。
// upstream に /v1 を含めない provider (api.anthropic.com など) のため /v1 付きも挙げ、
// Gemini API のため /v1beta 付きや POST /models/* も挙げる。
var DefaultAllow = func() []string {
	ops := []string{
		"POST /messages", "POST /messages/count_tokens", "POST /chat/completions",
		"POST /responses", "POST /models/*", "GET /models", "GET /models/*",
	}
	var out []string
	for _, op := range ops {
		m, p, _ := strings.Cut(op, " ")
		out = append(out, op, m+" /v1"+p, m+" /v1beta"+p)
	}
	// Claude Code の疎通確認
	return append(out, "HEAD /api/hello")
}()

type rule struct {
	method, path string
	prefix       bool
}

func parseRules(ops []string) ([]rule, error) {
	var rules []rule
	for _, op := range ops {
		m, p, ok := strings.Cut(strings.TrimSpace(op), " ")
		p = strings.TrimSpace(p)
		if !ok || m == "" || m != strings.ToUpper(m) || !strings.HasPrefix(p, "/") {
			return nil, fmt.Errorf("allow は \"POST /path\" の形にする: %q", op)
		}
		r := rule{method: m, path: p}
		if strings.HasSuffix(p, "*") {
			r.path, r.prefix = strings.TrimSuffix(p, "*"), true
		}
		rules = append(rules, r)
	}
	return rules, nil
}

func allowed(rules []rule, method, path string) bool {
	for _, r := range rules {
		if r.method == method && (path == r.path || r.prefix && strings.HasPrefix(path, r.path)) {
			return true
		}
	}
	return false
}

// Register は providers の転送ルートを mux に登録し、登録した provider ID を返す。
// 秘密は起動時に一度だけ取り出す (secret_command の対話を run 開始時に済ませるため)。
// 許可していない操作は upstream へ送らず 403 を返し、denied に知らせる (nil 可)。
// g が nil でなければ、転送する前にローカル LLM でリクエストの中身を点検する。
func Register(mux *http.ServeMux, providers map[string]config.Provider, logger *log.Logger, denied func(string), g *guard.Guard) ([]string, error) {
	registers := map[string]secretSource{}
	for id, p := range providers {
		p := p
		secret, err := p.Secret()
		if err != nil {
			return nil, fmt.Errorf("provider %s: 秘密を取り出せない: %w", id, err)
		}
		value := p.HeaderPrefix() + secret
		registers[id] = secretSource{
			upstream: p.Upstream,
			header:   p.HeaderName(),
			secret:   func() (string, error) { return value, nil },
			allow:    p.Allow,
		}
	}
	return registerAll(mux, registers, logger, denied, g)
}

// secretSource は upstream に付ける秘密の出どころ。
type secretSource struct {
	upstream string
	header   string
	// secret はリクエストごとに upstream に付けるヘッダ値を返す (前置き込み)。
	// 短命トークンのように作り直しが必要なものはここで更新する。
	secret func() (string, error)
	// allow は転送してよい操作。空なら推論とモデル一覧だけ (DefaultAllow)。
	allow []string
}

// RegisterDynamic は秘密を作り直しながら使う provider を 1 つ登録する
// (サブスクリプションのように短命トークンで回すもの用)。
func RegisterDynamic(mux *http.ServeMux, id, upstream, header string, secret func() (string, error), allow []string, logger *log.Logger, denied func(string), g *guard.Guard) error {
	_, err := registerAll(mux, map[string]secretSource{
		id: {upstream: upstream, header: header, secret: secret, allow: allow},
	}, logger, denied, g)
	return err
}

func registerAll(mux *http.ServeMux, registers map[string]secretSource, logger *log.Logger, denied func(string), g *guard.Guard) ([]string, error) {
	var ids []string
	for id, reg := range registers {
		up, err := url.Parse(reg.upstream)
		if err != nil || up.Scheme != "https" || up.Host == "" {
			return nil, fmt.Errorf("provider %s: upstream は https の URL にする: %q", id, reg.upstream)
		}
		ops := reg.allow
		if len(ops) == 0 {
			ops = DefaultAllow
		}
		rules, err := parseRules(ops)
		if err != nil {
			return nil, fmt.Errorf("provider %s: %w", id, err)
		}
		header := reg.header
		if header == "" {
			header = "Authorization"
		}
		h := handler(id, up, header, reg.secret, logger)
		mux.Handle(Prefix+id+"/", http.MaxBytesHandler(gate(id, up.Host, rules, h, logger, denied, g), 32<<20))
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// gate は rules に無い操作を upstream へ送らずに断り、g があれば中身を点検する。
// upstreamHost は実際の転送先 (設定の upstream)。r.Host は VM が決められるので
// 点検や承認の表示には使わない。
func gate(id, upstreamHost string, rules []rule, next http.Handler, logger *log.Logger, denied func(string), g *guard.Guard) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, Prefix+id)
		if !allowed(rules, r.Method, rest) {
			logger.Printf("llm %s 許可外 %s %q -> 403", id, r.Method, rest)
			if denied != nil {
				denied(fmt.Sprintf("%s %s %q", id, r.Method, rest))
			}
			http.Error(w, fmt.Sprintf("quagent: %s %s is not allowed by the LLM proxy (only inference endpoints are forwarded)", r.Method, rest), http.StatusForbidden)
			return
		}
		if g != nil {
			body, truncated, err := guard.PeekBody(r, g.InspectLimit())
			if err != nil {
				logger.Printf("llm %s 本文を読めない (%v) -> 403", id, err)
				if denied != nil {
					denied(fmt.Sprintf("%s %s %q: 本文を読めない: %v", id, r.Method, rest, err))
				}
				http.Error(w, "quagent: the request body could not be inspected, so it was not forwarded", http.StatusForbidden)
				return
			}
			req := guard.RequestFrom(r, id, body, truncated)
			req.Host = upstreamHost
			// 転送前に落とすヘッダ (窓口の合言葉など) はローカル LLM にも見せない
			req.Headers = r.Header.Clone()
			for _, h := range strippedHeaders {
				req.Headers.Del(h)
			}
			if err := g.Check(r.Context(), req); err != nil {
				logger.Printf("llm %s コンテンツガードが止めた %s %q: %v", id, r.Method, rest, err)
				if denied != nil {
					denied(fmt.Sprintf("%s %s %q: %v", id, r.Method, rest, err))
				}
				http.Error(w, "quagent: blocked by the request content guard: "+err.Error(), http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func handler(id string, up *url.URL, header string, secret func() (string, error), logger *log.Logger) http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			rest := strings.TrimPrefix(r.In.URL.Path, Prefix+id)
			r.Out.URL.Scheme = up.Scheme
			r.Out.URL.Host = up.Host
			r.Out.URL.Path = strings.TrimSuffix(up.Path, "/") + rest
			r.Out.URL.RawPath = ""
			r.Out.Host = up.Host
			for _, h := range strippedHeaders {
				r.Out.Header.Del(h)
			}
			value, err := secret()
			if err != nil {
				logger.Printf("llm %s 秘密を作り直せない (%v)", id, err)
				return
			}
			r.Out.Header.Set(header, value)
		},
		// ストリーミング応答 (SSE) をそのまま流す
		FlushInterval: -1,
		ErrorLog:      logger,
		ModifyResponse: func(resp *http.Response) error {
			// path は VM が決めるので %q で書く (改行や制御文字でログを偽装させない)
			logger.Printf("llm %s %s %q -> %d", id, resp.Request.Method, resp.Request.URL.Path, resp.StatusCode)
			return nil
		},
	}
	return rp
}

// GuestBaseURL は guest に設定する provider の baseURL を返す (origin は窓口の URL の先頭)。
func GuestBaseURL(origin, id string) string {
	return origin + Prefix + id
}
