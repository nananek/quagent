package main

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/nananek/quagent/internal/paths"
)

// --copy: git ではないディレクトリ (本番コピーや docker-compose 一式) を
// VM の /work へそのまま渡す。host のディレクトリは直接見せず、run ごとの
// 可変サイズの qcow2 ディスクにコピーする。終了時にディスクごと捨てるので、
// 試験中の変更が host の原本へ返ることはない。
const (
	workSerial = "quagent-work"
	// minWorkDisk は自動サイズの下限。空のディレクトリでもツールが動く程度。
	minWorkDisk = 2 << 30
	// maxAutoWorkDisk は自動サイズの上限。これを超えるコピーは --work-size を
	// 明示させる (うっかり巨大な sparse イメージを作らないため)。
	maxAutoWorkDisk = 256 << 30
	// workSlack は自動サイズで上乗せする余裕 (コピー中の tar 展開やログ用)。
	workSlack = 1 << 30
	// workSafetyFree はコピー先ファイルシステムに残す最低空き容量。
	workSafetyFree = 2 << 30
)

// copySource は --copy のコピー元を決める。シンボリックリンクは解決し、
// ディレクトリであることと、run の作業ディレクトリを再帰コピーしないことを確かめる。
func copySource(dir string) (string, error) {
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("コピー元を読めない: %w", err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("コピー元を読めない: %w", err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("コピー元がディレクトリではない: %s", abs)
	}
	if abs == string(filepath.Separator) {
		return "", fmt.Errorf("ルートディレクトリはコピーしない (対象を絞る)")
	}
	if rel, err := filepath.Rel(paths.RunsDir(), abs); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("quagent の作業ディレクトリの中はコピー元にできない: %s", abs)
	}
	return abs, nil
}

// dirUsage はコピー元の見かけの大きさとファイル数。
type dirUsage struct {
	Bytes int64
	Files int
}

// measureDir は src 配下の通常ファイルの大きさを合計する。skipTmp のときは
// 最上位の .tmp (--mount-tmp の受け渡し先) を数えない。
func measureDir(src string, skipTmp bool) (dirUsage, error) {
	var u dirUsage
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if skipTmp && rel == ".tmp" && d.IsDir() {
			return fs.SkipDir
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		u.Bytes += info.Size()
		u.Files++
		return nil
	})
	return u, err
}

// workDiskSize は /work に置くディスクの大きさ (バイト) を決める。spec が空なら
// コピー元の見かけの大きさから余裕を見て自動で決める。spec が指定されたときは
// コピー元が収まることを確かめる。
func workDiskSize(src, spec string, skipTmp bool) (size, data int64, err error) {
	u, err := measureDir(src, skipTmp)
	if err != nil {
		return 0, 0, fmt.Errorf("コピー元の大きさを調べられない: %w", err)
	}
	if spec != "" {
		n, err := parseSize(spec)
		if err != nil {
			return 0, 0, err
		}
		if n < 64<<20 {
			return 0, 0, fmt.Errorf("--work-size が小さすぎる (最低 64M): %s", spec)
		}
		need := u.Bytes + max(64<<20, u.Bytes/100)
		if n < need {
			return 0, 0, fmt.Errorf("--work-size %s はコピー元 (%s) と余裕 (%s) に足りない",
				spec, humanBytes(u.Bytes), humanBytes(need))
		}
		return roundUpMiB(n), u.Bytes, nil
	}
	size = u.Bytes + max(workSlack, u.Bytes/10)
	if size < minWorkDisk {
		size = minWorkDisk
	}
	if size > maxAutoWorkDisk {
		return 0, 0, fmt.Errorf("コピー元が大きすぎる (%s)。--work-size で容量を明示する (例: --work-size %s)",
			humanBytes(u.Bytes), humanBytes(u.Bytes+(u.Bytes/10)+workSlack))
	}
	return roundUpMiB(size), u.Bytes, nil
}

