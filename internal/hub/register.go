package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nananek/quagent/internal/paths"
)

// 登録の接続でやり取りするメッセージ (新行区切り JSON)。run が登録を送り、
// hub が URL を返す。接続を保持している間だけ登録が有効 (切断 = 解除)。
// 登録後も同じ接続で URL の再発行を要求できる。
type registerMsg struct {
	Type    string `json:"type"` // "register" / "reissue"
	ID      string `json:"id"`
	Name    string `json:"name"`
	Repo    string `json:"repo"`
	Agent   string `json:"agent"`
	Session string `json:"session"` // tmux セッション名
	Console string `json:"console"` // console.sock のパス
	// ThemeFile は hub がブラウザの配色を書き込むファイル。run が端末の配色を
	// 判定できなかったとき、__attach が色問い合わせに答えるのに使う。
	ThemeFile string `json:"theme_file"`
}

type registerReply struct {
	Type  string `json:"type"` // "registered" / "reissued" / "error"
	URL   string `json:"url,omitempty"`
	Error string `json:"error,omitempty"`
}

// Request は run が hub へ登録する内容。
type Request struct {
	ID        string
	Name      string
	Repo      string
	Agent     string
	Session   string
	Console   string
	ThemeFile string
}

// Registration は run から hub への登録。Close するまで (接続を保持する間) 有効。
type Registration struct {
	conn net.Conn
	enc  *json.Encoder
	dec  *json.Decoder

	mu  sync.Mutex
	url string
}

// URL は hub が発行したアクセス URL を返す。
func (r *Registration) URL() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.url
}

// Close は登録を解除する。
func (r *Registration) Close() error { return r.conn.Close() }

// reissueTimeout は URL 再発行の往復を待つ上限。
const reissueTimeout = 5 * time.Second

// Reissue は hub に新しいアクセス URL を発行させる。登録の接続を保持している間だけ
// 使える。発行済みの端末はそのまま使え、新しい端末を追加するための URL が返る。
func (r *Registration) Reissue() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.enc.Encode(registerMsg{Type: "reissue"}); err != nil {
		return "", fmt.Errorf("hub に再発行を要求できない: %w", err)
	}
	_ = r.conn.SetReadDeadline(time.Now().Add(reissueTimeout))
	var rep registerReply
	if err := r.dec.Decode(&rep); err != nil {
		return "", fmt.Errorf("hub の応答を読めない: %w", err)
	}
	_ = r.conn.SetReadDeadline(time.Time{})
	if rep.Type != "reissued" {
		if rep.Error != "" {
			return "", errors.New(rep.Error)
		}
		return "", errors.New("hub が再発行を拒否した")
	}
	r.url = rep.URL
	return rep.URL, nil
}

