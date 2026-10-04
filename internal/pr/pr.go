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
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// DefaultProtected は既定の保護ブランチ。
var DefaultProtected = []string{"main", "master", "develop"}

// 敵対的なエージェントに host の資源 (署名・ディスク・GitHub) を使い潰させない上限。
const (
	maxTitleRunes = 256
	maxBodyBytes  = 60000
	maxNewCommits = 500 // 1 回の依頼で署名し直すコミット数
	maxBranches   = 10  // 1 回の run で PR にできるブランチ数
	minInterval   = 10 * time.Second
	maxFetchFile  = 2 << 30 // guest からの取り込みで書けるファイルの大きさ
	gitTimeout    = 10 * time.Minute
)

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
	// MinInterval は依頼と依頼の最小の間隔 (0 なら制限しない)。
	MinInterval time.Duration
	// Approve は push の前に承認を求める (nil なら確認なしで進む)。エラーを返すと
	// push も PR の作成もしない。base は既定ブランチを埋めた値。
	Approve func(Request) error
	// GH は PR を作る (nil なら作らずに push まで)。テストで差し替える。
	GH func(dir string, args ...string) ([]byte, error)

	mu        sync.Mutex
	bare      string
	markFP    string
	signed    map[string]string // VM のコミット -> 署名し直したコミット
	identity  [2]string         // 署名し直すコミットの committer (host の user.name / user.email)
	signCfg   []string          // 署名し直すときに足す -c (host の repo で効いている署名の設定)
	branches  map[string]bool   // この run で扱ったブランチ
	last      time.Time         // 前回の依頼の時刻
	remoteTip string            // 既に push 済みのブランチの先端 (無ければ空)
}

