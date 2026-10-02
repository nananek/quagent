// Package pr は guest のブランチを host 側で取り込み、署名して push し、PR を作る。
//
// gh のトークンも利用者の署名鍵も VM に入れない。VM 内のコミットは run ごとの
// 捨て鍵 (印) で署名させておき、host は guest の /work からブランチを取り込んで、
// 印の付いたコミット (= VM で作ったもの) だけを利用者の鍵で署名し直して、host の
// repo の origin へ push する。host の repo に既にあるコミット (他人のものや未 push
// のもの) には一切触らない。push 先は固定で、保護ブランチには push しない。
// リモートに同名のブランチが既にあれば上書きしない。
package pr

import (
	"bytes"
	"fmt"
	"io"
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
	Head    string `json:"head"`    // push したコミット
	Signed  int    `json:"signed"`  // 今回署名し直したコミット数
	Created bool   `json:"created"` // PR を新しく作った
}

// Publisher は 1 つの run の PR 作成を担う。
type Publisher struct {
	Repo      string   // host の repo (origin と gh の対象)
	Work      string   // run の作業ディレクトリ
	GuestURL  string   // guest の repo を取り込む URL
	GitConfig []string // git に足す設定 (-c の値。ext:: 転送の許可など)
	Protected []string // 保護ブランチ
	// MarkPub は VM に渡した捨て鍵の公開鍵 (ssh 形式)。この鍵の署名がある
	// コミットだけを「VM で作ったもの」として署名し直す。
	MarkPub string
	// GH は PR を作る (nil なら作らずに push まで)。テストで差し替える。
	GH func(dir string, args ...string) ([]byte, error)

	mu       sync.Mutex
	bare     string
	markFP   string
	signed   map[string]string // VM のコミット -> 署名し直したコミット
	identity [2]string         // 署名し直すコミットの committer (host の user.name / user.email)
}

func (p *Publisher) gitIO(dir string, env []string, stdin io.Reader, args ...string) (string, error) {
	// 取り込んだ内容に由来する hooks は実行しない (bare repo にも念のため無効化)
	pre := []string{"-c", "core.hooksPath=/dev/null"}
	for _, c := range p.GitConfig {
		pre = append(pre, "-c", c)
	}
	cmd := exec.Command("git", append(pre, args...)...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), env...)
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func (p *Publisher) git(dir string, args ...string) (string, error) {
	return p.gitIO(dir, nil, nil, args...)
}

