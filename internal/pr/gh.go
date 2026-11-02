package pr

import (
	"encoding/json"
	"fmt"
	"strings"
)

// GHForge は gh CLI で GitHub / GitHub Enterprise の PR を扱う。
type GHForge struct {
	Dir string
	// Run は gh を実行する。nil なら RunGH。テストで差し替える。
	Run func(dir string, args ...string) ([]byte, error)
}

func (f *GHForge) run(args ...string) ([]byte, error) {
	run := f.Run
	if run == nil {
		run = RunGH
	}
	return run(f.Dir, args...)
}

// OpenPR は branch に開いている PR を返す。無ければ ok=false。
func (f *GHForge) OpenPR(branch string) (PullRequest, bool, error) {
	out, err := f.run("pr", "view", branch, "--json", "url,state,title,body,number")
	if err != nil {
		// gh は PR が無いときにも終了コードを変えるので、出力の文言で見分ける。
		low := strings.ToLower(string(out))
		if strings.Contains(low, "no pull requests found") ||
			strings.Contains(low, "no open pull requests found") {
			return PullRequest{}, false, nil
		}
		return PullRequest{}, false, fmt.Errorf("gh pr view: %v: %s", err, strings.TrimSpace(string(out)))
	}
	var v struct {
		URL    string `json:"url"`
		State  string `json:"state"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		Number int    `json:"number"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return PullRequest{}, false, fmt.Errorf("gh pr view の出力を読めない: %w", err)
	}
	if v.State != "OPEN" || v.URL == "" {
		return PullRequest{}, false, nil
	}
	return PullRequest{URL: v.URL, Number: v.Number, Title: v.Title, Body: v.Body}, true, nil
}

// CreatePR は PR を作成して返す。
func (f *GHForge) CreatePR(branch, base, title, body string) (PullRequest, error) {
	out, err := f.run("pr", "create", "--head", branch, "--base", base,
		"--title", title, "--body", body)
	if err != nil {
		return PullRequest{}, fmt.Errorf("gh pr create: %v: %s", err, strings.TrimSpace(string(out)))
	}
	url := strings.TrimSpace(lastLine(string(out)))
	if url == "" {
		return PullRequest{}, fmt.Errorf("gh pr create が URL を返さない: %s", strings.TrimSpace(string(out)))
	}
	return PullRequest{URL: url, Title: title, Body: body}, nil
}

// EditPR は既存 PR のタイトル・本文を更新する。
func (f *GHForge) EditPR(pr PullRequest, title, body string) error {
	if pr.URL == "" {
		return fmt.Errorf("PR の URL が無い")
	}
	out, err := f.run("pr", "edit", pr.URL, "--title", title, "--body", body)
	if err != nil {
		return fmt.Errorf("gh pr edit: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
