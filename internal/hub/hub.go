// Package hub は Web 遠隔操作 (hub) のホスト側デーモンを実装する。
//
// run からの登録 (unix socket) を受け、ブラウザへ承認コンソールと端末を中継する。
// 認証はセッションごとのワンタイム URL を Cookie に交換する方式で、待ち受けは
// Tailscale のアドレスに限定する。VM 側には新しい経路を追加しない。
package hub

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nananek/quagent/internal/console"
	"github.com/nananek/quagent/internal/notify"
)

const (
	maxSessions          = 32
	maxViewersPerSession = 4
	maxTermSize          = 500
	wsReadLimit          = 1 << 20
	registerTimeout      = 10 * time.Second
	notifyTTL            = 10 * time.Minute
)

// Config は hub の設定。
type Config struct {
	Addr               string // 待ち受けアドレス (host:port)
	BaseURL            string // 外部から見た基点 (URL 生成と Host 検査に使う)
	Notifier           *notify.Notifier
	NotifySessionStart bool
	NotifyApproval     bool
	Logger             func(string)
}

// Server は Web 遠隔操作の hub 本体。
type Server struct {
	cfg     Config
	reg     *Registry
	baseURL string
	secure  bool

	httpSrv *http.Server
	httpLn  net.Listener
	regLn   net.Listener

	allowedHosts map[string]bool

	mu      sync.Mutex
	viewers map[string]int

	limMu    sync.Mutex
	limStart time.Time
	limCount int

	notifyMu sync.Mutex
	notified map[string]time.Time
}

// New は hub を作る (待ち受けは Serve で開始する)。
func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = func(string) {}
	}
	base := strings.TrimRight(cfg.BaseURL, "/")
	s := &Server{
		cfg:          cfg,
		reg:          newRegistry(maxSessions),
		baseURL:      base,
		viewers:      map[string]int{},
		notified:     map[string]time.Time{},
		allowedHosts: map[string]bool{},
	}
	s.secure = strings.HasPrefix(base, "https://")
	// 許可するホスト名: 基点のホストと、バインド先のホスト (DNS リバインディング対策)。
	if h := hostOnly(base); h != "" {
		s.allowedHosts[strings.ToLower(h)] = true
	}
	if h := hostOnly(cfg.Addr); h != "" {
		s.allowedHosts[strings.ToLower(h)] = true
	}
	s.allowedHosts["localhost"] = true
	s.allowedHosts["127.0.0.1"] = true
	s.allowedHosts["[::1]"] = true
	return s
}

// BaseURL は外部から見た基点を返す。
func (s *Server) BaseURL() string { return s.baseURL }

// Addr は実際に待ち受けているアドレスを返す (Serve 後)。
func (s *Server) Addr() string {
	if s.httpLn == nil {
		return s.cfg.Addr
	}
	return s.httpLn.Addr().String()
}

func (s *Server) logf(format string, args ...any) { s.cfg.Logger(fmt.Sprintf(format, args...)) }

