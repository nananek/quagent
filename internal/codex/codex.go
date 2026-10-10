// Package codex は host 側の codex (OpenAI Codex CLI) のサブスクリプション
// (ChatGPT Plus / Pro 等の OAuth ログイン) を VM 内の codex に使わせるための、
// 短命アクセストークンの発行・中継を担う。
// VM には長期 refresh_token などの秘密は入れず、host が認証プロキシ経由で付け替える。
package codex

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Upstream は ChatGPT バックエンドの向き先。
const Upstream = "https://chatgpt.com"

// TokenURL は OpenAI OAuth のトークン再発行エンドポイント。
const TokenURL = "https://auth.openai.com/api/accounts/oauth/token"

// ProviderID は認証プロキシ上の provider ID (guest の CHATGPT_BASE_URL などの向き先)。
const ProviderID = "codex"

// Allow は転送してよい操作 (backend-api や responses, models など)。
var Allow = []string{
	"POST /backend-api/*", "GET /backend-api/*",
	"POST /v1/*", "GET /v1/*",
	"POST /responses", "POST /responses/*",
	"GET /models", "GET /models/*",
}

// EgressHosts はサブスクリプション起動時に一時的に開ける宛先。
var EgressHosts = []string{"chatgpt.com", "auth.openai.com"}

// TokenFile は host の codex の OAuth トークンファイル (auth.json) のパス。
func TokenFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "auth.json")
}

// RefreshSource はサブスクリプション用の長期 refresh_token / access_token の取り出し方。
// すべて空ならトークンファイル (TokenFile) を読む。config の codex の
// refresh_token_env / refresh_token_file / refresh_token_command に対応する。
type RefreshSource struct {
	Env     string
	File    string
	Command []string
}

// Configured は取り出し方が指定されているか (ファイル読みではないか) を返す。
func (s RefreshSource) Configured() bool {
	return s.Env != "" || s.File != "" || len(s.Command) > 0
}

// fetch は取り出し方に従って素の出力 (JSON またはトークン文字列) を返す。
// 秘密は host のプロセス内だけで使い、guest には渡さない。
func (s RefreshSource) fetch() (string, error) {
	var raw []byte
	var err error
	switch {
	case s.Env != "":
		v, ok := os.LookupEnv(s.Env)
		if !ok {
			return "", fmt.Errorf("環境変数 %s が未設定", s.Env)
		}
		raw = []byte(v)
	case s.File != "":
		raw, err = os.ReadFile(expandHome(s.File))
	case len(s.Command) > 0:
		cmd := exec.Command(s.Command[0], s.Command[1:]...)
		cmd.Stderr = os.Stderr
		raw, err = cmd.Output()
	default:
		return "", fmt.Errorf("refresh_token_env / refresh_token_file / refresh_token_command のどれかが必要")
	}
	if err != nil {
		return "", err
	}
	out := strings.TrimSpace(string(raw))
	if out == "" {
		return "", fmt.Errorf("トークンの取り出し方の出力が空")
	}
	return out, nil
}

// AuthInfo は host から取得した認証トークン情報。
type AuthInfo struct {
	AccessToken  string
	RefreshToken string
	AccountID    string
	ClientID     string
}

type codexTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	AccountID    string `json:"account_id"`
	ClientID     string `json:"client_id"`
}

type codexAuthFile struct {
	AuthMode     string      `json:"auth_mode"`
	Tokens       codexTokens `json:"tokens"`
	AccessToken  string      `json:"access_token"`
	RefreshToken string      `json:"refresh_token"`
	AccountID    string      `json:"account_id"`
}

// extractAuth は取り出し方の出力からトークン情報を抜く。
func extractAuth(out string) (AuthInfo, error) {
	var t codexAuthFile
	if err := json.Unmarshal([]byte(out), &t); err == nil {
		acc := t.Tokens.AccessToken
		if acc == "" {
			acc = t.AccessToken
		}
		ref := t.Tokens.RefreshToken
		if ref == "" {
			ref = t.RefreshToken
		}
		accID := t.Tokens.AccountID
		if accID == "" {
			accID = t.AccountID
		}
		if acc == "" && ref == "" {
			return AuthInfo{}, fmt.Errorf("出力に access_token または refresh_token が無い")
		}
		return AuthInfo{
			AccessToken:  acc,
			RefreshToken: ref,
			AccountID:    accID,
			ClientID:     t.Tokens.ClientID,
		}, nil
	}
	// JSON でなければ素のトークン文字列
	return AuthInfo{AccessToken: out}, nil
}

func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, rest)
	}
	return p
}

