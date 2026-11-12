package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenRegularNoFollowRejectsSwappedSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("benign"), 0o644); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	info, err := os.Lstat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(victim); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, victim); err != nil {
		t.Fatal(err)
	}
	if f, err := openRegularNoFollow(root, "victim", info); err == nil {
		f.Close()
		t.Fatal("差し替えられたシンボリックリンクを開いた")
	}
}
