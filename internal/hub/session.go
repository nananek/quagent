package hub

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// secretBytes はワンタイム URL のシークレットと Cookie のトークンの長さ。
const secretBytes = 32

// Session は登録された 1 つの run。
type Session struct {
	ID         string
	Name       string
	Repo       string
	Agent      string
	Tmux       string // tmux セッション名 (端末の中継に使う)
	Console    string // console.sock のパス (承認の中継に使う)
	Registered time.Time

	mu        sync.Mutex
	secret    string // ワンタイム URL のシークレット (交換で消費される)
	exchanged bool
	cookie    string // 交換後に発行する Cookie のトークン
}

// URL は base を基点としたアクセス URL を返す。
func (s *Session) URL(base string) string {
	return strings.TrimRight(base, "/") + "/s/" + s.secret
}

// LinkURL は通知に載せる URL を返す。未交換ならワンタイム URL、交換済みなら基点
// (既に Cookie を持っているのでシークレットは不要)。
func (s *Session) LinkURL(base string) string {
	s.mu.Lock()
	exchanged := s.exchanged
	s.mu.Unlock()
	if exchanged {
		return strings.TrimRight(base, "/") + "/"
	}
	return s.URL(base)
}

// Exchange はシークレットを Cookie のトークンに交換する。未交換で一致したときだけ
// 成功し、以降その URL のシークレットは無効になる (ワンタイム)。
func (s *Session) Exchange(secret string) (cookie string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exchanged || !secureEqual(s.secret, secret) {
		return "", false
	}
	s.exchanged = true
	s.cookie = newToken()
	return s.cookie, true
}

// Authorize は Cookie のトークンがこのセッションに有効かを返す。
func (s *Session) Authorize(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exchanged && token != "" && secureEqual(s.cookie, token)
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

// add はセッションを登録する。同じ ID は上書きし、上限を超えるならエラー。
func (r *Registry) add(s *Session) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sessions[s.ID]; !ok && len(r.sessions) >= r.max {
		return errRegistryFull
	}
	r.sessions[s.ID] = s
	return nil
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

// bySecret はシークレットに一致するセッションを探す (定数時間比較)。
func (r *Registry) bySecret(secret string) *Session {
	for _, s := range r.list() {
		if secureEqual(s.secret, secret) {
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
		s.mu.Lock()
		c := s.cookie
		ex := s.exchanged
		s.mu.Unlock()
		if ex && secureEqual(c, token) {
			return s
		}
	}
	return nil
}

// newSession はメタデータからセッションを作り、ワンタイムのシークレットを発行する。
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
