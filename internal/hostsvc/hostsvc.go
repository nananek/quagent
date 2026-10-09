// Package hostsvc は guest から到達できる唯一の host 側窓口 (HTTP over vsock)。
//
// host の quagent 本体が run ごとのポートで vsock を待ち受け、VM 内の受け口
// (quagent __guest) が guest の 127.0.0.1:GuestPort への接続をそこへ中継する。
// ネットワーク上の穴は開けず、nftables も通らない。接続ごとに host でプロセスを
// 起こさないので、接続を大量に張られても host は耐える (同時接続にも上限がある)。
// 認証プロキシや MCP サーバーはここにルートを足して載せる。
package hostsvc

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mdlayher/vsock"
)

const (
	// GuestHost は guest の /etc/hosts に登録する名前 (127.0.0.1 を指す)。
	GuestHost = "quagent.host"
	// GuestPort は guest 内で中継を待ち受けるポート。
	GuestPort = 7070
	// maxConns は同時に受け付ける接続の上限。
	maxConns = 64
)

// GuestOrigin は guest から見た窓口の URL の先頭 (http://quagent.host:7070)。
func GuestOrigin() string { return fmt.Sprintf("http://%s:%d", GuestHost, GuestPort) }

// AuditEvent は guest から host の窓口への 1 リクエスト。VM が何を host に
// 求めたかは host 信頼で記録できる (VM の中の記録とは違い改変されない)。
type AuditEvent struct {
	Method string
	Path   string
	Status int
	Bytes  int64
	Took   time.Duration
}

// LogLine は監査イベントを host.log の 1 行にする。Method / Path は VM が決められる
// ので %q で書く (改行や制御文字で別のログ行を偽装させない)。
func (e AuditEvent) LogLine() string {
	return fmt.Sprintf("audit: %q %q -> %d (%d bytes, %s)",
		e.Method, e.Path, e.Status, e.Bytes, e.Took.Round(time.Millisecond))
}

// Server は窓口の HTTP サーバー。
type Server struct {
	Mux *http.ServeMux
	// Audit が non-nil なら、窓口へのリクエストごとに呼ぶ (host 側の監査ログ用)。
	Audit func(AuditEvent)
	// OnReject が non-nil なら、窓口への vsock 接続を拒否したときに呼ぶ (host 側の監査ログ用)。
	OnReject func(reason string, remote net.Addr)
	// Token は run ごとの合言葉。エージェントの設定にだけ書き、/healthz 以外は
	// これを持たないリクエストを拒否する (VM 内のエージェント以外のプロセスや
	// コンテナが、プロキシ経由で鍵や MCP を使えないように)。
	Token string
	// ExtraTokens は Token の代わりに受け付ける合言葉 (agy のサブスクリプションで
	// guest に書いた短命トークンなど、run ごとに host が用意したもの)。
	ExtraTokens []string
	// Port は host で待ち受ける vsock のポート (Start で決まる)。
	Port uint32
	cid  uint32 // 受け付ける VM の CID (他の VM からの接続は切る)
	srv  *http.Server
}

// New は VM (cid) からの接続だけを受ける窓口を作る。ルートは Mux に登録してから Start する。
func New(cid uint32) (*Server, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	return &Server{Mux: mux, Token: "qa-" + hex.EncodeToString(b), cid: cid}, nil
}

// authorized はリクエストが合言葉を持っているかを返す。
func (s *Server) authorized(r *http.Request) bool {
	wants := append([]string{s.Token}, s.ExtraTokens...)
	for _, got := range []string{
		strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
		r.Header.Get("X-Api-Key"),
	} {
		for _, want := range wants {
			if subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
				return true
			}
		}
	}
	return false
}

// Handler は窓口の HTTP ハンドラ (合言葉の確認つき)。テスト用にも使う。
func (s *Server) Handler() http.Handler { return s.handler() }

func (s *Server) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Audit == nil {
			if r.URL.Path != "/healthz" && !s.authorized(r) {
				http.Error(w, "quagent: unauthorized", http.StatusUnauthorized)
				return
			}
			s.Mux.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &auditWriter{ResponseWriter: w, status: http.StatusOK}
		if r.URL.Path != "/healthz" && !s.authorized(r) {
			http.Error(rec, "quagent: unauthorized", http.StatusUnauthorized)
		} else {
			s.Mux.ServeHTTP(rec, r)
		}
		s.Audit(AuditEvent{
			Method: r.Method, Path: r.URL.Path,
			Status: rec.status, Bytes: rec.bytes, Took: time.Since(start),
		})
	})
}

// auditWriter はステータスと送信バイト数を数える。Flush は元の実装へ通す
// (ストリーミングを止めない)。
type auditWriter struct {
	http.ResponseWriter
	status  int
	bytes   int64
	written bool
}

func (w *auditWriter) WriteHeader(code int) {
	if !w.written {
		w.status = code
		w.written = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *auditWriter) Write(p []byte) (int, error) {
	w.written = true
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *auditWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap は包んでいる ResponseWriter を返す。http.ResponseController などが
// Hijack / SetWriteDeadline のような追加機能を元の実装まで辿れるようにする。
func (w *auditWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Start は host の vsock で待ち受けを始める (ポートは空いているものを選ぶ)。
func (s *Server) Start() error {
	var l net.Listener
	var err error
	for range 20 {
		n, _ := rand.Int(rand.Reader, big.NewInt(40000))
		port := uint32(20000 + n.Int64())
		if l, err = vsock.Listen(port, nil); err == nil {
			s.Port = port
			break
		}
	}
	if err != nil {
		return fmt.Errorf("vsock で待ち受けられない: %w", err)
	}
	return s.serve(&guardListener{Listener: l, cid: s.cid, sem: make(chan struct{}, maxConns), onReject: s.OnReject})
}

func (s *Server) serve(l net.Listener) error {
	s.srv = &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 30 * time.Second,
		MaxHeaderBytes:    64 << 10,
		IdleTimeout:       2 * time.Minute,
	}
	go func() { _ = s.srv.Serve(l) }()
	return nil
}

// Stop は待ち受けを止める。
func (s *Server) Stop() {
	if s.srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.srv.Shutdown(ctx)
}

// guardListener はこの run の VM 以外からの接続を切り、同時接続数を絞る。
type guardListener struct {
	net.Listener
	cid      uint32
	sem      chan struct{}
	onReject func(reason string, remote net.Addr)
}

func (l *guardListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		a, ok := c.RemoteAddr().(*vsock.Addr)
		if !ok || a.ContextID != l.cid {
			if l.onReject != nil {
				l.onReject(fmt.Sprintf("vsock 不正な CID からの接続を拒否 (want_cid=%d)", l.cid), c.RemoteAddr())
			}
			c.Close() // 別の VM から
			continue
		}
		select {
		case l.sem <- struct{}{}:
			return &releaseConn{Conn: c, release: func() { <-l.sem }}, nil
		default:
			if l.onReject != nil {
				l.onReject(fmt.Sprintf("vsock 同時接続上限 (%d) 超過のため拒否", cap(l.sem)), c.RemoteAddr())
			}
			c.Close() // 同時接続の上限
		}
	}
}

type releaseConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (c *releaseConn) Close() error {
	c.once.Do(c.release)
	return c.Conn.Close()
}
