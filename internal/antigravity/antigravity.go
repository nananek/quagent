// Package antigravity は host 側の agy (Antigravity CLI) のサブスクリプション
// (OAuth ログイン) を VM 内の agy に使わせるための、短命アクセストークンの発行を担う。
// VM にはrefresh_token・client_secret などの長期秘密は入れず、host が必要になるたび
// 1 時間のアクセストークンを作り直して認証プロキシが付ける。guest が直接 Google に
// 問い合わせる起動直後の 1 回 (ユーザー情報の確認) のため、run 開始時に作ったものを
// 1 つだけ guest のトークンファイルに書く (0600。有効期限は長めに書いて guest 側の
// 更新を起こさない。推論はすべてプロキシ経由で新しいものを使うので問題ない)。
package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Upstream はサブスクリプションの推論・管理 API (agy が CLOUD_CODE_URL 未設定時に
// 使うものと同じ。観測した host の agy の接続先に合わせる)。
const Upstream = "https://daily-cloudcode-pa.googleapis.com"

// OAuth クライアントは agy 本体に埋め込まれている公開クライアントを使う。
// ID と secret はリポジトリに書かず、host の agy バイナリから実行時に抜く
// (抜けなければエラーにする。agy の更新で回転しても追従できる)。
const TokenURL = "https://oauth2.googleapis.com/token"

// ProviderID は認証プロキシ上の provider ID (guest の CLOUD_CODE_URL の向き先)。
const ProviderID = "antigravity"

// Allow は転送してよい操作 (CodeAssist 系の v1internal メソッド。推論の
// streamGenerateContent も管理系の loadCodeAssist なども同じ前置き)。
var Allow = []string{
	"POST /v1internal:*", "POST /v1internal/*",
	"GET /v1internal:*", "GET /v1internal/*",
}

// TokenFile は host の agy の OAuth トークンのパス。
func TokenFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token")
}

// RefreshSource はサブスクリプション用の長期 refresh_token の取り出し方。
// すべて空ならトークンファイル (TokenFile) を読む。config の agy の
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

// fetch は取り出し方に従って素の出力 (JSON または refresh_token そのもの) を返す。
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
		return "", fmt.Errorf("refresh_token の取り出し方の出力が空")
	}
	return out, nil
}

// extractRefreshToken は取り出し方の出力から長期の refresh_token を抜く。
// トークンファイルと同じ JSON (token.refresh_token) ならそれを抜き、
// そうでなければ出力全体を素の refresh_token とみなす。
func extractRefreshToken(out string) (string, error) {
	var t struct {
		Token struct {
			RefreshToken string `json:"refresh_token"`
		} `json:"token"`
	}
	if err := json.Unmarshal([]byte(out), &t); err == nil {
		if t.Token.RefreshToken == "" {
			return "", fmt.Errorf("出力に token.refresh_token が無い (agy にログインし直すか、取り出し方の出力を確かめる)")
		}
		return t.Token.RefreshToken, nil
	}
	return out, nil
}

func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, rest)
	}
	return p
}

// CheckLogin は host の agy にログイン済みかだけを確かめる (通信なし)。
// VM を起動する前に失敗を返せるよう、run 開始直後の検査用。
func CheckLogin(src RefreshSource) error {
	if _, err := exec.LookPath("agy"); err != nil {
		return fmt.Errorf("agy が見つからない: %w", err)
	}
	if _, err := refreshTokenFrom(src); err != nil {
		return err
	}
	return nil
}

// WarmupTimeout はホスト側の agy 起動確認の制限時間。
const WarmupTimeout = 60 * time.Second

// Warmup はホスト側で agy を短時間動かしてサブスク認証が通るか確かめる。
// `agy models` は推論枠を消費せず OAuth 更新＋上流への到達を確認できる。
// コマンド終了でプロセスは必ず終わる (起動しっぱなしにしない)。
// 先にホストで起動しておかないとゲスト側で失敗することがあるため、
// VM 起動前 (および agy への切り替え前) に呼ぶ。
func Warmup(ctx context.Context, src RefreshSource) error {
	if err := CheckLogin(src); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, WarmupTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "agy", "models")
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("agy の起動確認がタイムアウトした (%s)", WarmupTimeout)
	}
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 500 {
			msg = msg[:500] + "..."
		}
		if msg == "" {
			return fmt.Errorf("agy の起動確認に失敗した: %w", err)
		}
		return fmt.Errorf("agy の起動確認に失敗した: %w: %s", err, msg)
	}
	return nil
}

// refreshTokenFrom は長期の refresh_token を読む。取り出し方 (src) が指定されて
// いればそれを使い、無ければトークンファイルを読む (agy がファイルに書く場合)。
func refreshTokenFrom(src RefreshSource) (string, error) {
	if src.Configured() {
		out, err := src.fetch()
		if err != nil {
			return "", err
		}
		return extractRefreshToken(out)
	}
	path := TokenFile()
	if path == "" {
		return "", fmt.Errorf("ホームディレクトリが分からない")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s が読めない (host で agy にログインしていない): %w", path, err)
	}
	var t struct {
		Token struct {
			RefreshToken string `json:"refresh_token"`
		} `json:"token"`
	}
	if err := json.Unmarshal(b, &t); err != nil {
		return "", fmt.Errorf("%s が壊れている: %w", path, err)
	}
	if t.Token.RefreshToken == "" {
		return "", fmt.Errorf("%s に refresh_token が無い (host で agy にログインし直す)", path)
	}
	return t.Token.RefreshToken, nil
}

