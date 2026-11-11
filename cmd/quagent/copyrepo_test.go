package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRepoWithOrigin は origin に main を push 済みで、ローカルに未 push のコミットが 1 つある repo を作る。
func newRepoWithOrigin(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	origin, repo := filepath.Join(base, "origin.git"), filepath.Join(base, "repo")
	gitT(t, base, "init", "-q", "--bare", "-b", "main", origin)
	gitT(t, base, "init", "-q", "-b", "main", repo)
	gitT(t, repo, "commit", "-q", "--allow-empty", "-m", "pushed")
	gitT(t, repo, "remote", "add", "origin", origin)
	gitT(t, repo, "push", "-q", "-u", "origin", "main")
	gitT(t, repo, "commit", "-q", "--allow-empty", "-m", "local only")
	return repo
}

// newShallowRepo は origin から --depth 1 で clone した shallow repo を作る。
func newShallowRepo(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	origin, full, clone := filepath.Join(base, "origin.git"), filepath.Join(base, "full"), filepath.Join(base, "clone")
	gitT(t, base, "init", "-q", "--bare", "-b", "main", origin)
	gitT(t, base, "init", "-q", "-b", "main", full)
	gitT(t, full, "commit", "-q", "--allow-empty", "-m", "one")
	gitT(t, full, "remote", "add", "origin", origin)
	gitT(t, full, "push", "-q", "-u", "origin", "main")
	gitT(t, full, "commit", "-q", "--allow-empty", "-m", "two")
	gitT(t, full, "push", "-q", "origin", "main")
	gitT(t, base, "clone", "-q", "--depth", "1", "file://"+origin, clone)
	return clone
}

func TestShallowBoundary(t *testing.T) {
	full := newRepoWithOrigin(t)
	if b, err := shallowBoundary(full); err != nil || b != nil {
		t.Fatalf("shallow でない repo: %q, %v", b, err)
	}
	shallow := newShallowRepo(t)
	b, err := shallowBoundary(shallow)
	if err != nil {
		t.Fatal(err)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		t.Fatal("shallow clone の境界を読めなかった")
	}
}

// TestShallowBundleFetch は shallow repo の bundle を、境界をゲスト側に書いた
// ときだけ取り込めることを確かめる。git bundle は shallow の境界を運ばない。
func TestShallowBundleFetch(t *testing.T) {
	repo := newShallowRepo(t)
	bundle := filepath.Join(t.TempDir(), "repo.bundle")
	gitT(t, repo, "bundle", "create", "-q", bundle, "refs/heads/main", "--remotes=origin")
	boundary, err := shallowBoundary(repo)
	if err != nil {
		t.Fatal(err)
	}

	guest := t.TempDir()
	gitT(t, guest, "init", "-q", "-b", "quagent-init")
	if err := os.WriteFile(filepath.Join(guest, ".git", "shallow"), boundary, 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, guest, "fetch", "-q", bundle, "+refs/heads/*:refs/heads/*", "+refs/remotes/*:refs/remotes/*")
	gitT(t, guest, "checkout", "-q", "main")
}

func TestPickHeadDefaultsToUpstream(t *testing.T) {
	repo := newRepoWithOrigin(t)
	src, dst, co, err := pickHead(repo, false)
	if err != nil || src != "refs/remotes/origin/main" || dst != "refs/heads/main" || co != "'main'" {
		t.Fatalf("src=%q dst=%q co=%q err=%v", src, dst, co, err)
	}
	src, _, _, err = pickHead(repo, true)
	if err != nil || src != "refs/heads/main" {
		t.Fatalf("local: src=%q err=%v", src, err)
	}
}

func TestPickHeadNeedsUpstream(t *testing.T) {
	repo := newRepoWithOrigin(t)
	gitT(t, repo, "switch", "-q", "-c", "topic")
	if _, _, _, err := pickHead(repo, false); err == nil {
		t.Fatal("upstream の無いブランチで既定のまま通った")
	}
	if _, _, _, err := pickHead(repo, true); err != nil {
		t.Fatalf("--local-head でも通らない: %v", err)
	}
}

func TestPickHeadDetached(t *testing.T) {
	repo := newRepoWithOrigin(t)
	gitT(t, repo, "switch", "-q", "--detach", "HEAD")
	if _, _, _, err := pickHead(repo, false); err == nil {
		t.Fatal("未 push のコミットの detached HEAD が既定のまま通った")
	}
	gitT(t, repo, "switch", "-q", "--detach", "origin/main")
	if src, _, _, err := pickHead(repo, false); err != nil || src != "HEAD" {
		t.Fatalf("push 済みの detached HEAD: src=%q err=%v", src, err)
	}
}
