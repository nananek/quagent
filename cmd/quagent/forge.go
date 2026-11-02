package main

import (
	"fmt"

	"github.com/nananek/quagent/internal/config"
	"github.com/nananek/quagent/internal/pr"
)

// newPRForge は設定から PR を扱う forge を作る。設定ミスは run の開始時に検出する。
func newPRForge(cfg *config.Config, repo string) (pr.Forge, error) {
	switch cfg.PR.ForgeResolved() {
	case "github":
		return &pr.GHForge{Dir: repo}, nil
	case "gitea":
		g := cfg.PR.Gitea
		if g == nil {
			return nil, fmt.Errorf(`pr.forge が "gitea" だが pr.gitea が無い`)
		}
		return pr.NewGiteaForge(pr.GiteaConfig{
			Dir: repo, URL: g.URL, Owner: g.Owner, Repo: g.Repo, Token: g.Token,
		})
	default:
		return nil, fmt.Errorf("pr.forge %q は未対応 (github / gitea)", cfg.PR.Forge)
	}
}