// readAuthFrom は認証情報を読む。src が設定されていればそれを使い、無ければ TokenFile を読む。
func readAuthFrom(src RefreshSource) (AuthInfo, error) {
	if src.Configured() {
		out, err := src.fetch()
		if err != nil {
			return AuthInfo{}, err
		}
		return extractAuth(out)
	}
	path := TokenFile()
	if path == "" {
		return AuthInfo{}, fmt.Errorf("ホームディレクトリが分からない")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return AuthInfo{}, fmt.Errorf("%s が読めない (host で codex にログインしていない): %w", path, err)
	}
	return extractAuth(string(b))
}

// CheckLogin は host の codex にログイン済みか確かめる (通信なし)。
func CheckLogin(src RefreshSource) error {
	if _, err := exec.LookPath("codex"); err != nil {
		return fmt.Errorf("codex が見つからない: %w", err)
	}
	if _, err := readAuthFrom(src); err != nil {
		return err
	}
	return nil
}

// WarmupTimeout はホスト側の codex 起動確認の制限時間。
const WarmupTimeout = 60 * time.Second

// Warmup はホスト側で codex を短時間動かして確認する。
func Warmup(ctx context.Context, src RefreshSource) error {
	if err := CheckLogin(src); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, WarmupTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "codex", "--version")
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("codex の起動確認がタイムアウトした (%s)", WarmupTimeout)
	}
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 500 {
			msg = msg[:500] + "..."
		}
		if msg == "" {
			return fmt.Errorf("codex の起動確認に失敗した: %w", err)
		}
		return fmt.Errorf("codex の起動確認に失敗した: %w: %s", err, msg)
	}
	return nil
}

// ParseJWTExpiry は JWT トークンから exp クレーム (有効期限) を抽出する。
// 不正なトークンや exp が無い場合はゼロ値を返す。
func ParseJWTExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}
	}
	payload := parts[1]
	// base64url のパディング補正
	if pad := len(payload) % 4; pad != 0 {
		payload += strings.Repeat("=", 4-pad)
	}
	b, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(b, &claims); err != nil || claims.Exp <= 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}

// Minter は短命アクセストークンの発行・管理を行う。
type Minter struct {
	mu        sync.Mutex
	token     string
	accountID string
	expiry    time.Time
	src       RefreshSource
	mint      func() (string, string, error)
}

// NewMinter は Minter を作る。
func NewMinter(src RefreshSource) *Minter {
	m := &Minter{src: src}
	m.mint = m.refresh
	return m
}

// Token は使えるアクセストークンを返す。残り 5 分を切っていたら作り直す。
func (m *Minter) Token() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.token != "" && time.Now().Add(5*time.Minute).Before(m.expiry) {
		return m.token, nil
	}
	mintFn := m.mint
	if mintFn == nil {
		mintFn = NewMinter(m.src).refresh
	}
	tok, accID, err := mintFn()
	if err != nil {
		return "", err
	}
	m.token = tok
	m.accountID = accID
	// JWT から期限を推測。取れなければ 30 分後
	if exp := ParseJWTExpiry(tok); !exp.IsZero() {
		m.expiry = exp
	} else {
		m.expiry = time.Now().Add(30 * time.Minute)
	}
	return m.token, nil
}

// AccountID は ChatGPT アカウント ID を返す。
func (m *Minter) AccountID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.accountID
}

// refresh は認証情報の読み出しとトークンのリフレッシュ。
func (m *Minter) refresh() (string, string, error) {
	info, err := readAuthFrom(m.src)
	if err != nil {
		return "", "", err
	}
	// access_token があり、期限が切れていなければそのまま使用
	if info.AccessToken != "" {
		if exp := ParseJWTExpiry(info.AccessToken); exp.IsZero() || time.Now().Add(5*time.Minute).Before(exp) {
			return info.AccessToken, info.AccountID, nil
		}
	}
	// 期限切れ、または access_token が無ければ refresh_token で再発行を試みる
	if info.RefreshToken != "" {
		clientID := info.ClientID
		if clientID == "" {
			clientID = "codex"
		}
		tok, err := mintWithEndpoint(TokenURL, clientID, info.RefreshToken)
		if err == nil && tok != "" {
			return tok, info.AccountID, nil
		}
		// リフレッシュに失敗しても、access_token があればそれをフォールバックで使う
		if info.AccessToken != "" {
			return info.AccessToken, info.AccountID, nil
		}
		return "", "", fmt.Errorf("codex のトークンを再発行できない: %w", err)
	}
	if info.AccessToken != "" {
		return info.AccessToken, info.AccountID, nil
	}
	return "", "", fmt.Errorf("codex の有効な認証トークンが無い")
}

func mintWithEndpoint(tokenURL, clientID, rt string) (string, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {rt},
		"client_id":     {clientID},
	}
	req, err := http.NewRequest(http.MethodPost, tokenURL, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("トークンを作り直せない: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("トークンの応答が読めない: %w", err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("トークンを作り直せない: %s %s", out.Error, out.ErrorDesc)
	}
	return out.AccessToken, nil
}