// Serve は登録の待ち受けと HTTP の待ち受けを開始する (HTTP が終わるまでブロックする)。
func (s *Server) Serve(registerSock string) error {
	rl, err := s.listenRegister(registerSock)
	if err != nil {
		return fmt.Errorf("登録用 socket を作れない: %w", err)
	}
	s.regLn = rl

	hl, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		rl.Close()
		return fmt.Errorf("待ち受けできない (%s): %w", s.cfg.Addr, err)
	}
	s.httpLn = hl
	s.httpSrv = &http.Server{Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
	s.logf("待ち受け開始: %s (外部 URL: %s)", hl.Addr(), s.baseURL)
	err = s.httpSrv.Serve(hl)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Close は待ち受けを止める。
func (s *Server) Close() {
	if s.httpSrv != nil {
		_ = s.httpSrv.Close()
	}
	if s.regLn != nil {
		_ = s.regLn.Close()
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/s/", s.handleExchange)
	mux.HandleFunc("/assets/", s.handleAsset)
	mux.HandleFunc("/api/session", s.handleSessionInfo)
	mux.HandleFunc("/api/terminal", s.handleTerminal)
	mux.HandleFunc("/api/console", s.handleConsole)
	mux.HandleFunc("/", s.handleIndex)
	return s.securityHeaders(s.checkHost(mux))
}

// assetContentType は配信を許す同梱資産と、その Content-Type。
// 許可リストにすることで、テンプレートや README が /assets/ から漏れないようにする。
var assetContentType = map[string]string{
	"app.css":      "text/css; charset=utf-8",
	"app.js":       "text/javascript; charset=utf-8",
	"xterm.js":     "text/javascript; charset=utf-8",
	"xterm.css":    "text/css; charset=utf-8",
	"addon-fit.js": "text/javascript; charset=utf-8",
}

// handleAsset は同梱資産のうち許可したものだけを返す。
func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/assets/")
	ct, ok := assetContentType[name]
	if !ok || strings.ContainsRune(name, '/') {
		http.NotFound(w, r)
		return
	}
	b, err := assetsFS.ReadFile("assets/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	_, _ = w.Write(b)
}

// csp は hub のページに適用する Content Security Policy。
//
// script は自前の /assets/*.js だけを許し、eval も inline も許さない。style は
// 自前の CSS に加えて 'unsafe-inline' を許す: 端末表示の xterm.js はターミナル
// ごとに <style> を注入するため、これが無いと端末が描画されない (自前の CSS は
// assets/app.css に外出ししてあるので、'unsafe-inline' が要るのは xterm.js の
// 注入するスタイルだけ)。connect-src の 'self' は同じ origin の WebSocket を含む。
const csp = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; font-src 'self'; connect-src 'self'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'; object-src 'none'"

// securityHeaders は CSP と周辺の防御ヘッダを付ける。
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		// ワンタイム URL やセッション情報をブラウザに残さない (資産は静的なので除く)。
		if !strings.HasPrefix(r.URL.Path, "/assets/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// checkHost は Host ヘッダを許可リストと照合する (DNS リバインディング対策)。
func (s *Server) checkHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.allowedHosts[strings.ToLower(hostOnly(r.Host))] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostOnly は "host:port" や "http://host:port/path" からホスト名だけを取り出す。
func hostOnly(s string) string {
	if s == "" {
		return ""
	}
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/"); i >= 0 {
		s = s[:i]
	}
	// IPv6 の [::1]:port はそのまま括弧ごと返す。
	if strings.HasPrefix(s, "[") {
		if j := strings.Index(s, "]"); j >= 0 {
			return s[:j+1]
		}
		return s
	}
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	return s
}

func (s *Server) acquireViewer(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.viewers[id] >= maxViewersPerSession {
		return false
	}
	s.viewers[id]++
	return true
}

func (s *Server) releaseViewer(id string) {
	s.mu.Lock()
	if s.viewers[id] > 0 {
		s.viewers[id]--
	}
	s.mu.Unlock()
}

// notifySessionStart はセッション開始を通知する。
func (s *Server) notifySessionStart(sess *Session, url string) {
	if !s.cfg.Notifier.Enabled() || !s.cfg.NotifySessionStart {
		return
	}
	body := fmt.Sprintf("エージェント: %s\nリポジトリ: %s", sess.Agent, sess.Repo)
	s.cfg.Notifier.Send(notify.Message{
		Title: "quagent: セッションを開始 (" + sess.Name + ")",
		Body:  body,
		URL:   url,
	})
}

// maybeNotifyApproval は承認ダイアログの発生を通知する。複数の閲覧者がいても
// 同じ申請で二重に通知しないよう、申請 ID で重複を除く。
func (s *Server) maybeNotifyApproval(sess *Session, m console.Msg) {
	if !s.cfg.Notifier.Enabled() || !s.cfg.NotifyApproval {
		return
	}
	title, body, ok := approvalSummary(m)
	if !ok {
		return
	}
	if !s.markNotified(sess.ID + "|" + m.Type + "|" + strconv.Itoa(m.ID)) {
		return
	}
	s.cfg.Notifier.Send(notify.Message{
		Title: "quagent: " + title,
		Body:  body,
		URL:   sess.LinkURL(s.baseURL),
	})
}

// markNotified は key が未通知なら true を返して記録する。
func (s *Server) markNotified(key string) bool {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	now := time.Now()
	for k, t := range s.notified {
		if now.Sub(t) > notifyTTL {
			delete(s.notified, k)
		}
	}
	if _, ok := s.notified[key]; ok {
		return false
	}
	s.notified[key] = now
	return true
}

// approvalSummary は承認要求メッセージを通知用の見出しと本文に変換する。
func approvalSummary(m console.Msg) (title, body string, ok bool) {
	switch m.Type {
	case "request":
		return "接続先の申請", "ドメイン: " + strings.Join(m.Domains, ", ") + "\n理由: " + m.Reason, true
	case "relaxrequest":
		return "ヘッダ緩和の申請", "ホスト: " + m.RelaxHost + "\n理由: " + m.Reason, true
	case "prrequest":
		return "PR 作成の承認", m.Branch + " → " + m.Base + "\n" + m.Title, true
	case "dockerrequest":
		return "Docker イメージの取得", m.DockerRegistry + "/" + m.DockerRepository + ":" + m.DockerReference, true
	case "batchrequest":
		return "パッケージ取得の承認", m.BatchEcosystem + ": " + strconv.Itoa(len(m.BatchItems)) + " 件", true
	case "clip":
		return "クリップボードへの書き込み", m.Text, true
	}
	return "", "", false
}
