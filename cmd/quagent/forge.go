package main

import (
	"fmt"
	"strings"

	"github.com/nananek/quagent/internal/config"
	"github.com/nananek/quagent/internal/pr"
)

// newPRForge は設定から PR を扱う forge を作る。設定ミスは run の開始時に検出する。
func newPRForge(cfg *config.Config, repo string) (pr.Forge, error) {
	forge, err := forgeForRepo(cfg, repo)
	if err != nil {
		return nil, err
	}
	switch forge {
	case config.ForgeGitHub:
		return &pr.GHForge{Dir: repo}, nil
	case config.ForgeGitea:
		g := cfg.PR.Gitea
		if g == nil {
			return nil, fmt.Errorf(`pr.forge が "gitea" だが pr.gitea が無い`)
		}
		return pr.NewGiteaForge(pr.GiteaConfig{
			Dir: repo, URL: g.URL, Owner: g.Owner, Repo: g.Repo, Token: g.Token,
		})
	default:
		return nil, fmt.Errorf(`pr.forge %q は未対応 (github / gitea / auto)`, forge)
	}
}

// forgeForRepo は repo で実際に使う forge を決める。"auto" のときだけ origin の
// ホストを見る (GitHub と Gitea のリポジトリを 1 つの設定で併用できるようにする)。
func forgeForRepo(cfg *config.Config, repo string) (string, error) {
	forge := cfg.PR.ForgeResolved()
	if forge != config.ForgeAuto {
		return forge, nil
	}
	host, err := pr.RemoteHost(repo)
	if err != nil {
		return "", fmt.Errorf(`pr.forge が "auto" だが origin のホストを読めない (pr.forge に "github" か "gitea" を指定する): %w`, err)
	}
	return selectForge(cfg, host)
}

// selectForge は origin のホスト host が pr.gitea.url と同じなら Gitea、
// それ以外は GitHub を返す。Gitea の設定が無ければ常に GitHub。
func selectForge(cfg *config.Config, host string) (string, error) {
	if cfg.PR.Gitea == nil {
		return config.ForgeGitHub, nil
	}
	want, err := cfg.PR.Gitea.Host()
	if err != nil {
		return "", fmt.Errorf(`pr.forge が "auto" だが %w`, err)
	}
	if strings.EqualFold(host, want) {
		return config.ForgeGitea, nil
	}
	return config.ForgeGitHub, nil
}
