// Package pr は guest のブランチを host 側で取り込み、署名して push し、PR を作る。
//
// gh のトークンも署名鍵も VM に入れない。host は ssh で guest の /work から
// ブランチを fetch し、run 専用の bare repo (hooks 無効) で新しいコミットだけを
// host の鍵で署名し直して、host の repo の origin へ push する。push 先は固定で、
// 保護ブランチには push しない。リモートに同名のブランチが既にあれば上書きしない。
package pr

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// DefaultProtected は既定の保護ブランチ。
var DefaultProtected = []string{"main", "master", "develop"}

// Request はエージェントからの PR 作成・更新の依頼。
type Request struct {
	Branch string
	Title  string
	Body   string
	// Base は PR の向き先。空なら origin の既定ブランチ。
	Base string
}

// Result は依頼の結果。
type Result struct {
	URL     string `json:"url,omitempty"`
	Branch  string `json:"branch"`
	Base    string `json:"base"`
	Head    string `json:"head"`    // push した (署名済みの) コミット
	Signed  int    `json:"signed"`  // 今回署名したコミット数
	Created bool   `json:"created"` // PR を新しく作った
}

// Publisher は 1 つの run の PR 作成を担う。
type Publisher struct {
	Repo      string   // host の repo (origin と gh の対象)
	Work      string   // run の作業ディレクトリ
	GuestURL  string   // guest の repo を取り込む URL
	GitConfig []string // git に足す設定 (-c の値。ext:: 転送の許可など)
	Protected []string // 保護ブランチ
	// GH は PR を作る (nil なら作らずに push まで)。テストで差し替える。
	GH func(dir string, args ...string) ([]byte, error)

	mu   sync.Mutex
	bare string
}

