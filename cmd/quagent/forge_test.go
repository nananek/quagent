package main

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/nananek/quagent/internal/config"
	"github.com/nananek/quagent/internal/pr"
)

func TestSelectForge(t *testing.T) {
	gitea := func(url string) *config.Gitea { return &config.Gitea{URL: url, TokenEnv: "TOKEN"} }
	cases := []struct {
		name    string
		gitea   *config.Gitea
		host    string
		want    string
		wantErr bool
	}{
		{name: "gitea 未設定なら github", host: "gitea.example.com", want: config.ForgeGitHub},
		{name: "ホストが一致すれば gitea", gitea: gitea("https://gitea.example.com"), host: "gitea.example.com", want: config.ForgeGitea},
		{name: "大文字小文字は無視", gitea: gitea("https://Gitea.Example.com"), host: "gitea.example.com", want: config.ForgeGitea},
		{name: "ポート付きの url でも一致する", gitea: gitea("http://gitea.example.com:3000"), host: "gitea.example.com", want: config.ForgeGitea},
		{name: "サブパス設置でも一致する", gitea: gitea("https://example.com/gitea"), host: "example.com", want: config.ForgeGitea},
		{name: "ホストが違えば github", gitea: gitea("https://gitea.example.com"), host: "github.com", want: config.ForgeGitHub},
		{name: "url が空なら失敗", gitea: gitea(""), host: "gitea.example.com", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &config.Config{PR: config.PR{Forge: config.ForgeAuto, Gitea: c.gitea}}
			got, err := selectForge(cfg, c.host)
			if c.wantErr {
				if err == nil {
					t.Fatalf("selectForge() = %q; want error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("selectForge() = %q; want %q", got, c.want)
			}
		})
	}
}

func TestForgeForRepo(t *testing.T) {
	gitea := &config.Gitea{URL: "https://gitea.example.com", TokenEnv: "TOKEN"}
	for _, c := range []struct {
		name  string
		forge string
		orig  string
		want  string
	}{
		{name: "auto で gitea の origin", forge: config.ForgeAuto, orig: "https://gitea.example.com/org/repo.git", want: config.ForgeGitea},
		{name: "auto で github の origin", forge: config.ForgeAuto, orig: "git@github.com:org/repo.git", want: config.ForgeGitHub},
		{name: "明示した gitea は origin を見ない", forge: config.ForgeGitea, orig: "git@github.com:org/repo.git", want: config.ForgeGitea},
		{name: "空なら github", forge: "", orig: "https://gitea.example.com/org/repo.git", want: config.ForgeGitHub},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, args := range [][]string{
				{"init", "-q", dir},
				{"-C", dir, "remote", "add", "origin", c.orig},
			} {
				if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v: %s", args, err, out)
				}
			}
			cfg := &config.Config{PR: config.PR{Forge: c.forge, Gitea: gitea}}
			got, err := forgeForRepo(cfg, dir)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("forgeForRepo() = %q; want %q", got, c.want)
			}
		})
	}
}

func TestNewPRForgeAuto(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", dir},
		{"-C", dir, "remote", "add", "origin", "https://gitea.example.com/org/repo.git"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	cfg := &config.Config{PR: config.PR{
		Forge: config.ForgeAuto,
		Gitea: &config.Gitea{URL: "https://gitea.example.com", TokenEnv: "TOKEN"},
	}}
	f, err := newPRForge(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.(*pr.GiteaForge); !ok {
		t.Errorf("newPRForge() = %T; want *pr.GiteaForge", f)
	}

	// auto で origin が読めないときは設定を直すよう促す
	bad := &config.Config{PR: config.PR{Forge: config.ForgeAuto, Gitea: cfg.PR.Gitea}}
	if _, err := newPRForge(bad, t.TempDir()); err == nil || !strings.Contains(err.Error(), "origin") {
		t.Errorf("origin の無い repo でエラーにならなかった: %v", err)
	}

	// 未知の forge は失敗する
	unknown := &config.Config{PR: config.PR{Forge: "gitlab"}}
	if _, err := newPRForge(unknown, dir); err == nil || !strings.Contains(err.Error(), "未対応") {
		t.Errorf("未知の forge でエラーにならなかった: %v", err)
	}
}
