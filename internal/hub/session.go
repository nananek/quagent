package hub

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// secretBytes はワンタイム URL のシークレットと Cookie のトークンの長さ。
const secretBytes = 32

// maxCookies は 1 セッションで同時に有効な Cookie の上限。URL を再発行するたびに
// 端末が増えるため、溢れたら古いものから捨てる。
const maxCookies = 8

// Session は登録された 1 つの run。
type Session struct {
	ID         string
	Name       string
	Repo       string
	Agent      string
	Tmux       string // tmux セッション名 (端末の中継に使う)
	Console    string // console.sock のパス (承認の中継に使う)
	ThemeFile  string // ブラウザの配色を書くファイル (端末の配色判定が失敗したとき用)
	Registered time.Time

	mu        sync.Mutex
	secret    string   // 現在のワンタイム URL のシークレット (交換で消費される)
	exchanged bool     // 現在のシークレットを交換済みか
	cookies   []string // 交換で発行した Cookie のトークン (再発行しても残す)
}

// URL は base を基点としたアクセス URL を返す。
func (s *Session) URL(base string) string {
	s.mu.Lock()
	secret := s.secret
	s.mu.Unlock()
	return linkURL(base, secret)
}

// linkURL は基点とシークレットからワンタイム URL を組み立てる。
func linkURL(base, secret string) string {
	return strings.TrimRight(base, "/") + "/s/" + secret
}

// LinkURL は通知に載せる URL を返す。未交換のシークレットがあればワンタイム URL、
// 交換済みなら基点 (既に Cookie を持っているのでシークレットは不要)。
func (s *Session) LinkURL(base string) string {
	s.mu.Lock()
	exchanged, secret := s.exchanged, s.secret
	s.mu.Unlock()
	if exchanged {
		return strings.TrimRight(base, "/") + "/"
	}
	return linkURL(base, secret)
}

// Exchange はシークレットを Cookie のトークンに交換する。未交換で一致したときだけ
// 成功し、以降その URL のシークレットは無効になる (ワンタイム)。発行済みの Cookie は
// 有効なまま残す (再発行で複数の端末から使える)。
func (s *Session) Exchange(secret string) (cookie string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exchanged || !secureEqual(s.secret, secret) {
		return "", false
	}
	s.exchanged = true
	tok := newToken()
	if tok != "" {
		s.cookies = append(s.cookies, tok)
		if len(s.cookies) > maxCookies {
			s.cookies = s.cookies[len(s.cookies)-maxCookies:]
		}
	}
	return tok, true
}

// Reissue は新しいワンタイム URL を発行して返す。以前のシークレットは使えなくなるが、
// 発行済みの Cookie はそのまま有効で、既存の端末は接続を続けられる。
func (s *Session) Reissue(base string) (string, error) {
	secret, err := newSecret()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.secret = secret
	s.exchanged = false
	s.mu.Unlock()
	return linkURL(base, secret), nil
}

// Authorize は Cookie のトークンがこのセッションに有効かを返す。
func (s *Session) Authorize(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.cookies {
		if secureEqual(c, token) {
			return true
		}
	}
	return false
}

// Registry は登録されたセッションを保持する。
type Registry struct {
	mu       sync.Mutex
	sessions map[string]*Session
	max      int
}

func newRegistry(max int) *Registry {
	return &Registry{sessions: map[string]*Session{}, max: max}
}

var errRegistryFull = errors.New("登録できるセッション数の上限に達している")

// errIDInUse は同じ ID のセッションが登録中のとき返す。
var errIDInUse = errors.New("同じ ID のセッションが登録中")

// add はセッションを登録する。同じ ID が登録中なら拒否し、上限を超えるならエラー。
func (r *Registry) add(s *Session) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.sessions[s.ID]; ok {
		if cur == s {
			return nil
		}
		// 作業ディレクトリ名から作る ID は別リポジトリでも衝突する。黙って上書き
		// すると先のセッションの端末が切れるため、登録中の ID は拒否する。
		return errIDInUse
	}
	if len(r.sessions) >= r.max {
		return errRegistryFull
	}
	r.sessions[s.ID] = s
	return nil
}

// removeIf は id の登録が s 自身のときだけ外す。切断処理が古い接続から呼んでも、
// 同じ ID で後から登録された別のセッションを消さないようにする。
func (r *Registry) removeIf(id string, s *Session) {
	r.mu.Lock()
	if r.sessions[id] == s {
		delete(r.sessions, id)
	}
	r.mu.Unlock()
}

func (r *Registry) remove(id string) {
	r.mu.Lock()
	delete(r.sessions, id)
	r.mu.Unlock()
}

func (r *Registry) get(id string) (*Session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	return s, ok
}

func (r *Registry) list() []*Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		out = append(out, s)
	}
	return out
}

// bySecret は現在のシークレットに一致するセッションを探す (定数時間比較)。
func (r *Registry) bySecret(secret string) *Session {
	if secret == "" {
		return nil
	}
	for _, s := range r.list() {
		s.mu.Lock()
		ok := secureEqual(s.secret, secret)
		s.mu.Unlock()
		if ok {
			return s
		}
	}
	return nil
}

// byCookie は Cookie のトークンに一致するセッションを探す (定数時間比較)。
func (r *Registry) byCookie(token string) *Session {
	if token == "" {
		return nil
	}
	for _, s := range r.list() {
		if s.Authorize(token) {
			return s
		}
	}
	return nil
}

// newSession はメタデータからセッションを作り、ワンタイムのシークレットを発行する。
// ThemeFile は登録側 (handleRegister) が後から設定する。
func newSession(id, name, repo, agent, tmux, console string) (*Session, error) {
	secret, err := newSecret()
	if err != nil {
		return nil, err
	}
	if name == "" {
		name = id
	}
	return &Session{
		ID: id, Name: name, Repo: repo, Agent: agent, Tmux: tmux, Console: console,
		Registered: time.Now(), secret: secret,
	}, nil
}

// SetTheme はブラウザの配色をテーマファイルへ書く。run が端末の配色を判定できな
// かったときだけ __attach がこれを読み、色問い合わせに直接答える (internal/guest)。
// 値は light / dark に限定し、一時ファイル経由で置き換えて読み手に途中を見せない。
func (s *Session) SetTheme(scheme string) error {
	if s.ThemeFile == "" {
		return nil // 端末の配色を判定できている (--theme 済み) か、hub 経由を使わない
	}
	if scheme != "light" && scheme != "dark" {
		return fmt.Errorf("配色が不正: %q", scheme)
	}
	// 複数のブラウザが同時に書いても衝突しないよう、一時ファイルは毎回別名にする。
	f, err := os.CreateTemp(filepath.Dir(s.ThemeFile), ".theme-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.WriteString(scheme + "\n"); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.ThemeFile); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func newSecret() (string, error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("シークレットを作れない: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func newToken() string {
	// 交換用のトークン。失敗はまず起きないので、起きたら空にして認可を閉じる。
	s, err := newSecret()
	if err != nil {
		return ""
	}
	return s
}

// secureEqual は定数時間で比較する。長さが違えば false (シークレットの長さは固定)。
func secureEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
