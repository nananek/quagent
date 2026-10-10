package dockercache

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Upstream はリモート OCI レジストリと対話するインターフェース。
type Upstream interface {
	Name() string
	GetManifest(ctx context.Context, repository, reference string, headers http.Header) (*http.Response, error)
	GetBlob(ctx context.Context, repository, digest string) (*http.Response, error)
}

// TokenResponse はレジストリトークンエンドポイントの JSON 応答。
type TokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

// tokenEntry はキャッシュされたトークンと有効期限。
type tokenEntry struct {
	token     string
	expiresAt time.Time
}

// BaseUpstream は一般的な OCI レジストリの共通実装。
type BaseUpstream struct {
	name       string
	registryURL string
	tokenURL   string
	service    string
	client     *http.Client

	mu     sync.Mutex
	tokens map[string]tokenEntry // key: repository
}

// NewBaseUpstream は新しい BaseUpstream を作成する。
func NewBaseUpstream(name, registryURL, tokenURL, service string, client *http.Client) *BaseUpstream {
	if client == nil {
		client = &http.Client{
			Timeout: 30 * time.Minute, // 大きなレイヤーのダウンロードを考慮
		}
	}
	return &BaseUpstream{
		name:        name,
		registryURL: strings.TrimSuffix(registryURL, "/"),
		tokenURL:    tokenURL,
		service:     service,
		client:      client,
		tokens:      make(map[string]tokenEntry),
	}
}

func (u *BaseUpstream) Name() string {
	return u.name
}

// getToken は指定リポジトリに対する匿名 Bearer トークンを取得する (キャッシュあり)。
func (u *BaseUpstream) getToken(ctx context.Context, repository string) (string, error) {
	u.mu.Lock()
	if entry, ok := u.tokens[repository]; ok && time.Now().Before(entry.expiresAt) {
		u.mu.Unlock()
		return entry.token, nil
	}
	u.mu.Unlock()

	q := url.Values{}
	q.Set("service", u.service)
	q.Set("scope", fmt.Sprintf("repository:%s:pull", repository))

	tokenReqURL := fmt.Sprintf("%s?%s", u.tokenURL, q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenReqURL, nil)
	if err != nil {
		return "", fmt.Errorf("トークンリクエスト作成失敗: %w", err)
	}

	resp, err := u.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("トークン取得失敗 (%s): %w", u.tokenURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("トークン取得で HTTP %d: %s", resp.StatusCode, string(body))
	}

	var tr TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("トークンレスポンス解析失敗: %w", err)
	}

	token := tr.Token
	if token == "" {
		token = tr.AccessToken
	}
	if token == "" {
		return "", fmt.Errorf("トークンが空です")
	}

	expiresIn := tr.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 300 // 既定で 5 分
	}
	// マージンをとって 30 秒前に失効扱いにする
	expiresAt := time.Now().Add(time.Duration(expiresIn)*time.Second - 30*time.Second)

	u.mu.Lock()
	u.tokens[repository] = tokenEntry{token: token, expiresAt: expiresAt}
	u.mu.Unlock()

	return token, nil
}

// GetManifest は上流からマニフェストを取得する。
func (u *BaseUpstream) GetManifest(ctx context.Context, repository, reference string, headers http.Header) (*http.Response, error) {
	token, err := u.getToken(ctx, repository)
	if err != nil {
		return nil, err
	}

	reqURL := fmt.Sprintf("%s/v2/%s/manifests/%s", u.registryURL, repository, reference)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+token)
	// クライアントが要求した Accept ヘッダー (OCI / Docker v2 manifest 等) を引き継ぐ
	if accepts := headers.Values("Accept"); len(accepts) > 0 {
		for _, a := range accepts {
			req.Header.Add("Accept", a)
		}
	} else {
		req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.index.v1+json")
	}

	return u.client.Do(req)
}

// GetBlob は上流からレイヤー blob を取得する。
func (u *BaseUpstream) GetBlob(ctx context.Context, repository, digest string) (*http.Response, error) {
	token, err := u.getToken(ctx, repository)
	if err != nil {
		return nil, err
	}

	reqURL := fmt.Sprintf("%s/v2/%s/blobs/%s", u.registryURL, repository, digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+token)
	return u.client.Do(req)
}

// DockerHubUpstream は Docker Hub (docker.io) 向けの実装。
type DockerHubUpstream struct {
	*BaseUpstream
}

// NewDockerHubUpstream は Docker Hub 向け Upstream を作成する。
func NewDockerHubUpstream(client *http.Client) *DockerHubUpstream {
	base := NewBaseUpstream(
		"docker.io",
		"https://registry-1.docker.io",
		"https://auth.docker.io/token",
		"registry.docker.io",
		client,
	)
	return &DockerHubUpstream{BaseUpstream: base}
}

// normalizeRepo は Docker Hub 特有の公式イメージ補完を行う (例: alpine -> library/alpine)。
func normalizeDockerHubRepo(repo string) string {
	if !strings.Contains(repo, "/") {
		return "library/" + repo
	}
	return repo
}

func (d *DockerHubUpstream) GetManifest(ctx context.Context, repository, reference string, headers http.Header) (*http.Response, error) {
	return d.BaseUpstream.GetManifest(ctx, normalizeDockerHubRepo(repository), reference, headers)
}

func (d *DockerHubUpstream) GetBlob(ctx context.Context, repository, digest string) (*http.Response, error) {
	return d.BaseUpstream.GetBlob(ctx, normalizeDockerHubRepo(repository), digest)
}

// GHCRUpstream は GitHub Container Registry (ghcr.io) 向けの実装。
type GHCRUpstream struct {
	*BaseUpstream
}

// NewGHCRUpstream は GHCR 向け Upstream を作成する。
func NewGHCRUpstream(client *http.Client) *GHCRUpstream {
	base := NewBaseUpstream(
		"ghcr.io",
		"https://ghcr.io",
		"https://ghcr.io/token",
		"ghcr.io",
		client,
	)
	return &GHCRUpstream{BaseUpstream: base}
}