// Register は hub の socket へ登録する (run 側)。hub が居なければエラー。
func Register(sock string, req Request, timeout time.Duration) (*Registration, error) {
	c, err := net.DialTimeout("unix", sock, timeout)
	if err != nil {
		return nil, err
	}
	enc := json.NewEncoder(c)
	dec := json.NewDecoder(c)
	msg := registerMsg{
		Type: "register", ID: req.ID, Name: req.Name, Repo: req.Repo,
		Agent: req.Agent, Session: req.Session, Console: req.Console,
		ThemeFile: req.ThemeFile,
	}
	if err := enc.Encode(msg); err != nil {
		c.Close()
		return nil, err
	}
	_ = c.SetReadDeadline(time.Now().Add(timeout))
	var rep registerReply
	if err := dec.Decode(&rep); err != nil {
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
	return &Registration{conn: c, enc: enc, dec: dec, url: rep.URL}, nil
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

// handleRegister は 1 件の登録を処理し、接続が切れるまで保持する。接続中の再発行要求
// (URL の再発行) にも応える。
func (s *Server) handleRegister(c net.Conn) {
	defer c.Close()
	enc := json.NewEncoder(c)
	dec := json.NewDecoder(c)
	_ = c.SetReadDeadline(time.Now().Add(registerTimeout))
	var m registerMsg
	if err := dec.Decode(&m); err != nil {
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	reject := func(reason string) {
		_ = enc.Encode(registerReply{Type: "error", Error: reason})
	}
	if m.Type != "register" {
		reject("登録の種類が不正")
		return
	}
	if strings.TrimSpace(m.ID) == "" || strings.TrimSpace(m.Session) == "" {
		reject("登録の内容が不正")
		return
	}
	// console.sock と tmux セッション名は run が使う値に限定する。無検証だと
	// 常駐 daemon が任意の unix socket や tmux セッションを対象にしてしまう。
	if !validConsolePath(m.Console) {
		reject("console.sock のパスが不正")
		return
	}
	if !validThemeFile(m.ThemeFile) {
		reject("テーマファイルのパスが不正")
		return
	}
	if !validSessionName(m.Session) {
		reject("tmux セッション名が不正")
		return
	}
	sess, err := newSession(cleanMeta(m.ID, 64), cleanMeta(m.Name, 120), cleanMeta(m.Repo, 300),
		cleanMeta(m.Agent, 40), m.Session, m.Console)
	if err != nil {
		reject(err.Error())
		return
	}
	sess.ThemeFile = m.ThemeFile
	if err := s.reg.add(sess); err != nil {
		reject(err.Error())
		return
	}
	url := sess.URL(s.baseURL)
	if err := enc.Encode(registerReply{Type: "registered", URL: url}); err != nil {
		s.reg.removeIf(sess.ID, sess)
		return
	}
	s.logf("セッションを登録: %s (%s)", sess.Name, sess.Agent)
	s.notifySessionStart(sess, url)

	// 接続が切れるまで保持する (切断 = 解除)。run からのデータは再発行要求だけ。
	for {
		var req registerMsg
		if err := dec.Decode(&req); err != nil {
			break
		}
		if req.Type != "reissue" {
			continue
		}
		newURL, err := s.reissue(sess)
		if err != nil {
			_ = enc.Encode(registerReply{Type: "error", Error: err.Error()})
			continue
		}
		_ = enc.Encode(registerReply{Type: "reissued", URL: newURL})
	}
	s.reg.removeIf(sess.ID, sess)
	s.logf("セッションの登録が切れた: %s", sess.Name)
}

// reissue はセッションの URL を再発行する (登録中の run からの要求で呼ぶ)。
func (s *Server) reissue(sess *Session) (string, error) {
	url, err := sess.Reissue(s.baseURL)
	if err != nil {
		return "", err
	}
	s.logf("セッションのアクセス URL を再発行: %s", sess.Name)
	s.notifyReissue(sess, url)
	return url, nil
}

// validConsolePath は console.sock のパスが run の作業ディレクトリ配下の
// console.sock であることを確かめる。
func validConsolePath(p string) bool { return validRunsFile(p, "console.sock") }

// validThemeFile はテーマファイルのパスが run の作業ディレクトリ配下の theme で
// あることを確かめる。hub はここへブラウザの配色を書き込むので、run が使う値に
// 限定する (常駐 daemon に任意のファイルを書かせない)。空は未指定として許す
// (run 側が配色を判定できていれば不要)。
func validThemeFile(p string) bool {
	if p == "" {
		return true
	}
	return validRunsFile(p, "theme")
}

// validRunsFile はパスが run の作業ディレクトリ (RunsDir 直下) の base という
// 名前のファイルであることを確かめる。
func validRunsFile(p, base string) bool {
	if p == "" {
		return false
	}
	clean := filepath.Clean(p)
	if filepath.Base(clean) != base {
		return false
	}
	root := filepath.Clean(paths.RunsDir())
	rel, err := filepath.Rel(root, clean)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}

var sessionNameRe = regexp.MustCompile(`^quagent-[0-9A-Za-z._-]{1,120}$`)

// validSessionName は tmux セッション名が run の命名 (quagent-<作業ディレクトリ名>)
// に合うことを確かめる。
func validSessionName(s string) bool { return sessionNameRe.MatchString(s) }

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
