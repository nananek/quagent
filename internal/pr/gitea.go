package pr

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

// giteaPageSize は PR 一覧を 1 ページに取る件数。
const giteaPageSize = 50

// GiteaConfig は GiteaForge の設定。
type GiteaConfig struct {
	// Dir は host の repo (origin から owner/repo を読む)。
	Dir string
	// URL は Gitea の base URL (例 https://git.example.com、サブパス設置なら
	// https://example.com/gitea)。
	URL string
	// Owner / Repo は省略可。空なら origin から解釈する。
	Owner string
	Repo  string
	// Token は必須。ホスト側でのみ実行する。
	Token func() (string, error)
	// Client はテスト用。nil ならタイムアウト 30 秒の既定を使う。
	Client *http.Client
}

// GiteaForge は Gitea / Forgejo の REST API で PR を扱う。
type GiteaForge struct {
	dir    string
	apiURL string
	owner  string
	repo   string
	token  func() (string, error)
	client *http.Client
}

// NewGiteaForge は設定を検証して GiteaForge を作る。
func NewGiteaForge(c GiteaConfig) (*GiteaForge, error) {
	if c.Token == nil {
		return nil, fmt.Errorf("Gitea のトークンの取り出し方が無い")
	}
	if strings.TrimSpace(c.URL) == "" {
		return nil, fmt.Errorf("pr.gitea.url が必要")
	}
	u, err := url.Parse(c.URL)
	if err != nil {
		return nil, fmt.Errorf("pr.gitea.url が不正: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("pr.gitea.url は http か https にする: %q", c.URL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("pr.gitea.url にホストが無い: %q", c.URL)
	}
	if (c.Owner == "") != (c.Repo == "") {
		return nil, fmt.Errorf("pr.gitea.owner と pr.gitea.repo は両方指定するか両方省く")
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &GiteaForge{
		dir:    c.Dir,
		apiURL: strings.TrimRight(c.URL, "/") + "/api/v1",
		owner:  c.Owner,
		repo:   c.Repo,
		token:  c.Token,
		client: client,
	}, nil
}

// repoPath は API のパスに使う owner と repo を返す。設定に無ければ origin から読む。
func (f *GiteaForge) repoPath() (owner, repo string, err error) {
	if f.owner != "" && f.repo != "" {
		return f.owner, f.repo, nil
	}
	cmd := exec.Command("git", "-C", f.dir, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("origin の URL を読めない (pr.gitea.owner / repo を指定する): %w", err)
	}
	owner, repo, err = parseRemote(strings.TrimSpace(string(out)))
	if err != nil {
		return "", "", fmt.Errorf("origin から owner/repo を解釈できない (pr.gitea.owner / repo を指定する): %w", err)
	}
	return owner, repo, nil
}

// parseRemote はリモートの URL から owner と repo を取り出す。
func parseRemote(raw string) (owner, repo string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", fmt.Errorf("リモートの URL が空")
	}
	if strings.HasPrefix(strings.ToLower(raw), "file://") {
		return "", "", fmt.Errorf("file:// のリモートは使えない: %q", raw)
	}
	var path string
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", "", fmt.Errorf("リモートの URL を解釈できない: %w", err)
		}
		switch strings.ToLower(u.Scheme) {
		case "http", "https", "ssh", "git":
		default:
			return "", "", fmt.Errorf("対応していないリモートの形式: %q", raw)
		}
		if u.Host == "" {
			return "", "", fmt.Errorf("リモートにホストが無い: %q", raw)
		}
		path = u.Path
	} else {
		// scp 形式 (git@host:owner/repo.git)。最初の ":" を区切りにする。
		colon := strings.Index(raw, ":")
		slash := strings.Index(raw, "/")
		if colon < 0 || (slash >= 0 && slash < colon) {
			return "", "", fmt.Errorf("対応していないリモートの形式: %q", raw)
		}
		path = raw[colon+1:]
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("リモートのパスに owner/repo が無い: %q", raw)
	}
	owner = parts[len(parts)-2]
	repo = strings.TrimSuffix(parts[len(parts)-1], ".git")
	if !validRepoPart(owner) || !validRepoPart(repo) {
		return "", "", fmt.Errorf("リモートの owner/repo が不正: %q", raw)
	}
	return owner, repo, nil
}

