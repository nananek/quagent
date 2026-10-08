package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// --mount-tmp: host の <repo>/.tmp と VM の /work/.tmp の受け渡し。
//
// VM には専用の小さなディスクを /work/.tmp として見せる (host のディレクトリは
// 直接見せない)。置き場の容量はディスクの大きさで頭打ちになり、VM が作った
// リンクや特殊ファイル・実行権限が host に現れることもない。起動時に host の
// .tmp の通常ファイルを VM へコピーし、終了時に VM の中身を host へ回収する。
const (
	tmpSerial = "quagent-tmp"
	// tmpDiskSize は VM の /work/.tmp の容量。レポートなどのテキストを置く想定。
	tmpDiskSize = 64 << 20
	// maxTmpIn は起動時に VM へ渡す host の .tmp の合計の上限 (ディスクに収まるよう)。
	maxTmpIn = 32 << 20
	// maxTmpOut は回収で読む tar の上限 (疎なファイルで見かけだけ大きくされても止まる)。
	maxTmpOut = 2 * tmpDiskSize
)

// prepareTmp は host の .tmp を用意し、VM に見せる空のディスクを work に作る。
func prepareTmp(repo, work string) (dir, img string, err error) {
	dir = filepath.Join(repo, ".tmp")
	// git で管理しているファイルがあると、/work の checkout とディスクが重なる
	if out, _ := exec.Command("git", "-C", repo, "ls-files", "--", ".tmp").Output(); len(strings.TrimSpace(string(out))) > 0 {
		return "", "", fmt.Errorf(".tmp に git で管理しているファイルがあるので受け渡さない")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	if exec.Command("git", "-C", repo, "check-ignore", "-q", ".tmp/").Run() != nil {
		logf("注意: %s は .gitignore されていない", dir)
	}
	img = filepath.Join(work, "tmp.img")
	f, err := os.OpenFile(img, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	if err := f.Truncate(tmpDiskSize); err != nil {
		return "", "", err
	}
	return dir, img, nil
}

// tmpRuncmd はディスクを VM の /work/.tmp にする cloud-init の runcmd (受け口の起動前)。
// 置くのはテキストの想定なので、実行・setuid・デバイスファイルは使えないようにする。
// デバイスの出現を待ってから mkfs する (udev が遅れると mkfs が失敗し、
// mkdir ごと実行されず /work/.tmp が無いまま進んでしまうため)。最後に
// /proc/mounts でマウントを確かめ、失敗なら cloud-init を失敗させる。
func tmpRuncmd(uid string) string {
	dev := "/dev/disk/by-id/virtio-" + tmpSerial
	return fmt.Sprintf("  - [sh, -c, \"mkdir -p /work/.tmp && dev=%s && for i in $(seq 1 30); do [ -b $dev ] && break; sleep 1; done && mkfs.ext4 -q -L %s $dev && mount -o nosuid,nodev,noexec $dev /work/.tmp && rmdir /work/.tmp/lost+found && chown %s:%s /work/.tmp && grep -q ' /work/.tmp ' /proc/mounts\"]\n",
		dev, tmpSerial, uid, uid)
}

// copyInTmp は host の .tmp の通常ファイルとディレクトリを VM の /work/.tmp へ渡す。
// リンクなどは渡さない。マウントされていない plain のディレクトリに書くと
// 隔離 (noexec 等) や容量制限が効かないので、先にマウントを確かめる。
func copyInTmp(g vmGuest, dir string) error {
	if out, err := g.sh("grep -q ' /work/.tmp ' /proc/mounts", nil); err != nil {
		return fmt.Errorf("/work/.tmp が VM でマウントされていない (cloud-init の準備に失敗): %v: %s\nVM 内の診断:\n%s", err, out, diagTmpMount(g))
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	var total int64
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: filepath.ToSlash(rel) + "/", Mode: 0o755, ModTime: info.ModTime()})
		case info.Mode().IsRegular():
			if total += info.Size(); total > maxTmpIn {
				return fmt.Errorf("%s が大きすぎる (VM へ渡すのは合計 %d MiB まで)", dir, maxTmpIn>>20)
			}
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: filepath.ToSlash(rel), Mode: 0o644, Size: info.Size(), ModTime: info.ModTime()}); err != nil {
				return err
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.CopyN(tw, f, info.Size())
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	var out bytes.Buffer
	if err := g.stream("tar -x -C /work/.tmp --no-same-owner --no-same-permissions", &buf, &out, &out); err != nil {
		return fmt.Errorf(".tmp を VM へ渡せない: %v: %s", err, out.String())
	}
	return nil
}