// prepare は run 専用の bare repo を用意する。オブジェクトは host の repo を
// 参照する (alternates) ので複製しない。捨て鍵の署名を確かめる設定も置く。
func (p *Publisher) prepare() error {
	if p.bare != "" {
		return nil
	}
	if p.MarkPub == "" {
		return fmt.Errorf("捨て鍵の公開鍵 (MarkPub) が無い")
	}
	bare := filepath.Join(p.Work, "pr.git")
	if _, err := p.git(p.Work, "init", "-q", "--bare", bare); err != nil {
		return err
	}
	objects, err := p.git(p.Repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	alt := filepath.Join(bare, "objects", "info", "alternates")
	if err := os.WriteFile(alt, []byte(filepath.Join(objects, "objects")+"\n"), 0o644); err != nil {
		return err
	}
	signers := filepath.Join(p.Work, "pr-allowed-signers")
	if err := os.WriteFile(signers, []byte("* namespaces=\"git\" "+strings.TrimSpace(p.MarkPub)+"\n"), 0o644); err != nil {
		return err
	}
	if _, err := p.git(bare, "config", "gpg.ssh.allowedSignersFile", signers); err != nil {
		return err
	}
	pubFile := filepath.Join(p.Work, "pr-mark.pub")
	if err := os.WriteFile(pubFile, []byte(p.MarkPub+"\n"), 0o644); err != nil {
		return err
	}
	out, err := exec.Command("ssh-keygen", "-lf", pubFile).Output()
	if err != nil {
		return fmt.Errorf("捨て鍵の指紋を取れない: %w", err)
	}
	f := strings.Fields(string(out))
	if len(f) < 2 {
		return fmt.Errorf("捨て鍵の指紋を取れない: %q", out)
	}
	name, _ := p.git(p.Repo, "config", "--get", "user.name")
	email, _ := p.git(p.Repo, "config", "--get", "user.email")
	if name == "" || email == "" {
		return fmt.Errorf("host の git に user.name / user.email が無い")
	}
	p.bare, p.markFP, p.identity = bare, f[1], [2]string{name, email}
	p.signed = map[string]string{}
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

// Publish はブランチを取り込み、VM で作ったコミットだけを署名し直して push し、
// PR を作る (既にあれば更新)。
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
	if err := p.prepare(); err != nil {
		return Result{}, err
	}
	origin, err := p.git(p.Repo, "remote", "get-url", "origin")
	if err != nil {
		return Result{}, fmt.Errorf("host の repo に origin が無い: %w", err)
	}

	incoming := "refs/quagent/incoming/" + req.Branch
	baseRef := "refs/quagent/base/" + base
	pushedRef := "refs/heads/" + req.Branch // 前回 push したもの
	if _, err := p.git(p.bare, "fetch", "-q", "--no-tags", p.GuestURL, "+refs/heads/"+req.Branch+":"+incoming); err != nil {
		return Result{}, fmt.Errorf("guest からブランチ %q を取り込めない (/work でコミット済みか確認): %w", req.Branch, err)
	}
	if _, err := p.git(p.bare, "fetch", "-q", "--no-tags", origin, "+refs/heads/"+base+":"+baseRef); err != nil {
		return Result{}, fmt.Errorf("origin の %q を取り込めない: %w", base, err)
	}
	tip, err := p.git(p.bare, "rev-parse", incoming)
	if err != nil {
		return Result{}, err
	}

	head, n, err := p.rewrite(tip, baseRef)
	if err != nil {
		return Result{}, err
	}
	prev, _ := p.git(p.bare, "rev-parse", "--verify", "-q", pushedRef)
	if head == prev {
		return p.finish(req, base, head, 0)
	}
	// 初回はリモートに同名のブランチが無いことを、更新時は前回 push したコミットの
	// ままであることを条件にする (利用者のブランチを上書きしない)
	lease := "--force-with-lease=" + pushedRef + ":" + prev
	if _, err := p.git(p.bare, "push", "-q", lease, origin, head+":"+pushedRef); err != nil {
		return Result{}, fmt.Errorf("push に失敗 (リモートに同名のブランチが既にあるなら上書きしない): %w", err)
	}
	if _, err := p.git(p.bare, "update-ref", pushedRef, head); err != nil {
		return Result{}, err
	}
	return p.finish(req, base, head, n)
}

// rewrite は base に無いコミットを古い順に見て、捨て鍵の印があるもの (VM で
// 作ったもの) だけを利用者の鍵で署名し直し、新しい先端と署名し直した数を返す。
// host の repo に既にあるコミットはそのまま使う (ハッシュも署名も変えない)。
// 印が無く host にも無いコミットは、VM で署名を切って作られたものなので拒否する。
func (p *Publisher) rewrite(tip, baseRef string) (string, int, error) {
	out, err := p.git(p.bare, "rev-list", "--reverse", "--topo-order", tip, "--not", baseRef)
	if err != nil {
		return "", 0, err
	}
	n := 0
	for _, c := range strings.Fields(out) {
		if _, ok := p.signed[c]; ok {
			continue
		}
		if p.hostHas(c) {
			continue
		}
		marked, err := p.isMarked(c)
		if err != nil {
			return "", 0, err
		}
		if !marked {
			return "", 0, fmt.Errorf("コミット %s は VM の署名 (捨て鍵) が無く、host の repo にも無い。"+
				"commit.gpgsign を無効にせずにコミットし直す", c[:12])
		}
		nc, err := p.resign(c)
		if err != nil {
			return "", 0, err
		}
		p.signed[c] = nc
		n++
	}
	if nc, ok := p.signed[tip]; ok {
		return nc, n, nil
	}
	return tip, n, nil
}

func (p *Publisher) hostHas(c string) bool {
	_, err := p.git(p.Repo, "cat-file", "-e", c+"^{commit}")
	return err == nil
}

// isMarked はコミットが捨て鍵で正しく署名されているかを返す。
func (p *Publisher) isMarked(c string) (bool, error) {
	out, err := p.git(p.bare, "log", "-1", "--format=%G?%x00%GF", c)
	if err != nil {
		return false, err
	}
	st, fp, _ := strings.Cut(out, "\x00")
	return st == "G" && fp == p.markFP, nil
}

// resign はコミット c を、中身・author・メッセージはそのままに、親を署名し直した
// ものに付け替え、committer を利用者にして利用者の鍵で署名し直す。
func (p *Publisher) resign(c string) (string, error) {
	info, err := p.git(p.bare, "log", "-1", "--date=raw", "--format=%T%x00%P%x00%an%x00%ae%x00%ad%x00%cd", c)
	if err != nil {
		return "", err
	}
	f := strings.Split(info, "\x00")
	if len(f) != 6 {
		return "", fmt.Errorf("コミット %s を読めない", c)
	}
	raw, err := p.git(p.bare, "cat-file", "commit", c)
	if err != nil {
		return "", err
	}
	_, msg, _ := strings.Cut(raw, "\n\n")
	args := []string{"commit-tree", "--gpg-sign", f[0]}
	for _, parent := range strings.Fields(f[1]) {
		if np, ok := p.signed[parent]; ok {
			parent = np
		}
		args = append(args, "-p", parent)
	}
	args = append(args, "-F", "-")
	env := []string{
		"GIT_AUTHOR_NAME=" + f[2], "GIT_AUTHOR_EMAIL=" + f[3], "GIT_AUTHOR_DATE=" + f[4],
		"GIT_COMMITTER_NAME=" + p.identity[0], "GIT_COMMITTER_EMAIL=" + p.identity[1], "GIT_COMMITTER_DATE=" + f[5],
	}
	nc, err := p.gitIO(p.bare, env, strings.NewReader(msg), args...)
	if err != nil {
		return "", fmt.Errorf("署名し直しに失敗: %w", err)
	}
	return nc, nil
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
