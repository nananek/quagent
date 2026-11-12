package main

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"10G", 10 << 30, true},
		{"10GiB", 10 << 30, true},
		{"512M", 512 << 20, true},
		{"1024K", 1 << 20, true},
		{"1T", 1 << 40, true},
		{"123", 123, true},
		{"0", 0, true},
		{"", 0, false},
		{"G", 0, false},
		{"10X", 0, false},
		{"-1G", 0, false},
	}
	for _, c := range cases {
		got, err := parseSize(c.in)
		if (err == nil) != c.ok {
			t.Errorf("parseSize(%q) error = %v, want ok=%v", c.in, err, c.ok)
			continue
		}
		if c.ok && got != c.want {
			t.Errorf("parseSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestWorkDiskSizeAutoAndExplicit(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "dump.sql"), make([]byte, 3<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(src, "files"), 0o755)
	if err := os.WriteFile(filepath.Join(src, "files", "a.bin"), make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}

	size, data, err := workDiskSize(src, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if data != 4<<20 {
		t.Fatalf("data = %d, want %d", data, int64(4<<20))
	}
	if size < minWorkDisk {
		t.Fatalf("auto size = %d, want >= %d", size, int64(minWorkDisk))
	}

	size, _, err = workDiskSize(src, "8G", false)
	if err != nil || size != 8<<30 {
		t.Fatalf("explicit size = %d, err = %v", size, err)
	}

	if _, _, err := workDiskSize(src, "1M", false); err == nil {
		t.Fatal("コピー元より小さい --work-size が通った")
	}
}

func TestWorkDiskSizeSkipsTmp(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "normal"), make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(src, ".tmp"), 0o755)
	if err := os.WriteFile(filepath.Join(src, ".tmp", "big"), make([]byte, 10<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	_, data, err := workDiskSize(src, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if data != 1<<20 {
		t.Fatalf("data = %d, want %d (.tmp を数えている)", data, int64(1<<20))
	}
}

func TestWriteTarDir(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "plain.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("plain.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, ".tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".tmp", "skip"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	tw := tar.NewWriter(&buf)
	var stats copyStats
	if err := writeTarDir(tw, src, true, &stats); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	tr := tar.NewReader(strings.NewReader(buf.String()))
	got := map[string]*tar.Header{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got[h.Name] = h
	}
	if _, ok := got[".tmp/"]; ok {
		t.Fatal(".tmp が tar に入っている")
	}
	if h := got["sub/run.sh"]; h == nil || h.Mode&0o111 == 0 {
		t.Fatalf("実行ビットが落ちている: %+v", h)
	}
	if h := got["plain.txt"]; h == nil || h.Mode != 0o600 {
		t.Fatalf("plain.txt のモードが違う: %+v", h)
	}
	if h := got["link"]; h == nil || h.Typeflag != tar.TypeSymlink || h.Linkname != "plain.txt" {
		t.Fatalf("シンボリックリンクが違う: %+v", h)
	}
	if stats.Files != 2 || stats.Links != 1 || stats.Dirs < 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestWriteTarDirExtractsWithTar(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar が無い")
	}
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "d", "x"), []byte("data"), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	tw := tar.NewWriter(&buf)
	var stats copyStats
	if err := writeTarDir(tw, src, false, &stats); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	cmd := exec.Command("tar", "-x", "-C", dst, "--no-same-owner", "--no-same-permissions")
	cmd.Stdin = strings.NewReader(buf.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tar -x: %v: %s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(dst, "d", "x"))
	if err != nil || string(b) != "data" {
		t.Fatalf("展開結果 = %q, err = %v", b, err)
	}
	fi, err := os.Stat(filepath.Join(dst, "d", "x"))
	if err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("実行ビットが落ちている: %v, %v", fi, err)
	}
}

type swapOnWrite func([]byte) (int, error)

func (f swapOnWrite) Write(p []byte) (int, error) { return f(p) }

// TestWriteTarDirRejectsSwappedSymlink は、WalkDir の Info() のあとで通常ファイルを
// シンボリックリンクに差し替えても、リンク先のホスト側ファイルを読まないことを確かめる。
func TestWriteTarDirRejectsSwappedSymlink(t *testing.T) {
	src := t.TempDir()
	victim := filepath.Join(src, "victim")
	if err := os.WriteFile(victim, []byte("benign"), 0o644); err != nil {
		t.Fatal(err)
	}
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "secret")
	if err := os.WriteFile(secret, []byte("TOPSECRET"), 0o600); err != nil {
		t.Fatal(err)
	}

	swapped := false
	swap := swapOnWrite(func(p []byte) (int, error) {
		if !swapped {
			swapped = true
			_ = os.Remove(victim)
			_ = os.Symlink(secret, victim)
		}
		return len(p), nil
	})

	var archive bytes.Buffer
	tw := tar.NewWriter(io.MultiWriter(&archive, swap))
	var stats copyStats
	if err := writeTarDir(tw, src, false, &stats); err != nil {
		return // 差し替えを検知して止まるのが正しい
	}
	if err := tw.Close(); err != nil {
		return
	}
	if strings.Contains(archive.String(), "TOPSEC") {
		t.Fatalf("差し替えられたシンボリックリンクを辿り、%s の内容を tar に含めた", secret)
	}
}

func TestWorkRuncmd(t *testing.T) {
	s := workRuncmd("1000")
	for _, want := range []string{workSerial, "mkfs.ext4", "mount", "/work", "chown 1000:1000"} {
		if !strings.Contains(s, want) {
			t.Errorf("workRuncmd に %q が無い: %s", want, s)
		}
	}
}

func TestCopySource(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	src := t.TempDir()
	got, err := copySource(src)
	if err != nil {
		t.Fatal(err)
	}
	if got != src {
		t.Fatalf("copySource = %q, want %q", got, src)
	}

	file := filepath.Join(src, "f")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := copySource(file); err == nil {
		t.Fatal("ファイルがコピー元として通った")
	}
}
