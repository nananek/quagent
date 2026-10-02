package pr

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// テスト用の git 環境: 署名はテスト用の ssh 鍵で行い、利用者の設定は読まない。
func setupEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	key := filepath.Join(dir, "signkey")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	cfg := filepath.Join(dir, "gitconfig")
	content := "[user]\n\tname = Host User\n\temail = host@example.com\n\tsigningkey = " + key + ".pub\n" +
		"[gpg]\n\tformat = ssh\n[init]\n\tdefaultBranch = main\n"
	if err := os.WriteFile(cfg, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return dir
}

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

func commit(t *testing.T, dir, file, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "add", file)
	run(t, dir, "git", "-c", "user.name=quagent", "-c", "user.email=quagent@localhost",
		"-c", "commit.gpgsign=false", "commit", "-q", "-m", "edit "+file)
}

func isSigned(t *testing.T, repo, rev string) bool {
	return strings.Contains(run(t, repo, "git", "cat-file", "commit", rev), "\ngpgsig ")
}

type fixture struct {
	origin, host, guest string
	p                   *Publisher
}

func newFixture(t *testing.T) fixture {
	dir := setupEnv(t)
	f := fixture{
		origin: filepath.Join(dir, "origin.git"),
		host:   filepath.Join(dir, "host"),
		guest:  filepath.Join(dir, "guest"),
	}
	run(t, dir, "git", "init", "-q", "--bare", f.origin)
	run(t, dir, "git", "init", "-q", f.host)
	commit(t, f.host, "README", "hello\n")
	run(t, f.host, "git", "remote", "add", "origin", f.origin)
	run(t, f.host, "git", "push", "-q", "origin", "main")
	run(t, f.host, "git", "remote", "set-head", "origin", "main")
	run(t, dir, "git", "clone", "-q", f.host, f.guest)
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	f.p = &Publisher{Repo: f.host, Work: work, GuestURL: f.guest, SSHCmd: "ssh", Protected: DefaultProtected}
	return f
}

func TestPublishSignsAndUpdatesIncrementally(t *testing.T) {
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
	remote := run(t, f.origin, "git", "rev-parse", "refs/heads/feature")
	if remote != res.Head {
		t.Fatal("push された先端が結果と違う")
	}
	for _, rev := range []string{"feature", "feature~1"} {
		if !isSigned(t, f.origin, rev) {
			t.Fatalf("%s が署名されていない", rev)
		}
	}
	if isSigned(t, f.origin, "feature~2") {
		t.Fatal("base のコミットまで署名し直している")
	}
	if run(t, f.origin, "git", "rev-parse", "feature^{tree}") != run(t, f.guest, "git", "rev-parse", "feature^{tree}") {
		t.Fatal("中身が変わった")
	}

	// 追加のコミットだけが署名され、既に push したコミットは書き換わらない
	commit(t, f.guest, "c.txt", "c\n")
	res2, err := f.p.Publish(Request{Branch: "feature", Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Signed != 1 {
		t.Fatalf("追加分だけ署名されていない: %+v", res2)
	}
	if run(t, f.origin, "git", "rev-parse", "feature~1") != res.Head {
		t.Fatal("既に push したコミットが書き換わった")
	}

	// 変化がなければ何もしない
	res3, err := f.p.Publish(Request{Branch: "feature", Title: "t"})
	if err != nil || res3.Signed != 0 || res3.Head != res2.Head {
		t.Fatalf("変化なしで何かした: %+v %v", res3, err)
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