func validRepoPart(s string) bool {
	return s != "" && s != "." && s != ".."
}

type giteaPR struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	State   string `json:"state"`
	Head    struct {
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
}

// do は Gitea の API を 1 回呼ぶ。2xx 以外は status と本文を含むエラーにする。
func (f *GiteaForge) do(method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, f.apiURL+"/"+strings.TrimPrefix(path, "/"), rd)
	if err != nil {
		return 0, nil, err
	}
	token, err := f.token()
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, data,
			fmt.Errorf("Gitea API %s %s: %d: %s", method, path, resp.StatusCode, bodySnippet(data))
	}
	return resp.StatusCode, data, nil
}

// bodySnippet はエラーに載せる本文の先頭 (2 KiB 程度) を返す。
func bodySnippet(b []byte) string {
	const max = 2048
	if len(b) > max {
		b = b[:max]
	}
	return strings.TrimSpace(string(b))
}

// OpenPR は branch に開いている PR を返す。無ければ ok=false。
func (f *GiteaForge) OpenPR(branch string) (PullRequest, bool, error) {
	owner, repo, err := f.repoPath()
	if err != nil {
		return PullRequest{}, false, err
	}
	full := owner + "/" + repo
	base := "repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/pulls"
	for page := 1; page <= 10; page++ {
		path := fmt.Sprintf("%s?state=open&limit=%d&page=%d", base, giteaPageSize, page)
		_, data, err := f.do(http.MethodGet, path, nil)
		if err != nil {
			return PullRequest{}, false, err
		}
		var list []giteaPR
		if err := json.Unmarshal(data, &list); err != nil {
			return PullRequest{}, false, fmt.Errorf("Gitea API の PR 一覧を読めない: %w", err)
		}
		for _, it := range list {
			// ブランチ名が同じでも、fork 元が違う PR は拾わない。
			if it.State != "open" || it.Head.Ref != branch {
				continue
			}
			if !strings.EqualFold(it.Head.Repo.FullName, full) {
				continue
			}
			if it.HTMLURL == "" {
				continue
			}
			return PullRequest{URL: it.HTMLURL, Number: it.Number, Title: it.Title, Body: it.Body}, true, nil
		}
		if len(list) < giteaPageSize {
			break
		}
	}
	return PullRequest{}, false, nil
}

// CreatePR は PR を作成して返す。
func (f *GiteaForge) CreatePR(branch, base, title, body string) (PullRequest, error) {
	owner, repo, err := f.repoPath()
	if err != nil {
		return PullRequest{}, err
	}
	path := "repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/pulls"
	_, data, err := f.do(http.MethodPost, path, map[string]string{
		"head": branch, "base": base, "title": title, "body": body,
	})
	if err != nil {
		return PullRequest{}, err
	}
	var created giteaPR
	if err := json.Unmarshal(data, &created); err != nil {
		return PullRequest{}, fmt.Errorf("Gitea API の PR を読めない: %w", err)
	}
	if created.HTMLURL == "" {
		return PullRequest{}, fmt.Errorf("Gitea API が PR の URL を返さない")
	}
	return PullRequest{URL: created.HTMLURL, Number: created.Number, Title: title, Body: body}, nil
}

// EditPR は既存 PR のタイトル・本文を更新する。
func (f *GiteaForge) EditPR(pr PullRequest, title, body string) error {
	if pr.Number <= 0 {
		return fmt.Errorf("PR の番号が無い")
	}
	owner, repo, err := f.repoPath()
	if err != nil {
		return err
	}
	path := fmt.Sprintf("repos/%s/%s/pulls/%d",
		url.PathEscape(owner), url.PathEscape(repo), pr.Number)
	_, _, err = f.do(http.MethodPatch, path, map[string]string{"title": title, "body": body})
	return err
}