// diagTmpMount は /work/.tmp のマウント失敗時に VM 内の手がかりを集める
// (読み取り専用の確認コマンドのみ)。
func diagTmpMount(g vmGuest) string {
	out, _ := g.sh("echo '--- cloud-init status:'; cloud-init status --long 2>&1 | head -n 20; echo '--- lsblk:'; lsblk -o NAME,SIZE,TYPE,MOUNTPOINTS,SERIAL 2>&1; echo '--- by-id:'; ls -l /dev/disk/by-id/ 2>&1; echo '--- work:'; ls -ld /work /work/.tmp 2>&1; df -h /work/.tmp 2>&1; echo '--- mounts:'; grep -E 'work|vdb|vdc' /proc/mounts 2>&1; echo '--- cloud-init tmp:'; grep -a -i 'tmp' /var/log/cloud-init-output.log 2>&1 | tail -n 20", nil)
	return string(out)
}

// collectTmp は VM の /work/.tmp を host の .tmp へ回収する。
func collectTmp(g vmGuest, dir string) (files int, size int64, err error) {
	pr, pw := io.Pipe()
	var stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		err := g.stream("tar -c -C /work/.tmp .", nil, pw, &stderr)
		pw.CloseWithError(err)
		done <- err
	}()
	lr := &io.LimitedReader{R: pr, N: maxTmpOut}
	files, size, err = extractTar(lr, dir)
	if err != nil && lr.N <= 0 {
		err = fmt.Errorf("回収する量が上限 (%d MiB) を超えた", maxTmpOut>>20)
	}
	pr.CloseWithError(errors.New("回収をやめた"))
	if gerr := <-done; err == nil && gerr != nil {
		err = fmt.Errorf("VM で tar に失敗: %v: %s", gerr, stderr.String())
	}
	return files, size, err
}

// extractTar は VM が作った tar を dir に展開する。VM は信用しないので、通常ファイル
// (0644) とディレクトリ (0755) だけを dir の中に作り、リンク・特殊ファイルは捨てる。
// host 側にリンクなど通常ファイル以外が同じ名前であれば上書きしない。
func extractTar(r io.Reader, dir string) (files int, size int64, err error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0, 0, err
	}
	defer root.Close()
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files, size, nil
		}
		if err != nil {
			return files, size, err
		}
		name := filepath.Clean(filepath.FromSlash(h.Name))
		if name == "." {
			continue
		}
		if !filepath.IsLocal(name) {
			return files, size, fmt.Errorf("不正な名前: %q", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o755); err != nil {
				return files, size, err
			}
		case tar.TypeReg:
			if fi, err := root.Lstat(name); err == nil && !fi.Mode().IsRegular() {
				logf(".tmp/%s は host で通常ファイルではないので上書きしない", name)
				continue
			}
			if d := filepath.Dir(name); d != "." {
				if err := root.MkdirAll(d, 0o755); err != nil {
					return files, size, err
				}
			}
			tmp := name + ".quagent-part"
			f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
			if err != nil {
				return files, size, err
			}
			n, err := io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err == nil {
				err = root.Chtimes(tmp, h.ModTime, h.ModTime)
			}
			if err == nil {
				err = root.Rename(tmp, name)
			}
			if err != nil {
				_ = root.Remove(tmp)
				return files, size, err
			}
			files++
			size += n
		default:
			// シンボリックリンク・ハードリンク・デバイスなどは host に作らない
		}
	}
}
