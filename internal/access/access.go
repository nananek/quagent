// Package access はエージェントからの接続先申請と、人間による承認を管理する。
//
// 申請は「理由 + ドメイン群」をひとまとめに扱い、承認も一括で行う。承認の種類:
//   - Once:    今回は許可。OnceTTL 後に新規接続を止める (確立済みの接続は切らない)
//   - Session: この VM のあいだ確認しない。エージェントが放棄しても、再申請は自動で通る
//   - Always:  以後確認しない (全プロジェクト共通、ファイルに保存)
//
// 申請中のドメインが全部 Session/Always 済みなら確認せずに通す。
// DecisionTimeout 内に応答がなければ時間切れとして拒否する。
package access

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nananek/quagent/internal/netns"
	"github.com/nananek/quagent/internal/paths"
)

const (
	OnceTTL         = 5 * time.Minute
	DecisionTimeout = 10 * time.Minute
)

// Kind は承認の種類。
type Kind string

const (
	Once    Kind = "once"
	Session Kind = "session"
	Always  Kind = "always"
)

// Status は申請の結果。
type Status string

const (
	Approved Status = "approved"
	Denied   Status = "denied"
	Question Status = "question" // 承認者からの問い返し。答えを理由に含めて再申請してもらう
	TimedOut Status = "timeout"
	Pending  Status = "pending" // まだ決まっていない (Wait で待ち続ける)
)

// Result は申請に対する応答。
type Result struct {
	RequestID int      `json:"request_id"`
	Status    Status   `json:"status"`
	Kind      Kind     `json:"kind,omitempty"`
	Domains   []string `json:"domains"`
	// ExpiresAt は Once の許可が新規接続を止める時刻 (RFC3339)。
	ExpiresAt string `json:"expires_at,omitempty"`
	// Question は Status が question のときの承認者の問い。
	Question string `json:"question,omitempty"`
	// Auto は確認なしで通ったとき true。
	Auto bool `json:"auto,omitempty"`
}

// Request は承認待ちの申請。
type Request struct {
	ID      int
	Domains []string
	Reason  string
	Created time.Time

	done    chan struct{}
	result  Result
	claimed bool // 決着させる権利を誰かが取った (m.mu で保護)
}

// Decision は承認者の判断。
type Decision struct {
	Status   Status // Approved / Denied / Question
	Kind     Kind   // Approved のとき
	Question string // Question のとき
}

// Applier は許可の一覧を egress に反映する (netns.Launcher)。
type Applier interface {
	SetGrants([]netns.Grant) error
}

// Manager は申請・許可の状態を持つ。
type Manager struct {
	apply      Applier
	alwaysPath string
	now        func() time.Time

	mu      sync.Mutex
	nextID  int
	grants  map[string]int64 // パターン -> 期限 (unix 秒、0 は期限なし)
	session map[string]bool  // このセッションでは確認しない
	always  map[string]bool  // 以後確認しない
	pending []*Request
	byID    map[int]*Request

	// Notify には新しい申請が来るたびに通知が入る (UI 用)。
	Notify chan struct{}
	// Log は確認なしで通した申請や放棄などを UI に伝える (nil なら捨てる)。
	Log func(string)
}

func (m *Manager) logf(format string, a ...any) {
	if m.Log != nil {
		m.Log(fmt.Sprintf(format, a...))
	}
}

// Settled は決着済みの申請の結果を返す。
func (m *Manager) Settled(id int) (Result, bool) {
	m.mu.Lock()
	r, ok := m.byID[id]
	m.mu.Unlock()
	if !ok {
		return Result{}, false
	}
	select {
	case <-r.done:
		return r.result, true
	default:
		return Result{}, false
	}
}

// NewManager は Manager を作る。alwaysPath は「以後確認しない」の保存先。
func NewManager(apply Applier, alwaysPath string) (*Manager, error) {
	m := &Manager{
		apply: apply, alwaysPath: alwaysPath, now: time.Now,
		grants: map[string]int64{}, session: map[string]bool{}, always: map[string]bool{},
		byID: map[int]*Request{}, Notify: make(chan struct{}, 1),
	}
	b, err := os.ReadFile(alwaysPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		var list []string
		if err := json.Unmarshal(b, &list); err != nil {
			return nil, fmt.Errorf("%s: %w", alwaysPath, err)
		}
		for _, d := range list {
			m.always[d] = true
		}
	}
	return m, nil
}

