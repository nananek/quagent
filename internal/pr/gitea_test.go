package pr

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseRemote(t *testing.T) {
	cases := []struct {
		in      string
		owner   string
		repo    string
		wantErr bool
	}{
		{in: "https://git.example.com/org/repo.git", owner: "org", repo: "repo"},
		{in: "http://git.example.com:3000/org/repo", owner: "org", repo: "repo"},
		{in: "ssh://git@git.example.com:2222/org/repo.git", owner: "org", repo: "repo"},
		{in: "git@git.example.com:org/repo.git", owner: "org", repo: "repo"},
		{in: "https://example.com/gitea/org/repo.git", owner: "org", repo: "repo"},
		{in: "https://git.example.com/org/repo.git/", owner: "org", repo: "repo"},
		{in: "file:///srv/git/org/repo.git", wantErr: true},
		{in: "/home/user/repo", wantErr: true},
		{in: "org/repo", wantErr: true},
		{in: "git@git.example.com:repo.git", wantErr: true},
		{in: "https://git.example.com/org/.git", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, c := range cases {
		owner, repo, err := parseRemote(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseRemote(%q) = %q, %q; want error", c.in, owner, repo)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseRemote(%q): %v", c.in, err)
			continue
		}
		if owner != c.owner || repo != c.repo {
			t.Errorf("parseRemote(%q) = %q, %q; want %q, %q", c.in, owner, repo, c.owner, c.repo)
		}
	}
}

func TestNewGiteaForgeValidation(t *testing.T) {
	token := func() (string, error) { return "secret", nil }

	if _, err := NewGiteaForge(GiteaConfig{URL: "https://git.example.com"}); err == nil {
		t.Error("Token が nil でも通った")
	}
	if _, err := NewGiteaForge(GiteaConfig{Token: token}); err == nil {
		t.Error("URL が空でも通った")
	}
	if _, err := NewGiteaForge(GiteaConfig{URL: "ftp://git.example.com", Token: token}); err == nil {
		t.Error("http/https 以外の scheme が通った")
	}
	if _, err := NewGiteaForge(GiteaConfig{URL: "https://", Token: token}); err == nil {
		t.Error("ホストの無い URL が通った")
	}
	if _, err := NewGiteaForge(GiteaConfig{URL: "https://git.example.com", Owner: "org", Token: token}); err == nil {
		t.Error("Owner だけの指定が通った")
	}
	if _, err := NewGiteaForge(GiteaConfig{URL: "https://git.example.com", Repo: "repo", Token: token}); err == nil {
		t.Error("Repo だけの指定が通った")
	}

	f, err := NewGiteaForge(GiteaConfig{URL: "https://example.com/gitea/", Token: token})
	if err != nil {
		t.Fatal(err)
	}
	if f.apiURL != "https://example.com/gitea/api/v1" {
		t.Errorf("apiURL = %q", f.apiURL)
	}
	if f.client == nil || f.client.Timeout != 30*time.Second {
		t.Errorf("既定の client が違う: %+v", f.client)
	}
}

// newTestGitea は httptest のサーバーに繋いだ GiteaForge を作る。
func newTestGitea(t *testing.T, h http.HandlerFunc, owner, repo string) (*GiteaForge, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	f, err := NewGiteaForge(GiteaConfig{
		Dir: t.TempDir(), URL: srv.URL, Owner: owner, Repo: repo,
		Token: func() (string, error) { return "secret", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return f, srv
}

func TestGiteaForgeOpenPR(t *testing.T) {
	var mu sync.Mutex
	var pages []string
	var auth []string
	f, _ := newTestGitea(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s", r.Method)
		}
		if r.URL.Path != "/api/v1/repos/org/repo/pulls" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("state"); got != "open" {
			t.Errorf("state = %q", got)
		}
		if got := r.URL.Query().Get("limit"); got != "50" {
			t.Errorf("limit = %q", got)
		}
		page := r.URL.Query().Get("page")
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		pages = append(pages, page)
		mu.Unlock()
		items := []map[string]any{}
		switch page {
		case "1":
			// 50 件で埋めて次のページへ進ませる
			for i := 0; i < 50; i++ {
				items = append(items, map[string]any{
					"number": i + 1, "html_url": "https://gitea.example/org/repo/pulls/x",
					"title": "other", "body": "", "state": "open",
					"head": map[string]any{"ref": "other", "repo": map[string]any{"full_name": "org/repo"}},
				})
			}
		case "2":
			items = append(items,
				// ブランチ名は同じでも fork 元が違うものは拾わない
				map[string]any{
					"number": 10, "html_url": "https://gitea.example/fork/repo/pulls/10",
					"title": "fork", "body": "", "state": "open",
					"head": map[string]any{"ref": "feature", "repo": map[string]any{"full_name": "fork/repo"}},
				},
				// 閉じた PR は拾わない
				map[string]any{
					"number": 11, "html_url": "https://gitea.example/org/repo/pulls/11",
					"title": "closed", "body": "", "state": "closed",
					"head": map[string]any{"ref": "feature", "repo": map[string]any{"full_name": "org/repo"}},
				},
				// 開いていてブランチも fork 元も一致するもの (大小文字は無視)
				map[string]any{
					"number": 77, "html_url": "https://gitea.example/org/repo/pulls/77",
					"title": "title", "body": "body", "state": "open",
					"head": map[string]any{"ref": "feature", "repo": map[string]any{"full_name": "ORG/Repo"}},
				},
			)
		}
		_ = json.NewEncoder(w).Encode(items)
	}, "org", "repo")

	got, ok, err := f.OpenPR("feature")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("一致する PR を見つけられなかった")
	}
	if got.URL != "https://gitea.example/org/repo/pulls/77" || got.Number != 77 ||
		got.Title != "title" || got.Body != "body" {
		t.Errorf("unexpected PR: %+v", got)
	}
	mu.Lock()
	gotPages := append([]string(nil), pages...)
	gotAuth := append([]string(nil), auth...)
	mu.Unlock()
	if len(gotPages) != 2 || gotPages[0] != "1" || gotPages[1] != "2" {
		t.Errorf("ページングが違う: %v", gotPages)
	}
	for _, a := range gotAuth {
		if a != "token secret" {
			t.Errorf("Authorization = %q", a)
		}
	}

	// 一致する PR が無ければ ok=false
	f2, _ := newTestGitea(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	}, "org", "repo")
	if _, ok, err := f2.OpenPR("feature"); err != nil || ok {
		t.Fatalf("PR が無いとき: ok=%v err=%v", ok, err)
	}
}

