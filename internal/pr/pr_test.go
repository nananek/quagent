package pr

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func keygen(t *testing.T, path string) {
	t.Helper()
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", path).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
}

func commit(t *testing.T, dir, file, content string, extra ...string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "add", file)
	run(t, dir, append(append([]string{"git"}, extra...), "commit", "-q", "-m", "edit "+file)...)
}

// sigFP はコミットの署名鍵の指紋 (無ければ空) を返す。
func sigFP(t *testing.T, repo, rev, signers string) string {
	t.Helper()
	return run(t, repo, "git", "-c", "gpg.ssh.allowedSignersFile="+signers, "log", "-1", "--format=%GF", rev)
}

func fingerprint(t *testing.T, pub string) string {
	return strings.Fields(run(t, ".", "ssh-keygen", "-lf", pub))[1]
}

type fixture struct {
	origin, host, guest string
	hostKey, markKey    string // 利用者の鍵、VM の捨て鍵 (どちらも ssh)
	signers             string // 両方の鍵を許可した allowed signers
	p                   *Publisher
}

// newFixture は origin / host / guest の 3 つの repo を作る。host の利用者は
// hostKey で署名し、guest (VM) は markKey で署名する。利用者の設定は読まない。
func newFixture(t *testing.T) fixture {
	dir := t.TempDir()
	f := fixture{
		origin:  filepath.Join(dir, "origin.git"),
		host:    filepath.Join(dir, "host"),
		guest:   filepath.Join(dir, "guest"),
		hostKey: filepath.Join(dir, "hostkey"),
		markKey: filepath.Join(dir, "markkey"),
		signers: filepath.Join(dir, "signers"),
	}
	keygen(t, f.hostKey)
	keygen(t, f.markKey)
	cfg := filepath.Join(dir, "gitconfig")
	content := "[user]\n\tname = Host User\n\temail = host@example.com\n\tsigningkey = " + f.hostKey + ".pub\n" +
		"[gpg]\n\tformat = ssh\n[commit]\n\tgpgsign = true\n[init]\n\tdefaultBranch = main\n"
	if err := os.WriteFile(cfg, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	var sig strings.Builder
	for _, k := range []string{f.hostKey, f.markKey} {
		b, _ := os.ReadFile(k + ".pub")
		sig.WriteString("* " + strings.TrimSpace(string(b)) + "\n")
	}
	if err := os.WriteFile(f.signers, []byte(sig.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	run(t, dir, "git", "init", "-q", "--bare", f.origin)
	run(t, dir, "git", "init", "-q", f.host)
	commit(t, f.host, "README", "hello\n")
	run(t, f.host, "git", "remote", "add", "origin", f.origin)
	run(t, f.host, "git", "push", "-q", "origin", "main")
	run(t, f.host, "git", "remote", "set-head", "origin", "main")
	run(t, dir, "git", "clone", "-q", f.host, f.guest)
	// VM と同じく、guest のコミットは捨て鍵で署名する
	run(t, f.guest, "git", "config", "user.signingkey", f.markKey+".pub")

	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	mark, _ := os.ReadFile(f.markKey + ".pub")
	f.p = &Publisher{Repo: f.host, Work: work, GuestURL: f.guest, Protected: DefaultProtected,
		MarkPub: strings.TrimSpace(string(mark))}
	return f
}

func TestPublishResignsVMCommitsIncrementally(t *testing.T) {
	f := newFixture(t)
	run(t, f.guest, "git", "switch", "-q", "-c", "feature")
	commit(t, f.guest, "a.txt", "a\n")
	commit(t, f.guest, "b.txt", "b\n")

	res, err := f.p.Publish(Request{Branch: "feature", Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Signed != 2 || res.Base != "main" {
		t.Fatalf("unexpected %+v", res)
	}
	if run(t, f.origin, "git", "rev-parse", "refs/heads/feature") != res.Head {
		t.Fatal("push された先端が結果と違う")
	}
	hostFP := fingerprint(t, f.hostKey+".pub")
	for _, rev := range []string{"feature", "feature~1"} {
		if got := sigFP(t, f.origin, rev, f.signers); got != hostFP {
			t.Fatalf("%s が利用者の鍵で署名されていない: %q", rev, got)
		}
	}
	if run(t, f.origin, "git", "rev-parse", "feature~2") != run(t, f.host, "git", "rev-parse", "main") {
		t.Fatal("base のコミットが書き換わった")
	}
	if run(t, f.origin, "git", "rev-parse", "feature^{tree}") != run(t, f.guest, "git", "rev-parse", "feature^{tree}") {
		t.Fatal("中身が変わった")
	}
	if got := run(t, f.origin, "git", "log", "-1", "--format=%s|%an|%cn", "feature"); got != "edit b.txt|Host User|Host User" {
		t.Fatalf("メッセージ・author・committer が違う: %q", got)
	}

	// 追加のコミットだけが署名され、既に push したコミットは書き換わらない
	commit(t, f.guest, "c.txt", "c\n")
	res2, err := f.p.Publish(Request{Branch: "feature", Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Signed != 1 || run(t, f.origin, "git", "rev-parse", "feature~1") != res.Head {
		t.Fatalf("追加分だけの署名になっていない: %+v", res2)
	}

	// 変化がなければ何もしない
	res3, err := f.p.Publish(Request{Branch: "feature", Title: "t"})
	if err != nil || res3.Signed != 0 || res3.Head != res2.Head {
		t.Fatalf("変化なしで何かした: %+v %v", res3, err)
	}
}

// host の repo だけに書いた署名の設定 (.git/config や includeIf) で署名し直す。
func TestPublishUsesRepoSigningConfig(t *testing.T) {
	f := newFixture(t)
	repoKey := filepath.Join(t.TempDir(), "repokey")
	keygen(t, repoKey)
	run(t, f.host, "git", "config", "user.signingkey", repoKey+".pub")
	run(t, f.host, "git", "config", "gpg.ssh.allowedSignersFile", "/nonexistent") // bare repo の設定を上書きしない
	run(t, f.guest, "git", "switch", "-q", "-c", "feature")
	commit(t, f.guest, "a.txt", "a\n")

	if _, err := f.p.Publish(Request{Branch: "feature", Title: "t"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(repoKey + ".pub")
	signers := filepath.Join(t.TempDir(), "signers")
	if err := os.WriteFile(signers, []byte("* "+strings.TrimSpace(string(b))+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := sigFP(t, f.origin, "feature", signers), fingerprint(t, repoKey+".pub"); got != want {
		t.Fatalf("repo の鍵で署名されていない: %q (want %q)", got, want)
	}
}

func TestPublishKeepsOthersCommits(t *testing.T) {
	f := newFixture(t)
	// 同僚のブランチ: 別の鍵で署名されたコミットと、署名の無いコミット (host にある)
	colleague := filepath.Join(t.TempDir(), "colleague")
	keygen(t, colleague)
	run(t, f.host, "git", "switch", "-q", "-c", "colleague")
	commit(t, f.host, "c1.txt", "1\n", "-c", "user.name=Colleague", "-c", "user.signingkey="+colleague+".pub")
	commit(t, f.host, "c2.txt", "2\n", "-c", "user.name=Colleague", "-c", "commit.gpgsign=false")
	colleagueTip := run(t, f.host, "git", "rev-parse", "colleague")
	colleagueSig := run(t, f.host, "git", "cat-file", "commit", "colleague~1")

	// VM は同僚のブランチの上に積む
	run(t, f.guest, "git", "fetch", "-q", "origin", "colleague:colleague")
	run(t, f.guest, "git", "switch", "-q", "-c", "on-colleague", "colleague")
	commit(t, f.guest, "mine.txt", "m\n")

	res, err := f.p.Publish(Request{Branch: "on-colleague", Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Signed != 1 {
		t.Fatalf("VM のコミットだけを署名し直すはず: %+v", res)
	}
	if run(t, f.origin, "git", "rev-parse", "on-colleague~1") != colleagueTip {
		t.Fatal("同僚のコミットのハッシュが変わった")
	}
	if run(t, f.origin, "git", "cat-file", "commit", "on-colleague~2") != colleagueSig {
		t.Fatal("同僚のコミットの署名が書き換わった")
	}
}

func TestPublishRejectsUnmarkedVMCommit(t *testing.T) {
	f := newFixture(t)
	run(t, f.guest, "git", "switch", "-q", "-c", "feature")
	commit(t, f.guest, "a.txt", "a\n", "-c", "commit.gpgsign=false") // VM で署名を切った
	if _, err := f.p.Publish(Request{Branch: "feature", Title: "t"}); err == nil {
		t.Fatal("印の無い VM のコミットが通った")
	}
	if out, _ := exec.Command("git", "-C", f.origin, "rev-parse", "--verify", "-q", "feature").Output(); len(out) > 0 {
		t.Fatal("push されてしまった")
	}

	// 別の鍵 (捨て鍵でも利用者の鍵でもない) で署名されたものも VM の印とはみなさない
	other := filepath.Join(t.TempDir(), "other")
	keygen(t, other)
	run(t, f.guest, "git", "switch", "-q", "-c", "feature2", "main")
	commit(t, f.guest, "b.txt", "b\n", "-c", "user.signingkey="+other+".pub")
	if _, err := f.p.Publish(Request{Branch: "feature2", Title: "t"}); err == nil {
		t.Fatal("別の鍵の署名を VM の印とみなした")
	}
}

func TestPublishRejectsProtectedAndExistingBranches(t *testing.T) {
	f := newFixture(t)
	commit(t, f.guest, "x.txt", "x\n")
	if _, err := f.p.Publish(Request{Branch: "main", Title: "t"}); err == nil {
		t.Fatal("保護ブランチへの push が通った")
	}
	if _, err := f.p.Publish(Request{Branch: "--force", Title: "t"}); err == nil {
		t.Fatal("オプションに見えるブランチ名が通った")
	}

	// 利用者が作った同名のブランチを上書きしない
	run(t, f.host, "git", "push", "-q", "origin", "main:refs/heads/users-branch")
	before := run(t, f.origin, "git", "rev-parse", "users-branch")
	run(t, f.guest, "git", "switch", "-q", "-c", "users-branch")
	commit(t, f.guest, "y.txt", "y\n")
	if _, err := f.p.Publish(Request{Branch: "users-branch", Title: "t"}); err == nil {
		t.Fatal("既存のリモートブランチを上書きした")
	}
	if run(t, f.origin, "git", "rev-parse", "users-branch") != before {
		t.Fatal("既存のリモートブランチが変わった")
	}
}

func TestPublishIgnoresGuestHooks(t *testing.T) {
	f := newFixture(t)
	marker := filepath.Join(t.TempDir(), "pwned")
	hook := filepath.Join(f.guest, ".git", "hooks", "post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, f.guest, "git", "switch", "-q", "-c", "feature")
	_ = os.Remove(marker) // guest 自身の switch で作られた分は消す
	commit(t, f.guest, "a.txt", "a\n")
	if _, err := f.p.Publish(Request{Branch: "feature", Title: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("guest の hook が host で実行された")
	}
}

func TestPublishLimits(t *testing.T) {
	f := newFixture(t)
	run(t, f.guest, "git", "switch", "-q", "-c", "feature")
	commit(t, f.guest, "a.txt", "a\n")
	if _, err := f.p.Publish(Request{Branch: "feature", Title: strings.Repeat("あ", maxTitleRunes+1)}); err == nil {
		t.Fatal("長すぎるタイトルが通った")
	}
	f.p.MinInterval = time.Hour
	if _, err := f.p.Publish(Request{Branch: "feature", Title: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.p.Publish(Request{Branch: "feature", Title: "t"}); err == nil {
		t.Fatal("間隔の制限が効かない")
	}
	f.p.MinInterval = 0
	for i := range maxBranches + 1 {
		b := fmt.Sprintf("b%d", i)
		run(t, f.guest, "git", "switch", "-q", "-c", b, "feature")
		_, err := f.p.Publish(Request{Branch: b, Title: "t"})
		if i+1 < maxBranches && err != nil {
			t.Fatalf("%s: %v", b, err)
		}
		if i+1 > maxBranches && err == nil {
			t.Fatal("ブランチ数の上限が効かない")
		}
	}
}

// 承認制のときは、承認が拒否された依頼では push も PR の作成もしない。
func TestPublishApprovalGatesPush(t *testing.T) {
	f := newFixture(t)
	run(t, f.guest, "git", "switch", "-q", "-c", "feature")
	commit(t, f.guest, "a.txt", "a\n")

	var got Request
	calls := 0
	f.p.Approve = func(req Request) error {
		calls++
		got = req
		return fmt.Errorf("承認されなかった")
	}
	if _, err := f.p.Publish(Request{Branch: "feature", Title: "t", Body: "b"}); err == nil {
		t.Fatal("承認が拒否されたのに push された")
	}
	if calls != 1 {
		t.Fatalf("承認は 1 回のはず: %d", calls)
	}
	if got.Branch != "feature" || got.Base != "main" || got.Body != "b" {
		t.Fatalf("承認に渡す内容が違う: %+v", got)
	}
	if out, _ := exec.Command("git", "-C", f.origin, "rev-parse", "--verify", "-q", "feature").Output(); len(out) > 0 {
		t.Fatal("承認の前に push された")
	}

	// 承認すれば push される
	f.p.Approve = func(Request) error { return nil }
	res, err := f.p.Publish(Request{Branch: "feature", Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Signed != 1 {
		t.Fatalf("承認後の push がおかしい: %+v", res)
	}
}

// 別の run でも、quagent が前に push したブランチに追加コミットを積める。
// 前に push したコミットは内容で照合して再利用するので履歴は書き換わらない。
func TestPublishUpdatesExistingBranch(t *testing.T) {
	f := newFixture(t)
	run(t, f.guest, "git", "switch", "-q", "-c", "feature")
	commit(t, f.guest, "a.txt", "a\n")
	res1, err := f.p.Publish(Request{Branch: "feature", Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if res1.Signed != 1 {
		t.Fatalf("1 回目: %+v", res1)
	}

	// 新しい run (bare repo も別) から同じ guest ブランチに積む
	p2 := &Publisher{Repo: f.host, Work: t.TempDir(), GuestURL: f.guest,
		Protected: DefaultProtected, MarkPub: f.p.MarkPub}
	commit(t, f.guest, "b.txt", "b\n")
	res2, err := p2.Publish(Request{Branch: "feature", Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Signed != 1 {
		t.Fatalf("追加分だけのはず: %+v", res2)
	}
	if run(t, f.origin, "git", "rev-parse", "feature~1") != res1.Head {
		t.Fatal("既に push したコミットが書き換わった")
	}
	if run(t, f.origin, "git", "rev-parse", "feature") != res2.Head {
		t.Fatal("push された先端が結果と違う")
	}

	// 変化がなければ何もしない
	res3, err := p2.Publish(Request{Branch: "feature", Title: "t"})
	if err != nil || res3.Signed != 0 || res3.Head != res2.Head {
		t.Fatalf("変化なしで何かした: %+v %v", res3, err)
	}
}

// guest と対応しないコミットがある既存ブランチは書き換えない。
func TestPublishRejectsForeignExistingBranch(t *testing.T) {
	f := newFixture(t)
	// 利用者が other ブランチにコミットして push 済み
	run(t, f.host, "git", "switch", "-q", "-c", "other", "main")
	commit(t, f.host, "o.txt", "o\n")
	run(t, f.host, "git", "push", "-q", "origin", "other")
	before := run(t, f.origin, "git", "rev-parse", "other")

	// guest は別の中身で同名のブランチを作る
	run(t, f.guest, "git", "switch", "-q", "-c", "other", "main")
	commit(t, f.guest, "z.txt", "z\n")
	if _, err := f.p.Publish(Request{Branch: "other", Title: "t"}); err == nil {
		t.Fatal("対応しないコミットがあるブランチを書き換えた")
	}
	if run(t, f.origin, "git", "rev-parse", "other") != before {
		t.Fatal("他人のブランチが変わった")
	}
}

func TestCheckBranch(t *testing.T) {
	p := &Publisher{
		Work:      t.TempDir(),
		Protected: []string{"main", "master"},
	}
	// 正常系
	if err := p.CheckBranch("feature/branch-1"); err != nil {
		t.Errorf("CheckBranch(feature/branch-1) = %v", err)
	}

	// 空文字
	if err := p.CheckBranch(""); err == nil {
		t.Error("expected error for empty branch name")
	}

	// - 始まり
	if err := p.CheckBranch("-b"); err == nil {
		t.Error("expected error for branch starting with -")
	}

	// 保護ブランチ
	if err := p.CheckBranch("main"); err == nil {
		t.Error("expected error for protected branch main")
	}
}

func TestLastLine(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"single line", "single line"},
		{"line1\nline2", "line2"},
		{"line1\nline2\n", "line2"},
		{"line1\n  line2", "  line2"},
	}
	for _, c := range cases {
		if got := lastLine(c.in); got != c.want {
			t.Errorf("lastLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFinishWithGH(t *testing.T) {
	p := &Publisher{
		Repo: t.TempDir(),
	}

	// 1. 既存のOPENなPRがある場合
	p.GH = func(dir string, args ...string) ([]byte, error) {
		if args[0] == "pr" && args[1] == "view" {
			return []byte("https://github.com/org/repo/pull/123\n"), nil
		}
		return nil, fmt.Errorf("unexpected args: %v", args)
	}
	res, err := p.finish(Request{Branch: "feature"}, "main", "commit1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.URL != "https://github.com/org/repo/pull/123" || res.Created {
		t.Fatalf("unexpected res: %+v", res)
	}

	// 2. 新規PR作成の場合
	p.GH = func(dir string, args ...string) ([]byte, error) {
		if args[0] == "pr" && args[1] == "view" {
			return nil, fmt.Errorf("no pr")
		}
		if args[0] == "pr" && args[1] == "create" {
			return []byte("Creating pull request...\nhttps://github.com/org/repo/pull/456\n"), nil
		}
		return nil, fmt.Errorf("unexpected args: %v", args)
	}
	res2, err := p.finish(Request{Branch: "new-feature", Title: "title", Body: "body"}, "main", "commit2", 1)
	if err != nil {
		t.Fatal(err)
	}
	if res2.URL != "https://github.com/org/repo/pull/456" || !res2.Created {
		t.Fatalf("unexpected res: %+v", res2)
	}
}

// git 呼び出し時に transfer.fsckObjects=true が有効になっていることを確かめる。
func TestGitIOTransferFsckObjects(t *testing.T) {
	p := &Publisher{}
	dir := t.TempDir()
	out, err := p.git(dir, "config", "transfer.fsckObjects")
	if err != nil {
		t.Fatalf("git config transfer.fsckObjects: %v", err)
	}
	if out != "true" {
		t.Fatalf("transfer.fsckObjects = %q, want true", out)
	}
}