// parseSize は "10G" / "512MiB" / "1048576" のような容量をバイトにする。
// 接尾辞は K/M/G/T (2 進) で、B / iB は任意。
func parseSize(s string) (int64, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, fmt.Errorf("容量が空")
	}
	i := 0
	for i < len(t) && t[i] >= '0' && t[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("容量の形式が不正: %q (例: 20G)", s)
	}
	n, err := strconv.ParseInt(t[:i], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("容量の形式が不正: %q", s)
	}
	suffix := strings.ToUpper(strings.TrimSpace(t[i:]))
	suffix = strings.TrimSuffix(suffix, "IB")
	suffix = strings.TrimSuffix(suffix, "B")
	var mult int64 = 1
	switch suffix {
	case "":
	case "K":
		mult = 1 << 10
	case "M":
		mult = 1 << 20
	case "G":
		mult = 1 << 30
	case "T":
		mult = 1 << 40
	default:
		return 0, fmt.Errorf("容量の接尾辞が不正: %q (K/M/G/T を使う)", s)
	}
	if n > (1<<63-1)/mult {
		return 0, fmt.Errorf("容量が大きすぎる: %q", s)
	}
	return n * mult, nil
}

func roundUpMiB(n int64) int64 {
	const mib = 1 << 20
	return (n + mib - 1) / mib * mib
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// prepareWorkDisk は /work 用の空の qcow2 を work に作る。仮想容量は size だが、
// 実体は書いた分だけなので、大きめに取っても host をすぐ食い潰さない。
func prepareWorkDisk(work string, size, data int64) (string, error) {
	if err := checkWorkDiskSpace(work, data); err != nil {
		return "", err
	}
	img := filepath.Join(work, "work.qcow2")
	out, err := exec.Command("qemu-img", "create", "-q", "-f", "qcow2", img, strconv.FormatInt(size, 10)).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("作業ディスクを作れない: %v: %s", err, out)
	}
	return img, nil
}

// checkWorkDiskSpace は host のコピー先に、実際に書く量 (data) と安全余裕が
// 残っているかを確かめる。qcow2 の仮想容量ではなく実データ量で判断する。
func checkWorkDiskSpace(work string, data int64) error {
	var sfs unix.Statfs_t
	if err := unix.Statfs(work, &sfs); err != nil {
		return fmt.Errorf("コピー先の空き容量を調べられない: %w", err)
	}
	free := int64(sfs.Bavail) * int64(sfs.Bsize)
	need := data + workSafetyFree
	if free < need {
		return fmt.Errorf("コピー先の空き容量が足りない (必要 %s、空き %s)。不要なイメージやキャッシュを消すか、--work-size を小さくする",
			humanBytes(need), humanBytes(free))
	}
	return nil
}

// workRuncmd は作業ディスクを VM の /work にする cloud-init の runcmd (受け口の起動前)。
// /work をまるごと置き換えるので、以降のコピーはこのディスクへ入る。
func workRuncmd(uid string) string {
	dev := "/dev/disk/by-id/virtio-" + workSerial
	return fmt.Sprintf("  - [sh, -c, \"mkdir -p /work && dev=%s && for i in $(seq 1 30); do [ -b $dev ] && break; sleep 1; done && mkfs.ext4 -q -F -L %s $dev && mount %s /work && chown %s:%s /work && grep -q ' /work ' /proc/mounts\"]\n",
		dev, workSerial, dev, uid, uid)
}

// copyStats はコピーした内容の内訳。
type copyStats struct {
	Files   int
	Dirs    int
	Links   int
	Skipped int
	Bytes   int64
}

