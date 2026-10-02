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

// Server は窓口の HTTP サーバー。
type Server struct {
	Mux *http.ServeMux
	// Token は run ごとの合言葉。エージェントの設定にだけ書き、/healthz 以外は
	// これを持たないリクエストを拒否する (VM 内のエージェント以外のプロセスや
	// コンテナが、プロキシ経由で鍵や MCP を使えないように)。
	Token string
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
	want := []byte(s.Token)
	for _, got := range []string{
		strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
		r.Header.Get("X-Api-Key"),
	} {
		if subtle.ConstantTimeCompare([]byte(got), want) == 1 {
			return true
		}
	}
	return false
}

func (s *Server) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" && !s.authorized(r) {
			http.Error(w, "quagent: unauthorized", http.StatusUnauthorized)
			return
		}
		s.Mux.ServeHTTP(w, r)
	})
}

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
	return s.serve(&guardListener{Listener: l, cid: s.cid, sem: make(chan struct{}, maxConns)})
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
	cid uint32
	sem chan struct{}
}

func (l *guardListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if a, ok := c.RemoteAddr().(*vsock.Addr); !ok || a.ContextID != l.cid {
			c.Close() // 別の VM から
			continue
		}
		select {
		case l.sem <- struct{}{}:
			return &releaseConn{Conn: c, release: func() { <-l.sem }}, nil
		default:
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