// Minter は短命アクセストークンを作り直す。覚えたものは期限の少し手前まで使い回す。
type Minter struct {
	mu     sync.Mutex
	token  string
	expiry time.Time
	cred   credentials
	src    RefreshSource
	mint   func() (string, error)
}

// credentials は agy の公開 OAuth クライアントの ID と secret の組み合わせ。
type credentials struct {
	id, secret string
}

// NewMinter は Minter を作る。src が空ならトークンファイルを読む。
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
	tok, err := mintFn()
	if err != nil {
		return "", err
	}
	m.token = tok
	m.expiry = time.Now().Add(55 * time.Minute)
	return m.token, nil
}

// Mint は refresh_token から新しいアクセストークンを 1 つ作る (guest の
// トークンファイルの種に使う。長持ちはしないので推論には Minter を使う)。
func Mint(src RefreshSource) (string, error) { return NewMinter(src).refresh() }

// refresh は Minter の作り直し (覚えたクライアント情報を使い回す)。
func (m *Minter) refresh() (string, error) {
	rt, err := refreshTokenFrom(m.src)
	if err != nil {
		return "", err
	}
	if m.cred.id != "" {
		tok, err := mintWith(m.cred, rt)
		if err == nil {
			return tok, nil
		}
		m.cred = credentials{}
	}
	creds, err := findCredentials()
	if err != nil {
		return "", err
	}
	for _, c := range creds {
		tok, err := mintWith(c, rt)
		if err == nil {
			m.cred = c
			return tok, nil
		}
	}
	return "", fmt.Errorf("agy の OAuth クライアントでトークンを作り直せない")
}

func mintWith(c credentials, rt string) (string, error) {

	return mintWithEndpoint(TokenURL, c, rt)
}

func mintWithEndpoint(tokenURL string, c credentials, rt string) (string, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {rt},
		"client_id":     {c.id},
		"client_secret": {c.secret},
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

// findCredentials は host の agy バイナリから公開 OAuth クライアントの
// ID と secret の候補を抜く (ID × secret の順に試す)。
func findCredentials() ([]credentials, error) {
	bin, err := exec.LookPath("agy")
	if err != nil {
		return nil, fmt.Errorf("agy が見つからない: %w", err)
	}
	b, err := os.ReadFile(bin)
	if err != nil {
		return nil, fmt.Errorf("%s が読めない: %w", bin, err)
	}
	ids := uniq(regexp.MustCompile(`[0-9]+-[A-Za-z0-9-]+\.apps\.googleusercontent\.com`).FindAllString(string(b), -1))
	// secret はバイナリ中で前後に切れ目なく連結していることがあるので、
	// 開始位置で切って 1 つずつ取り出す。
	var secrets []string
	for _, loc := range regexp.MustCompile(`GOCSPX-`).FindAllStringIndex(string(b), -1) {
		rest := string(b)[loc[0]:]
		m := regexp.MustCompile(`^GOCSPX-[A-Za-z0-9_-]+`).FindString(rest)
		for _, part := range regexp.MustCompile(`GOCSPX-`).Split(m, -1) {
			if part != "" {
				secrets = append(secrets, "GOCSPX-"+part)
			}
		}
	}
	secrets = uniq(secrets)
	if len(ids) == 0 || len(secrets) == 0 {
		return nil, fmt.Errorf("%s から OAuth クライアント情報を抜けない (agy の版が変わった?)", bin)
	}
	var creds []credentials
	for _, id := range ids {
		for _, s := range secrets {
			creds = append(creds, credentials{id: id, secret: s})
			if len(creds) >= 4 {
				return creds, nil
			}
		}
	}
	return creds, nil
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// userInfoURL は起動直後に agy が直接見に行くユーザー情報の向き先。
const userInfoURL = "https://www.googleapis.com/oauth2/v2/userinfo"

// UserInfo はアクセストークンでユーザー情報 (email とプロフィール画像の URL) を
// 取る。画像の置き場所 (ホスト) は人によって違うので、一時 egress の宛先に使う。
func UserInfo(accessToken string) (email, picture string, err error) {
	return userInfoWith(userInfoURL, accessToken)
}

func userInfoWith(endpoint, accessToken string) (email, picture string, err error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("ユーザー情報を取れない: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("ユーザー情報を取れない (HTTP %d)", resp.StatusCode)
	}
	var out struct {
		Email   string `json:"email"`
		Picture string `json:"picture"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", fmt.Errorf("ユーザー情報が読めない: %w", err)
	}
	return out.Email, out.Picture, nil
}

// PictureHost はプロフィール画像 URL のホストを返す。画像が無ければ空。
func PictureHost(picture string) string {
	u, err := url.Parse(picture)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}
