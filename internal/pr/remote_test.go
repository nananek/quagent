package pr

import (
	"os/exec"
	"testing"
)

func TestRemoteHost(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "https://git.example.com/org/repo.git", want: "git.example.com"},
		{in: "http://git.example.com:3000/org/repo", want: "git.example.com"},
		{in: "ssh://git@git.example.com:2222/org/repo.git", want: "git.example.com"},
		{in: "git@git.example.com:org/repo.git", want: "git.example.com"},
		{in: "GIT@Git.Example.COM:org/repo.git", want: "git.example.com"},
		{in: "https://example.com/gitea/org/repo.git", want: "example.com"},
		{in: "git@[2001:db8::1]:org/repo.git", want: "2001:db8::1"},
		{in: "ssh://git@[2001:db8::1]:2222/org/repo.git", want: "2001:db8::1"},
		{in: "git@git.example.com:repo.git", want: "git.example.com"},
		{in: "", wantErr: true},
		{in: "file:///srv/git/org/repo.git", wantErr: true},
		{in: "/srv/git/org/repo.git", wantErr: true},
		{in: "git@:org/repo.git", wantErr: true},
	}
	for _, c := range cases {
		got, err := remoteHost(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("remoteHost(%q) = %q; want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("remoteHost(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("remoteHost(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

func TestRemoteHostFromOrigin(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", dir},
		{"-C", dir, "remote", "add", "origin", "ssh://git@Git.Example.com:2222/org/repo.git"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	got, err := RemoteHost(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "git.example.com" {
		t.Errorf("RemoteHost() = %q; want git.example.com", got)
	}

	// origin が無ければ分かりやすく失敗する
	bare := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", bare).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if _, err := RemoteHost(bare); err == nil {
		t.Error("origin の無い repo でエラーにならなかった")
	}
}
