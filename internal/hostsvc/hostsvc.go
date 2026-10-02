// Package hostsvc は guest から到達できる唯一の host 側窓口 (HTTP over unix socket)。
//
// qemu の guestfwd が guest の GuestAddr への TCP 接続を socat 経由でこの socket に
// 中継する。ネットワーク上の穴は開けず、nftables も通らない。認証プロキシや
// MCP サーバーはここにルートを足して載せる。
package hostsvc

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// GuestAddr は guest から見た窓口のアドレス (qemu user-net の仮想 IP)。
	GuestAddr = "10.0.2.200"
	// GuestHost は guest の /etc/hosts に登録する名前。
	GuestHost = "quagent.host"
)

// Server は窓口の HTTP サーバー。
type Server struct {
	Mux  *http.ServeMux
	sock string
	srv  *http.Server
}

// New は sock で待ち受けるサーバーを作る。ルートは Mux に登録してから Start する。
func New(sock string) *Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	return &Server{Mux: mux, sock: sock}
}

// Start は待ち受けを始める。
func (s *Server) Start() error {
	_ = os.Remove(s.sock)
	l, err := net.Listen("unix", s.sock)
	if err != nil {
		return err
	}
	if err := os.Chmod(s.sock, 0o600); err != nil {
		l.Close()
		return err
	}
	s.srv = &http.Server{Handler: s.Mux, ReadHeaderTimeout: 30 * time.Second}
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

// Guestfwd は qemu -netdev user に付ける guestfwd オプションを返す。
// qemu は接続ごとに socat を起動し、その標準入出力を sock につなぐ。
func Guestfwd(sock string) string {
	cmd := "socat STDIO UNIX-CONNECT:" + sock
	// qemu のオプション値の中のカンマは二重にしてエスケープする
	return fmt.Sprintf("guestfwd=tcp:%s:80-cmd:%s", GuestAddr, strings.ReplaceAll(cmd, ",", ",,"))
}