// copyDir は src の内容を tar にして VM の /work へ流し込む。host のファイルを
// 直接マウントしないので、VM から host の他のパスへは触れない。リンクはリンクの
// まま渡し、デバイス・FIFO・ソケットは渡さない。
func copyDir(g vmGuest, src string, skipTmp bool) (copyStats, error) {
	if out, err := g.sh("grep -q ' /work ' /proc/mounts", nil); err != nil {
		return copyStats{}, fmt.Errorf("/work が VM の作業ディスクとしてマウントされていない (cloud-init の準備に失敗): %v: %s\nVM 内の診断:\n%s", err, out, diagWorkMount(g))
	}
	var stats copyStats
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		tw := tar.NewWriter(pw)
		err := writeTarDir(tw, src, skipTmp, &stats)
		if cerr := tw.Close(); err == nil {
			err = cerr
		}
		pw.CloseWithError(err)
		done <- err
	}()
	out := &cappedBuffer{max: maxShOutput}
	streamErr := g.stream("tar -x -C /work --no-same-owner --no-same-permissions --delay-directory-restore", pr, out, out)
	pr.CloseWithError(errors.New("コピーを中断した"))
	tarErr := <-done
	if tarErr != nil && !errors.Is(tarErr, io.ErrClosedPipe) {
		return stats, fmt.Errorf("コピー元の読み取りに失敗: %w", tarErr)
	}
	if streamErr != nil {
		return stats, fmt.Errorf("VM の /work へ展開できない: %v: %s", streamErr, out.String())
	}
	return stats, nil
}

// diagWorkMount は /work のマウント失敗時に VM 内の手がかりを集める
// (読み取り専用の確認コマンドのみ)。
func diagWorkMount(g vmGuest) string {
	out, _ := g.sh("echo '--- cloud-init status:'; cloud-init status --long 2>&1 | head -n 20; echo '--- lsblk:'; lsblk -o NAME,SIZE,TYPE,MOUNTPOINTS,SERIAL 2>&1; echo '--- by-id:'; ls -l /dev/disk/by-id/ 2>&1; echo '--- work:'; ls -ld /work 2>&1; df -h /work 2>&1; echo '--- mounts:'; grep -E 'work|vd' /proc/mounts 2>&1; echo '--- cloud-init work:'; grep -a -i 'work' /var/log/cloud-init-output.log 2>&1 | tail -n 20", nil)
	return string(out)
}

// openSourceFile はコピー元の通常ファイルを、WalkDir が見たものと同じ実体として開く。
// WalkDir の Info() から os.Open までのあいだにパスをシンボリックリンクへ差し替えられても、
// O_NOFOLLOW と SameFile でリンク先を読まない。
func openSourceFile(root *os.Root, rel string, want fs.FileInfo) (*os.File, error) {
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

// writeTarDir は src の中身 (src 自身は含めない) を tar に書く。
func writeTarDir(tw *tar.Writer, src string, skipTmp bool, stats *copyStats) error {
	root, err := os.OpenRoot(src)
	if err != nil {
		return err
	}
	defer root.Close()
	links := map[[2]uint64]string{}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if skipTmp && rel == ".tmp" && d.IsDir() {
			return fs.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()
		switch {
		case mode.IsDir():
			stats.Dirs++
			return tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeDir, Name: filepath.ToSlash(rel) + "/",
				Mode: int64(mode.Perm()), ModTime: info.ModTime(),
			})
		case mode.IsRegular():
			// 同じ (dev, ino) が既にあればハードリンクとして書く (DB のデータ等で容量を食わない)
			if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
				key := [2]uint64{uint64(st.Dev), uint64(st.Ino)}
				if prev, ok := links[key]; ok {
					stats.Links++
					return tw.WriteHeader(&tar.Header{
						Typeflag: tar.TypeLink, Name: filepath.ToSlash(rel), Linkname: prev,
						Mode: int64(mode.Perm()), ModTime: info.ModTime(),
					})
				}
				links[key] = filepath.ToSlash(rel)
			}
			if err := tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeReg, Name: filepath.ToSlash(rel),
				Mode: int64(mode.Perm()), Size: info.Size(), ModTime: info.ModTime(),
			}); err != nil {
				return err
			}
			f, err := openSourceFile(root, rel, info)
			if err != nil {
				return err
			}
			n, err := io.CopyN(tw, f, info.Size())
			cerr := f.Close()
			if err != nil {
				return err
			}
			if cerr != nil {
				return cerr
			}
			stats.Files++
			stats.Bytes += n
			return nil
		case mode&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			stats.Links++
			return tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeSymlink, Name: filepath.ToSlash(rel), Linkname: target,
				Mode: 0o777, ModTime: info.ModTime(),
			})
		default:
			// デバイス・FIFO・ソケットは host から持ち込まない
			stats.Skipped++
			return nil
		}
	})
}
