package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// 登録の接続でやり取りするメッセージ (新行区切り JSON)。run が登録を送り、
// hub が URL を返す。接続を保持している間だけ登録が有効 (切断 = 解除)。
type registerMsg struct {
	Type    string `json:"type"` // "register"
	ID      string `json:"id"`
	Name    string `json:"name"`
	Repo    string `json:"repo"`
	Agent   string `json:"agent"`
	Session string `json:"session"` // tmux セッション名
	Console string `json:"console"` // console.sock のパス
}

type registerReply struct {
	Type  string `json:"type"` // "registered" / "error"
	URL   string `json:"url,omitempty"`
	Error string `json:"error,omitempty"`
}

// Request は run が hub へ登録する内容。
type Request struct {
	ID      string
	Name    string
	Repo    string
	Agent   string
	Session string
	Console string
}

// Registration は run から hub への登録。Close するまで (接続を保持する間) 有効。
type Registration struct {
	conn net.Conn
	url  string
}

// URL は hub が発行したアクセス URL を返す。
func (r *Registration) URL() string { return r.url }

// Close は登録を解除する。
func (r *Registration) Close() error { return r.conn.Close() }

// Register は hub の socket へ登録する (run 側)。hub が居なければエラー。
func Register(sock string, req Request, timeout time.Duration) (*Registration, error) {
	c, err := net.DialTimeout("unix", sock, timeout)
	if err != nil {
		return nil, err
	}
	msg := registerMsg{
		Type: "register", ID: req.ID, Name: req.Name, Repo: req.Repo,
		Agent: req.Agent, Session: req.Session, Console: req.Console,
	}
	if err := json.NewEncoder(c).Encode(msg); err != nil {
		c.Close()
		return nil, err
	}
	_ = c.SetReadDeadline(time.Now().Add(timeout))
	var rep registerReply
	if err := json.NewDecoder(c).Decode(&rep); err != nil {
		c.Close()
		return nil, fmt.Errorf("hub の応答を読めない: %w", err)
	}
	_ = c.SetReadDeadline(time.Time{})
	if rep.Type != "registered" {
		c.Close()
		if rep.Error != "" {
			return nil, errors.New(rep.Error)
		}
		return nil, errors.New("hub が登録を拒否した")
	}
	return &Registration{conn: c, url: rep.URL}, nil
}

// listenRegister は sock (0600) で run からの登録を待ち受ける。
func (s *Server) listenRegister(sock string) (net.Listener, error) {
	if dir := filepath.Dir(sock); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	_ = os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go s.handleRegister(c)
		}
	}()
	return l, nil
}

// handleRegister は 1 件の登録を処理し、接続が切れるまで保持する。
func (s *Server) handleRegister(c net.Conn) {
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(registerTimeout))
	var m registerMsg
	if err := json.NewDecoder(c).Decode(&m); err != nil {
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	reject := func(reason string) {
		_ = json.NewEncoder(c).Encode(registerReply{Type: "error", Error: reason})
	}
	if m.Type != "register" {
		reject("登録の種類が不正")
		return
	}
	if strings.TrimSpace(m.ID) == "" || strings.TrimSpace(m.Session) == "" {
		reject("登録の内容が不正")
		return
	}
	sess, err := newSession(cleanMeta(m.ID, 64), cleanMeta(m.Name, 120), cleanMeta(m.Repo, 300),
		cleanMeta(m.Agent, 40), m.Session, m.Console)
	if err != nil {
		reject(err.Error())
		return
	}
	if err := s.reg.add(sess); err != nil {
		reject(err.Error())
		return
	}
	url := sess.URL(s.baseURL)
	if err := json.NewEncoder(c).Encode(registerReply{Type: "registered", URL: url}); err != nil {
		s.reg.remove(sess.ID)
		return
	}
	s.logf("セッションを登録: %s (%s) %s", sess.Name, sess.Agent, url)
	s.notifySessionStart(sess, url)

	// 接続が切れるまで保持する (切断 = 解除)。run からのデータは使わない。
	_, _ = io.Copy(io.Discard, c)
	s.reg.remove(sess.ID)
	s.logf("セッションの登録が切れた: %s", sess.Name)
}

// cleanMeta は run から渡されるメタデータを表示用に整える (制御文字を落とし長さを切る)。
func cleanMeta(s string, maxRunes int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if utf8.RuneCountInString(s) > maxRunes {
		r := []rune(s)
		s = string(r[:maxRunes]) + "…"
	}
	return strings.TrimSpace(s)
}