var domainRe = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,62}$`)

// NormalizeDomains は申請されたドメインを検証・正規化する。
func NormalizeDomains(in []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, d := range in {
		d = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d), "."))
		if !domainRe.MatchString(d) {
			return nil, fmt.Errorf("ドメインとして不正: %q (例: pypi.org, *.githubusercontent.com)", d)
		}
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("ドメインが空")
	}
	sort.Strings(out)
	return out, nil
}

// Preallow は最初から許可しておくドメインを登録する (このセッションでは確認しない扱い)。
func (m *Manager) Preallow(domains []string) error {
	if len(domains) == 0 {
		return nil
	}
	domains, err := NormalizeDomains(domains)
	if err != nil {
		return err
	}
	m.mu.Lock()
	for _, d := range domains {
		m.session[d] = true
	}
	m.mu.Unlock()
	return m.grant(domains, 0)
}

// Submit は申請を受け付ける。確認不要なら即座に許可し、そうでなければ承認待ちに積む。
func (m *Manager) Submit(domains []string, reason string) (*Request, error) {
	domains, err := NormalizeDomains(domains)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("理由 (reason) が必要")
	}
	m.mu.Lock()
	m.nextID++
	r := &Request{ID: m.nextID, Domains: domains, Reason: reason, Created: m.now(), done: make(chan struct{})}
	m.byID[r.ID] = r
	trusted := true
	for _, d := range domains {
		if !m.session[d] && !m.always[d] {
			trusted = false
		}
	}
	if trusted {
		r.claimed = true
		m.mu.Unlock()
		if err := m.grant(domains, 0); err != nil {
			m.finish(r, Result{Status: Denied})
			return nil, err
		}
		m.finish(r, Result{Status: Approved, Kind: Session, Auto: true})
		m.logf("確認済みのため自動で許可: %s (%s)", strings.Join(domains, ", "), reason)
		return r, nil
	}
	m.pending = append(m.pending, r)
	m.mu.Unlock()
	select {
	case m.Notify <- struct{}{}:
	default:
	}
	return r, nil
}

// Wait は申請の結果を最大 d だけ待つ。決まらなければ Pending を返す。
// 申請から DecisionTimeout を過ぎたら時間切れとして拒否する。
func (m *Manager) Wait(ctx context.Context, id int, d time.Duration) (Result, error) {
	m.mu.Lock()
	r, ok := m.byID[id]
	m.mu.Unlock()
	if !ok {
		return Result{}, fmt.Errorf("申請 %d は無い", id)
	}
	deadline := r.Created.Add(DecisionTimeout)
	wait := min(d, time.Until(deadline))
	if wait < 0 {
		wait = 0
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-r.done:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case <-t.C:
		if m.now().Before(deadline) {
			return Result{RequestID: id, Status: Pending, Domains: r.Domains}, nil
		}
		if m.claim(r) {
			m.finish(r, Result{Status: TimedOut})
		}
		<-r.done
	}
	return r.result, nil
}

// Pending は承認待ちの申請を古い順に返す。
func (m *Manager) Pending() []*Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*Request(nil), m.pending...)
}

// Decide は承認者の判断を申請に適用する。
func (m *Manager) Decide(id int, d Decision) error {
	m.mu.Lock()
	r, ok := m.byID[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("申請 %d は無い", id)
	}
	switch d.Status {
	case Approved:
		if d.Kind != Once && d.Kind != Session && d.Kind != Always {
			return fmt.Errorf("不明な承認の種類: %q", d.Kind)
		}
	case Denied, Question:
	default:
		return fmt.Errorf("不明な判断: %q", d.Status)
	}
	// 時間切れなどで決着済み (または決着中) の申請には何も反映しない
	if !m.claim(r) {
		return fmt.Errorf("申請 %d は既に決着している", id)
	}
	switch d.Status {
	case Approved:
		res, err := m.approve(r, d.Kind)
		if err != nil {
			// 反映に失敗したら拒否として決着させ、エージェントを待たせ続けない
			m.finish(r, Result{Status: Denied})
			return err
		}
		m.finish(r, res)
	case Denied:
		m.finish(r, Result{Status: Denied})
	case Question:
		m.finish(r, Result{Status: Question, Question: d.Question})
	}
	return nil
}

func (m *Manager) approve(r *Request, kind Kind) (Result, error) {
	var expires int64
	switch kind {
	case Once:
		expires = m.now().Add(OnceTTL).Unix()
	case Session:
		m.mu.Lock()
		for _, dom := range r.Domains {
			m.session[dom] = true
		}
		m.mu.Unlock()
	case Always:
		if err := m.addAlways(r.Domains); err != nil {
			return Result{}, err
		}
	}
	if err := m.grant(r.Domains, expires); err != nil {
		return Result{}, err
	}
	res := Result{Status: Approved, Kind: kind}
	if expires != 0 {
		res.ExpiresAt = time.Unix(expires, 0).Format(time.RFC3339)
	}
	return res, nil
}

// Release はエージェントが用済みのドメインの許可を放棄する。新規接続だけを止める。
func (m *Manager) Release(domains []string) ([]string, error) {
	domains, err := NormalizeDomains(domains)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	var released []string
	for _, d := range domains {
		if _, ok := m.grants[d]; ok {
			delete(m.grants, d)
			released = append(released, d)
		}
	}
	m.mu.Unlock()
	if len(released) > 0 {
		m.logf("エージェントが放棄: %s", strings.Join(released, ", "))
	}
	return released, m.sync()
}

// Grants は有効な許可を返す (パターン -> 期限。0 は期限なし)。
func (m *Manager) Grants() map[string]int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().Unix()
	out := map[string]int64{}
	for d, exp := range m.grants {
		if exp == 0 || now < exp {
			out[d] = exp
		}
	}
	return out
}

func (m *Manager) grant(domains []string, expires int64) error {
	m.mu.Lock()
	for _, d := range domains {
		cur, ok := m.grants[d]
		// 期限なし、またはより長い許可を短い許可で上書きしない
		if ok && (cur == 0 || (expires != 0 && cur >= expires)) {
			continue
		}
		m.grants[d] = expires
	}
	m.mu.Unlock()
	return m.sync()
}

func (m *Manager) sync() error {
	m.mu.Lock()
	now := m.now().Unix()
	gs := make([]netns.Grant, 0, len(m.grants))
	for d, exp := range m.grants {
		if exp != 0 && now >= exp {
			delete(m.grants, d)
			continue
		}
		gs = append(gs, netns.Grant{Pattern: d, Expires: exp})
	}
	m.mu.Unlock()
	sort.Slice(gs, func(i, j int) bool { return gs[i].Pattern < gs[j].Pattern })
	return m.apply.SetGrants(gs)
}

func (m *Manager) addAlways(domains []string) error {
	m.mu.Lock()
	for _, d := range domains {
		m.always[d] = true
	}
	list := make([]string, 0, len(m.always))
	for d := range m.always {
		list = append(list, d)
	}
	m.mu.Unlock()
	sort.Strings(list)
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.alwaysPath), 0o755); err != nil {
		return err
	}
	tmp := m.alwaysPath + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, m.alwaysPath)
}

// claim は申請を決着させる権利を取る。既に誰かが取っていれば false。
func (m *Manager) claim(r *Request) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.claimed {
		return false
	}
	r.claimed = true
	return true
}

// finish は申請を決着させる。呼ぶ前に claim で権利を取っておくこと。
func (m *Manager) finish(r *Request, res Result) {
	m.mu.Lock()
	defer m.mu.Unlock()
	select {
	case <-r.done:
		return // 既に決着済み
	default:
	}
	res.RequestID = r.ID
	res.Domains = r.Domains
	r.result = res
	close(r.done)
	for i, p := range m.pending {
		if p == r {
			m.pending = append(m.pending[:i], m.pending[i+1:]...)
			break
		}
	}
}

// AlwaysPath は「以後確認しない」の保存先。
func AlwaysPath() string { return filepath.Join(paths.DataDir(), "always-allow.json") }

// LoadAlways は「以後確認しない」ドメインの一覧を読む。
func LoadAlways(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var list []string
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	sort.Strings(list)
	return list, nil
}

// RemoveAlways は「以後確認しない」からドメインを外す。動いている run には効かない。
func RemoveAlways(path string, domains []string) ([]string, error) {
	list, err := LoadAlways(path)
	if err != nil {
		return nil, err
	}
	var kept, removed []string
	for _, d := range list {
		if slices.Contains(domains, d) {
			removed = append(removed, d)
		} else {
			kept = append(kept, d)
		}
	}
	if len(removed) == 0 {
		return nil, nil
	}
	b, err := json.MarshalIndent(nonNilStrings(kept), "", "  ")
	if err != nil {
		return nil, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return nil, err
	}
	return removed, os.Rename(tmp, path)
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