func TestGiteaForgeCreatePR(t *testing.T) {
	var mu sync.Mutex
	var body map[string]string
	f, _ := newTestGitea(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if r.URL.Path != "/api/v1/repos/org/repo/pulls" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		if a := r.Header.Get("Authorization"); a != "token secret" {
			t.Errorf("Authorization = %q", a)
		}
		raw, _ := io.ReadAll(r.Body)
		var got map[string]string
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("body を読めない: %v", err)
		}
		mu.Lock()
		body = got
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 42, "html_url": "https://gitea.example/org/repo/pulls/42",
		})
	}, "org", "repo")

	got, err := f.CreatePR("feature", "main", "title", "body")
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "https://gitea.example/org/repo/pulls/42" || got.Number != 42 ||
		got.Title != "title" || got.Body != "body" {
		t.Errorf("unexpected PR: %+v", got)
	}
	mu.Lock()
	gotBody := body
	mu.Unlock()
	want := map[string]string{"head": "feature", "base": "main", "title": "title", "body": "body"}
	for k, v := range want {
		if gotBody[k] != v {
			t.Errorf("body[%q] = %q, want %q", k, gotBody[k], v)
		}
	}
}

func TestGiteaForgeEditPR(t *testing.T) {
	var mu sync.Mutex
	var body map[string]string
	f, _ := newTestGitea(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s", r.Method)
		}
		if r.URL.Path != "/api/v1/repos/org/repo/pulls/42" {
			t.Errorf("path = %q", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		var got map[string]string
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("body を読めない: %v", err)
		}
		mu.Lock()
		body = got
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42})
	}, "org", "repo")

	if err := f.EditPR(PullRequest{Number: 42}, "new title", "new body"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotBody := body
	mu.Unlock()
	if gotBody["title"] != "new title" || gotBody["body"] != "new body" {
		t.Errorf("unexpected body: %+v", gotBody)
	}

	// 番号が無ければ API を呼ばない
	requests := 0
	f2, _ := newTestGitea(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
	}, "org", "repo")
	if err := f2.EditPR(PullRequest{}, "t", "b"); err == nil {
		t.Error("番号の無い PR が通った")
	}
	if requests != 0 {
		t.Errorf("番号が無いのに API を呼んだ: %d", requests)
	}
}

func TestGiteaForgeAPIErrors(t *testing.T) {
	f, _ := newTestGitea(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"bad token"}`))
	}, "org", "repo")
	_, _, err := f.OpenPR("feature")
	if err == nil {
		t.Fatal("401 が通った")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "bad token") {
		t.Errorf("status か本文が抜けている: %v", err)
	}

	f2, _ := newTestGitea(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`already exists`))
	}, "org", "repo")
	if _, err := f2.CreatePR("feature", "main", "t", "b"); err == nil {
		t.Fatal("409 が通った")
	} else if !strings.Contains(err.Error(), "409") || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("status か本文が抜けている: %v", err)
	}
}

// Owner / Repo を省いたときは origin から解釈する。
func TestGiteaForgeRepoFromOrigin(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", dir},
		{"-C", dir, "remote", "add", "origin", "https://git.example.com/team/project.git"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	f, err := NewGiteaForge(GiteaConfig{
		Dir: dir, URL: "https://git.example.com",
		Token: func() (string, error) { return "secret", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	owner, repo, err := f.repoPath()
	if err != nil {
		t.Fatal(err)
	}
	if owner != "team" || repo != "project" {
		t.Errorf("repoPath() = %q, %q; want team, project", owner, repo)
	}

	// origin がローカルパスなら分かりやすく失敗する
	local := filepath.Join(t.TempDir(), "local")
	if out, err := exec.Command("git", "init", "-q", local).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", local, "remote", "add", "origin", local).CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v: %s", err, out)
	}
	f2, err := NewGiteaForge(GiteaConfig{
		Dir: local, URL: "https://git.example.com",
		Token: func() (string, error) { return "secret", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f2.repoPath(); err == nil {
		t.Error("ローカルパスの origin が通った")
	}
}