func (p *Publisher) gitIO(dir string, env []string, stdin io.Reader, args ...string) (string, error) {
	// 取り込んだ内容に由来する hooks は実行しない (bare repo にも念のため無効化)
	pre := []string{"-c", "core.hooksPath=/dev/null"}
	for _, c := range p.GitConfig {
		pre = append(pre, "-c", c)
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	argv := append([]string{"git"}, append(pre, args...)...)
	if len(args) > 0 && args[0] == "fetch" {
		// guest から取り込むときは書けるファイルの大きさを制限する (host のディスクを守る)
		argv = append([]string{"prlimit", fmt.Sprintf("--fsize=%d", maxFetchFile), "--"}, argv...)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), env...)
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("git %s が %s 以内に終わらなかった", args[0], gitTimeout)
		}
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
	signCfg, err := p.signingConfig()
	if err != nil {
		return err
	}
	p.bare, p.markFP, p.identity, p.signCfg = bare, f[1], [2]string{name, email}, signCfg
	p.signed = map[string]string{}
	p.branches = map[string]bool{}
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
	if utf8.RuneCountInString(req.Title) > maxTitleRunes || len(req.Body) > maxBodyBytes {
		return Result{}, fmt.Errorf("タイトルは %d 文字、本文は %d バイトまで", maxTitleRunes, maxBodyBytes)
	}
	if wait := p.MinInterval - time.Since(p.last); wait > 0 {
		return Result{}, fmt.Errorf("PR の依頼は %s おきにしかできない。%s 後にもう一度", p.MinInterval, wait.Round(time.Second))
	}
	p.last = time.Now()
	base := req.Base
	if base == "" {
		base = p.defaultBase()
	}
	if strings.HasPrefix(base, "-") {
		return Result{}, fmt.Errorf("base が不正: %q", base)
	}
	if !p.branches[req.Branch] && len(p.branches) >= maxBranches {
		return Result{}, fmt.Errorf("1 回の run で PR にできるブランチは %d 本まで", maxBranches)
	}
	// 承認制なら、取り込みや push の前に人が確認する。拒否・時間切れなら何もしない。
	if p.Approve != nil {
		ar := req
		ar.Base = base
		if err := p.Approve(ar); err != nil {
			return Result{}, err
		}
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
	pushedRef := "refs/heads/" + req.Branch
	remoteRef := "refs/quagent/remote/" + req.Branch
	if _, err := p.git(p.bare, "fetch", "-q", "--no-tags", p.GuestURL, "+refs/heads/"+req.Branch+":"+incoming); err != nil {
		return Result{}, fmt.Errorf("guest からブランチ %q を取り込めない (/work でコミット済みか確認): %w", req.Branch, err)
	}
	if _, err := p.git(p.bare, "fetch", "-q", "--no-tags", origin, "+refs/heads/"+base+":"+baseRef); err != nil {
		return Result{}, fmt.Errorf("origin の %q を取り込めない: %w", base, err)
	}
	// 既に push 済みのブランチなら取り込む。同じ run でなくても、guest に残っている
	// 対応するコミットを再利用して追記できるようにする (内容で照合し、対応しない
	// コミットがあるブランチは rewrite が拒否する)。
	p.remoteTip = ""
	if out, err := p.git(p.bare, "ls-remote", origin, "refs/heads/"+req.Branch); err == nil {
		if f := strings.Fields(out); len(f) > 0 {
			p.remoteTip = f[0]
			if _, err := p.git(p.bare, "fetch", "-q", "--no-tags", origin, "+refs/heads/"+req.Branch+":"+remoteRef); err != nil {
				return Result{}, fmt.Errorf("origin の %q を取り込めない: %w", req.Branch, err)
			}
		}
	}
	tip, err := p.git(p.bare, "rev-parse", incoming)
	if err != nil {
		return Result{}, err
	}

	head, n, err := p.rewrite(tip, baseRef)
	if err != nil {
		return Result{}, err
	}
	p.branches[req.Branch] = true
	if p.remoteTip != "" && head == p.remoteTip {
		return p.finish(req, base, head, 0)
	}
	// 新規はリモートに同名のブランチが無いことを、更新は取り込んだ先端のままで
	// あることを条件にする。既存ブランチへは、rewrite が「guest のコミットと
	// 対応する」と確かめた先端にしか push しない (他人のブランチを書き換えない)。
	lease := "--force-with-lease=" + pushedRef + ":" + p.remoteTip
	if _, err := p.git(p.bare, "push", "-q", lease, origin, head+":"+pushedRef); err != nil {
		return Result{}, fmt.Errorf("push に失敗 (リモートに同名のブランチが既にあるなら上書きしない): %w", err)
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
	commits := strings.Fields(out)
	// 既存ブランチなら、前に push したコミットを guest のコミットと内容で照合し、
	// 署名し直さずそのまま使う (ハッシュを変えずに追記できるようにする)。
	if p.remoteTip != "" {
		if err := p.reuseRemote(commits, baseRef); err != nil {
			return "", 0, err
		}
	}
	fresh := 0
	for _, c := range commits {
		if _, ok := p.signed[c]; !ok {
			fresh++
		}
	}
	// host にあるコミットも含めた数で先に弾く (1 件ずつ確かめる前に)
	if fresh > maxNewCommits*4 {
		return "", 0, fmt.Errorf("base に無いコミットが多すぎる (%d 件)", fresh)
	}
	n := 0
	for _, c := range commits {
		if _, ok := p.signed[c]; ok {
			continue
		}
		if p.hostHas(c) {
			continue
		}
		if n >= maxNewCommits {
			return "", 0, fmt.Errorf("1 回に署名し直せるコミットは %d 件まで", maxNewCommits)
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

// reuseRemote は、既に push 済みのブランチのコミットを guest のコミットと内容で
// 照合し、対応するものは署名し直さずそのまま使う (PR への追記でハッシュを変え
// ないため)。リモートに guest と対応しないコミットがあるなら、quagent が作った
// ブランチではないので拒否する (他人のブランチを書き換えない)。
func (p *Publisher) reuseRemote(commits []string, baseRef string) error {
	out, err := p.git(p.bare, "rev-list", "--reverse", "--topo-order", p.remoteTip, "--not", baseRef)
	if err != nil {
		return err
	}
	remote := strings.Fields(out)
	used := map[string]bool{}
	for _, c := range commits {
		if s, ok := p.signed[c]; ok {
			used[s] = true // この run で既に署名した分
			continue
		}
		cm, err := p.commitMeta(c)
		if err != nil {
			return err
		}
		want := make([]string, len(cm.parents))
		for i, par := range cm.parents {
			if s, ok := p.signed[par]; ok {
				want[i] = s
			} else {
				want[i] = par
			}
		}
		for _, r := range remote {
			if used[r] {
				continue
			}
			rm, err := p.commitMeta(r)
			if err != nil {
				return err
			}
			if sameCommit(cm, rm, want) {
				p.signed[c] = r
				used[r] = true
				break
			}
		}
	}
	if !used[p.remoteTip] {
		return fmt.Errorf("リモートのブランチは guest のコミットと対応していない。上書きしない")
	}
	for _, r := range remote {
		if !used[r] {
			return fmt.Errorf("リモートのブランチに guest と対応しないコミットがある。上書きしない")
		}
	}
	return nil
}

// commitMeta は署名と committer の名義を除いた、コミットの中身を比べるための情報。
// guest のコミット (捨て鍵で署名) と host が署名し直したコミットは、中身・author・
// author date・committer date・メッセージが同じで、親は署名し直しの対応をたどれば
// 一致する。committer の名義 (捨て鍵の run か利用者か) と署名だけが違う。
type commitMeta struct {
	tree          string
	parents       []string
	authorName    string
	authorEmail   string
	authorDate    string
	committerDate string
	message       string
}

func (p *Publisher) commitMeta(rev string) (commitMeta, error) {
	out, err := p.git(p.bare, "log", "-1", "--date=raw",
		"--format=%T%x00%P%x00%an%x00%ae%x00%ad%x00%cd%x00%B", rev)
	if err != nil {
		return commitMeta{}, err
	}
	f := strings.Split(out, "\x00")
	if len(f) < 7 {
		return commitMeta{}, fmt.Errorf("コミット %s を読めない", rev)
	}
	return commitMeta{tree: f[0], parents: strings.Fields(f[1]), authorName: f[2], authorEmail: f[3],
		authorDate: f[4], committerDate: f[5], message: f[6]}, nil
}

func sameCommit(a, b commitMeta, wantParents []string) bool {
	if a.tree != b.tree || a.authorName != b.authorName || a.authorEmail != b.authorEmail ||
		a.authorDate != b.authorDate || a.committerDate != b.committerDate || a.message != b.message {
		return false
	}
	if len(b.parents) != len(wantParents) {
		return false
	}
	for i := range wantParents {
		if b.parents[i] != wantParents[i] {
			return false
		}
	}
	return true
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

// signingConfig は host の repo で効いている署名の設定 (gpg.* と user.signingkey) を
// -c の引数の形で返す。署名し直しは run 専用の bare repo で行うので、そのままでは
// repo の .git/config や includeIf "gitdir:..." の設定が効かない。捨て鍵の確認に使う
// allowedSignersFile は bare repo のものを使うので渡さない。
func (p *Publisher) signingConfig() ([]string, error) {
	cmd := exec.Command("git", "config", "-z", "--get-regexp", `^(gpg\.|user\.signingkey$)`)
	cmd.Dir = p.Repo
	out, err := cmd.Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return nil, nil // 該当する設定が無い
	}
	if err != nil {
		return nil, fmt.Errorf("host の repo の署名の設定を読めない: %w", err)
	}
	var args []string
	for _, ent := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		key, val, hasVal := strings.Cut(ent, "\n")
		switch {
		case key == "gpg.ssh.allowedsignersfile":
		case hasVal:
			args = append(args, "-c", key+"="+val)
		default: // 値の無い真偽値 ("[gpg] foo" だけの行)
			args = append(args, "-c", key)
		}
	}
	return args, nil
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
	args := append(slices.Clone(p.signCfg), "commit-tree", "--gpg-sign", f[0])
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
