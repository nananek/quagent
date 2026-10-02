package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func makeTar(t *testing.T, entries ...tar.Header) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range entries {
		body := []byte("x")
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			_, _ = tw.Write(body)
		}
	}
	_ = tw.Close()
	return &buf
}

func TestExtractTarKeepsOnlyPlainFiles(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	// host 側に既にあるリンクは上書きしない (たどって外へ書かない)
	if err := os.Symlink(secret, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	tr := makeTar(t,
		tar.Header{Typeflag: tar.TypeDir, Name: "./sub/", Mode: 0o777},
		tar.Header{Typeflag: tar.TypeReg, Name: "./sub/report.md", Mode: 0o4755},
		tar.Header{Typeflag: tar.TypeSymlink, Name: "./evil", Linkname: secret},
		tar.Header{Typeflag: tar.TypeReg, Name: "./link", Mode: 0o644},
		tar.Header{Typeflag: tar.TypeChar, Name: "./dev", Devmajor: 1, Devminor: 3},
	)
	files, _, err := extractTar(tr, dir)
	if err != nil || files != 1 {
		t.Fatalf("files=%d err=%v", files, err)
	}
	fi, err := os.Stat(filepath.Join(dir, "sub", "report.md"))
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("report.md: %v %v", fi, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "evil")); err == nil {
		t.Fatal("シンボリックリンクが作られた")
	}
	if _, err := os.Lstat(filepath.Join(dir, "dev")); err == nil {
		t.Fatal("デバイスファイルが作られた")
	}
	if b, _ := os.ReadFile(secret); string(b) != "s" {
		t.Fatal("リンク先が書き換えられた")
	}
}

func TestExtractTarRejectsEscape(t *testing.T) {
	for _, name := range []string{"../out", "/abs", "a/../../out"} {
		dir := t.TempDir()
		if _, _, err := extractTar(makeTar(t, tar.Header{Typeflag: tar.TypeReg, Name: name}), dir); err == nil {
			t.Errorf("%s を受け付けた", name)
		}
	}
}