func (p *Publisher) git(dir string, args ...string) (string, error) {
	// 取り込んだ内容に由来する hooks は実行しない (bare repo にも念のため無効化)
	pre := []string{"-c", "core.hooksPath=/dev/null"}
	for _, c := range p.GitConfig {
		pre = append(pre, "-c", c)
	}
	cmd := exec.Command("git", append(pre, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func (p *Publisher) ensureBare() error {
	if p.bare != "" {
		return nil
	}
	bare := filepath.Join(p.Work, "pr.git")
	if _, err := p.git(p.Work, "init", "-q", "--bare", bare); err != nil {
		return err
	}
	p.bare = bare
	return nil
}

// CheckBranch はエージェントが使ってよいブランチ名かを確かめる。
func (p *Publisher) CheckBranch(branch string) error {
	if branch == "" || strings.HasPrefix(branch, "-") {
		return fmt.Errorf("ブランチ名が不正: %q", branch)
	}
	if _, err := p.git(p.Work, "check-ref-format", "--branch", branch); err != nil {
		return fmt.Errorf("ブランチ名が不正: %q", branch)
	}
	if slices.Contains(p.Protected, branch) {
		return fmt.Errorf("%q は保護ブランチなので push できない。別のブランチで作業する", branch)
	}
	return nil
}

func (p *Publisher) defaultBase() string {
	ref, err := p.git(p.Repo, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err == nil {
		if b, ok := strings.CutPrefix(ref, "origin/"); ok && b != "" {
			return b
		}
	}
	return "main"
}

// Publish はブランチを取り込み、署名して push し、PR を作る (既にあれば更新)。
func (p *Publisher) Publish(req Request) (Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.CheckBranch(req.Branch); err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(req.Title) == "" {
		return Result{}, fmt.Errorf("タイトルが必要")
	}
	base := req.Base
	if base == "" {
		base = p.defaultBase()
	}
	if strings.HasPrefix(base, "-") {
		return Result{}, fmt.Errorf("base が不正: %q", base)
	}
	if err := p.ensureBare(); err != nil {
		return Result{}, err
	}
	origin, err := p.git(p.Repo, "remote", "get-url", "origin")
	if err != nil {
		return Result{}, fmt.Errorf("host の repo に origin が無い: %w", err)
	}

	guestRef := "refs/quagent/guest/" + req.Branch // 前回取り込んだ guest の先端
	newRef := "refs/quagent/incoming/" + req.Branch
	signedRef := "refs/heads/" + req.Branch // 署名済み (push したもの)

	// guest のブランチと、origin の base を取り込む
	if _, err := p.git(p.bare, "fetch", "-q", "--no-tags", p.GuestURL, "+refs/heads/"+req.Branch+":"+newRef); err != nil {
		return Result{}, fmt.Errorf("guest からブランチ %q を取り込めない (/work でコミット済みか確認): %w", req.Branch, err)
	}
	if _, err := p.git(p.bare, "fetch", "-q", "--no-tags", origin, "+refs/heads/"+base+":refs/quagent/base/"+base); err != nil {
		return Result{}, fmt.Errorf("origin の %q を取り込めない: %w", base, err)
	}
	newTip, _ := p.git(p.bare, "rev-parse", newRef)

	// 署名し直す範囲: 前回取り込んだ先端から続いていれば新しい分だけ、
	// そうでなければ (初回・履歴の書き換え) base から分岐した全部
	prevGuest, errPrev := p.git(p.bare, "rev-parse", "--verify", "-q", guestRef)
	prevSigned, _ := p.git(p.bare, "rev-parse", "--verify", "-q", signedRef)
	var onto, upstream string
	if errPrev == nil && prevSigned != "" && p.isAncestor(prevGuest, newTip) {
		if prevGuest == newTip {
			return p.finish(req, base, prevSigned, 0)
		}
		onto, upstream = prevSigned, prevGuest
	} else {
		mb, err := p.git(p.bare, "merge-base", newTip, "refs/quagent/base/"+base)
		if err != nil {
			return Result{}, fmt.Errorf("%q と %q の共通の祖先が無い", req.Branch, base)
		}
		onto, upstream = mb, mb
	}
	count, _ := p.git(p.bare, "rev-list", "--count", upstream+".."+newTip)
	signed, err := p.resign(onto, upstream, newTip)
	if err != nil {
		return Result{}, err
	}

	// push。初回はリモートに同名のブランチが無いことを、更新時は前回 push した
	// コミットのままであることを条件にする (利用者のブランチを上書きしない)
	lease := "--force-with-lease=" + signedRef + ":" + prevSigned
	if _, err := p.git(p.bare, "push", "-q", lease, origin, signed+":"+signedRef); err != nil {
		return Result{}, fmt.Errorf("push に失敗 (リモートに同名のブランチが既にあるなら上書きしない): %w", err)
	}
	if _, err := p.git(p.bare, "update-ref", signedRef, signed); err != nil {
		return Result{}, err
	}
	if _, err := p.git(p.bare, "update-ref", guestRef, newTip); err != nil {
		return Result{}, err
	}
	n := 0
	fmt.Sscan(count, &n)
	return p.finish(req, base, signed, n)
}

func (p *Publisher) isAncestor(a, b string) bool {
	_, err := p.git(p.bare, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

// resign は upstream..tip のコミットを onto の上に host の鍵で署名し直して積み、
// 新しい先端を返す。中身 (tree) は変えない。
func (p *Publisher) resign(onto, upstream, tip string) (string, error) {
	wt := filepath.Join(p.Work, "pr-worktree")
	_ = os.RemoveAll(wt)
	if _, err := p.git(p.bare, "worktree", "add", "-q", "--detach", wt, tip); err != nil {
		return "", err
	}
	defer func() {
		_, _ = p.git(p.bare, "worktree", "remove", "--force", wt)
	}()
	if _, err := p.git(wt, "rebase", "-q", "--force-rebase", "--rebase-merges", "--gpg-sign",
		"--onto", onto, upstream); err != nil {
		_, _ = p.git(wt, "rebase", "--abort")
		return "", fmt.Errorf("署名し直しに失敗: %w", err)
	}
	head, err := p.git(wt, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	// 念のため中身が変わっていないことを確かめる
	a, _ := p.git(p.bare, "rev-parse", tip+"^{tree}")
	b, _ := p.git(p.bare, "rev-parse", head+"^{tree}")
	if a != b {
		return "", fmt.Errorf("署名し直しで中身が変わった (%s != %s)", a, b)
	}
	return head, nil
}

func (p *Publisher) finish(req Request, base, head string, signed int) (Result, error) {
	res := Result{Branch: req.Branch, Base: base, Head: head, Signed: signed}
	if p.GH == nil {
		return res, nil
	}
	// 開いている PR があれば push で更新済み。閉じた・マージ済みの PR は使い回さない
	if out, err := p.GH(p.Repo, "pr", "view", req.Branch, "--json", "url,state", "-q", `select(.state == "OPEN") | .url`); err == nil {
		if url := strings.TrimSpace(string(out)); url != "" {
			res.URL = url
			return res, nil
		}
	}
	out, err := p.GH(p.Repo, "pr", "create", "--head", req.Branch, "--base", base,
		"--title", req.Title, "--body", req.Body)
	if err != nil {
		return res, fmt.Errorf("push はしたが PR を作れない: %v: %s", err, out)
	}
	res.URL = strings.TrimSpace(lastLine(string(out)))
	res.Created = true
	return res, nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// RunGH は host の gh を実行する。
func RunGH(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("gh", args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}
