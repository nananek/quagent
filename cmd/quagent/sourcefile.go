package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// openDirNoSymlink は dir を、最後の要素がシンボリックリンクでない実ディレクトリとして
// 開く。親ディレクトリを root にして OpenRoot するので、dir が親の外を指すリンクなら
// 失敗する。host の .tmp やスキルディレクトリを、リンク越しに別の場所として使わない。
func openDirNoSymlink(dir string) (*os.Root, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	parent, base := filepath.Split(filepath.Clean(abs))
	if base == "" || base == "." || base == string(filepath.Separator) {
		return nil, fmt.Errorf("ディレクトリの指定が不正: %s", dir)
	}
	parentRoot, err := os.OpenRoot(parent)
	if err != nil {
		return nil, err
	}
	defer parentRoot.Close()
	fi, err := parentRoot.Lstat(base)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s がシンボリックリンクなので受け渡さない", dir)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s がディレクトリではない", dir)
	}
	return parentRoot.OpenRoot(base)
}

// ensureRealDir は path を実ディレクトリとして用意する。既にあるシンボリックリンクは
// 辿らずにエラーにする。作成と確認のあいだに差し替えられても、Lstat でリンクを拒否する。
func ensureRealDir(path string) error {
	fi, err := os.Lstat(path)
	if err == nil {
		return checkRealDir(path, fi)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Mkdir(path, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	fi, err = os.Lstat(path)
	if err != nil {
		return err
	}
	return checkRealDir(path, fi)
}

func checkRealDir(path string, fi os.FileInfo) error {
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s がシンボリックリンクなので受け渡さない", path)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s がディレクトリではない", path)
	}
	return nil
}

// openRegularNoFollow は root 配下の通常ファイルを、WalkDir が見たものと同じ実体として
// 開く。Info() から open までのあいだにシンボリックリンクへ差し替えられても、リンク先は
// 読まない。開いた実体が別のファイルでも SameFile で検知する。
func openRegularNoFollow(root *os.Root, rel string, want fs.FileInfo) (*os.File, error) {
	f, err := root.OpenFile(rel, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	got, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !got.Mode().IsRegular() || !os.SameFile(want, got) {
		f.Close()
		return nil, fmt.Errorf("コピー中にファイルが差し替えられた: %s", rel)
	}
	return f, nil
}
