package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nananek/quagent/internal/console"
	"github.com/nananek/quagent/internal/ws"
)

// cookieName は交換後のアクセスに使う Cookie の名前。
const cookieName = "quagent_hub"

// 端末の出力を溜める上限。溢れたら遅いクライアントとみなして切断する
// (再接続時に現在の画面を送り直すので、表示はすぐ復帰する)。
const terminalQueue = 256

// termInput はブラウザから端末へ送るメッセージ。
type termInput struct {
	Type string `json:"type"`           // "input" / "resize"
	Data string `json:"data,omitempty"` // base64 の入力バイト
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

// handleTerminal はエージェントの端末を WebSocket で中継する。
func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.authSession(w, r)
	if !ok {
		return
	}
	if !s.acquireViewer(sess.ID) {
		http.Error(w, "同時接続数の上限に達している", http.StatusServiceUnavailable)
		return
	}
	defer s.releaseViewer(sess.ID)

	c, err := s.acceptWS(w, r)
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(wsReadLimit)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	out := make(chan []byte, terminalQueue)
	var overflowOnce sync.Once
	overflow := make(chan struct{})
	sink := func(b []byte) {
		select {
		case out <- b:
		default:
			overflowOnce.Do(func() { close(overflow) })
		}
	}
	cc, err := startControl(sess.Tmux, sink)
	if err != nil {
		s.logf("端末へ接続できない (%s): %v", sess.Tmux, err)
		_ = c.Close(ws.StatusInternalError, "端末へ接続できない")
		return
	}
	defer cc.Close()

	// 入力・リサイズを受ける。
	go func() {
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				cancel()
				return
			}
			var m termInput
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			switch m.Type {
			case "input":
				b, err := base64.StdEncoding.DecodeString(m.Data)
				if err == nil {
					_ = cc.Send(b)
				}
			case "resize":
				_ = cc.Resize(m.Cols, m.Rows)
			}
		}
	}()

	// 繋いだ直後は現在の画面を送る (control mode は新しい出力しか流さないため)。
	if screen, err := cc.Capture(); err == nil && len(screen) > 0 {
		if err := writeBinary(ctx, c, screen); err != nil {
			return
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-cc.Done():
			_ = c.Close(ws.StatusNormalClosure, "端末が終了した")
			return
		case <-overflow:
			_ = c.Close(ws.StatusPolicyViolation, "クライアントが遅い")
			return
		case b := <-out:
			if err := writeBinary(ctx, c, b); err != nil {
				return
			}
		}
	}
}

// handleConsole は承認コンソール (console.sock) を WebSocket で中継する。
func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.authSession(w, r)
	if !ok {
		return
	}
	if !s.acquireViewer(sess.ID) {
		http.Error(w, "同時接続数の上限に達している", http.StatusServiceUnavailable)
		return
	}
	defer s.releaseViewer(sess.ID)

	c, err := s.acceptWS(w, r)
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(wsReadLimit)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	send := make(chan console.Msg, terminalQueue)
	var overflowOnce sync.Once
	overflow := make(chan struct{})
	bridge := newConsoleBridge(sess.Console, func(m console.Msg) {
		s.maybeNotifyApproval(sess, m)
		select {
		case send <- m:
		default:
			overflowOnce.Do(func() { close(overflow) })
		}
	})
	defer bridge.Close()

	go func() {
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				cancel()
				return
			}
			var m console.Msg
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			if !isDecision(m.Type) {
				continue
			}
			if err := bridge.Send(m); err != nil {
				s.logf("承認の判断を送れない: %v", err)
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-overflow:
			_ = c.Close(ws.StatusPolicyViolation, "クライアントが遅い")
			return
		case m := <-send:
			b, err := json.Marshal(m)
			if err != nil {
				continue
			}
			wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
			err = c.Write(wctx, ws.MessageText, b)
			wcancel()
			if err != nil {
				return
			}
		}
	}
}

// handleSessionInfo はブラウザにセッションのメタデータを返す。
func (s *Server) handleSessionInfo(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.authSession(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"id": sess.ID, "name": sess.Name, "repo": sess.Repo, "agent": sess.Agent,
	})
}

// isDecision はブラウザから受け付ける判断メッセージかを返す。承認コンソールの
// プロトコルのうち、判断に関わる種別だけを通す (ui / clipboard / quit は通さない)。
func isDecision(t string) bool {
	switch t {
	case "decide", "relaxdecide", "prdecide", "dockerdecide", "batchdecide", "clipdecide":
		return true
	}
	return false
}

func writeBinary(ctx context.Context, c *ws.Conn, b []byte) error {
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.Write(wctx, ws.MessageBinary, b)
}

// acceptWS は WebSocket を確立する。Cookie (SameSite=Lax) と Host 検査で守るため、
// Origin は既定の同一オリジン検査に任せる (ブラウザ以外で Origin が無い接続は通す)。
func (s *Server) acceptWS(w http.ResponseWriter, r *http.Request) (*ws.Conn, error) {
	return ws.Accept(w, r)
}

// authSession は Cookie からセッションを引く。無効なら応答を書いて false を返す。
func (s *Server) authSession(w http.ResponseWriter, r *http.Request) (*Session, bool) {
	ck, err := r.Cookie(cookieName)
	if err != nil || ck.Value == "" {
		s.unauthorized(w)
		return nil, false
	}
	sess := s.reg.byCookie(ck.Value)
	if sess == nil {
		s.unauthorized(w)
		return nil, false
	}
	return sess, true
}

func (s *Server) unauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte("<!doctype html><meta charset=utf-8><title>quagent</title>" +
		"<p>このページを見るには、通知されたワンタイム URL を開いてください。</p>"))
}

// handleExchange はワンタイム URL のシークレットを Cookie に交換する。
func (s *Server) handleExchange(w http.ResponseWriter, r *http.Request) {
	if !s.allowExchange() {
		http.Error(w, "試行が多すぎます。しばらくしてからどうぞ。", http.StatusTooManyRequests)
		return
	}
	secret := strings.TrimPrefix(r.URL.Path, "/s/")
	if secret == "" || strings.ContainsRune(secret, '/') {
		http.NotFound(w, r)
		return
	}
	sess := s.reg.bySecret(secret)
	if sess == nil {
		http.Error(w, "この URL は無効です。", http.StatusNotFound)
		return
	}
	token, ok := sess.Exchange(secret)
	if !ok {
		http.Error(w, "この URL は使用済みです。新しい URL の発行を受けてください。", http.StatusNotFound)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: token, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.secure,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleIndex はアプリの HTML を返す。
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	sess, ok := s.authSession(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// セッション名だけ差し込む (それ以外は静的な JS が /api/session から取る)。
	_ = indexTemplate.Execute(w, map[string]string{"Name": sess.Name})
}

// allowExchange は短時間の交換試行を制限する (総当たり対策)。
func (s *Server) allowExchange() bool {
	s.limMu.Lock()
	defer s.limMu.Unlock()
	now := time.Now()
	if now.Sub(s.limStart) > 10*time.Second {
		s.limStart, s.limCount = now, 0
	}
	s.limCount++
	return s.limCount <= 30
}
