// Package vm は qemu VM の起動に必要な部品 (cloud-init seed・overlay・
// qemu 引数・ssh) をまとめる。ネットワーク隔離は netns パッケージが担う。
package vm

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// GuestUser はベースイメージに焼き込むユーザー。
const GuestUser = "agent"

// MakeSeed は NoCloud の seed ISO を dir に作り、そのパスを返す。
func MakeSeed(dir, instanceID, hostname, userData string) (string, error) {
	seedDir := filepath.Join(dir, "seed")
	if err := os.MkdirAll(seedDir, 0o755); err != nil {
		return "", err
	}
	meta := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", instanceID, hostname)
	if err := os.WriteFile(filepath.Join(seedDir, "meta-data"), []byte(meta), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(seedDir, "user-data"), []byte(userData), 0o644); err != nil {
		return "", err
	}
	iso := filepath.Join(dir, "seed.iso")
	cmd := exec.Command("xorriso", "-as", "mkisofs", "-quiet", "-output", iso,
		"-volid", "cidata", "-joliet", "-rock",
		filepath.Join(seedDir, "user-data"), filepath.Join(seedDir, "meta-data"))
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("seed ISO 作成に失敗: %v: %s", err, out)
	}
	return iso, nil
}

// MakeOverlay は base を backing file とする qcow2 差分を作る。
func MakeOverlay(base, overlay string) error {
	out, err := exec.Command("qemu-img", "create", "-q", "-f", "qcow2",
		"-b", base, "-F", "qcow2", overlay).CombinedOutput()
	if err != nil {
		return fmt.Errorf("overlay 作成に失敗: %v: %s", err, out)
	}
	return nil
}

// QemuOpts は qemu の起動パラメータ。
type QemuOpts struct {
	Disk       string
	Seed       string
	CPUs       int
	MemMiB     int
	ConsoleLog string
	// Netdev は -netdev user, に続けるオプション (dns= や hostfwd=)。
	Netdev string
}

// QemuArgv は qemu-system-x86_64 のコマンドラインを返す。
func QemuArgv(o QemuOpts) []string {
	netdev := "user,id=n0"
	if o.Netdev != "" {
		netdev += "," + o.Netdev
	}
	return []string{
		"qemu-system-x86_64",
		"-machine", "q35,accel=kvm", "-cpu", "host",
		"-smp", strconv.Itoa(o.CPUs), "-m", strconv.Itoa(o.MemMiB),
		"-nographic", "-serial", "file:" + o.ConsoleLog, "-monitor", "none",
		"-drive", "file=" + o.Disk + ",if=virtio,format=qcow2",
		"-drive", "file=" + o.Seed + ",if=virtio,format=raw,readonly=on",
		"-netdev", netdev,
		"-device", "virtio-net-pci,netdev=n0",
		"-device", "virtio-rng-pci",
	}
}

// HostDNS は netns と guest に渡す host の実 IPv4 リゾルバを返す。
// loopback (systemd-resolved の stub 等) は slirp4netns から届かないので除く。
func HostDNS() (string, error) {
	for _, f := range []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"} {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 2 || fields[0] != "nameserver" {
				continue
			}
			ip := net.ParseIP(fields[1])
			if ip == nil || ip.To4() == nil || ip.IsLoopback() {
				continue
			}
			fh.Close()
			return ip.String(), nil
		}
		fh.Close()
	}
	return "", fmt.Errorf("host の IPv4 リゾルバ (loopback 以外) が見つからない")
}

// FreePort は host 127.0.0.1 の空きポートを返す。
func FreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// SSH は host から guest への ssh/scp 接続情報。
type SSH struct {
	Port int
	Key  string
}

func (s SSH) commonOpts() []string {
	return []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=5",
		"-i", s.Key,
	}
}

// Command は guest で args を実行する ssh コマンドを返す。tty が要るなら -t を args より前に渡す。
func (s SSH) Command(sshFlags []string, args ...string) *exec.Cmd {
	argv := append(s.commonOpts(), "-p", strconv.Itoa(s.Port))
	argv = append(argv, sshFlags...)
	argv = append(argv, GuestUser+"@127.0.0.1")
	argv = append(argv, args...)
	return exec.Command("ssh", argv...)
}

// Run は guest でコマンドを実行し、出力を返す。
func (s SSH) Run(args ...string) ([]byte, error) {
	return s.Command(nil, args...).CombinedOutput()
}

// CopyTo は host のファイルを guest の dst へ送る。
func (s SSH) CopyTo(src, dst string) error {
	argv := append(s.commonOpts(), "-q", "-P", strconv.Itoa(s.Port), src, GuestUser+"@127.0.0.1:"+dst)
	if out, err := exec.Command("scp", argv...).CombinedOutput(); err != nil {
		return fmt.Errorf("scp に失敗: %v: %s", err, out)
	}
	return nil
}

// WriteFile は guest の path (~ 始まり可) に data を書く。
func (s SSH) WriteFile(path string, data []byte) error {
	dir := path[:strings.LastIndex(path, "/")]
	cmd := s.Command(nil, fmt.Sprintf("mkdir -p %s && cat > %s", dir, path))
	cmd.Stdin = strings.NewReader(string(data))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("guest への書き込みに失敗 (%s): %v: %s", path, err, out)
	}
	return nil
}

// WaitReady は ssh が通るまで待つ。fail が non-nil を返したら中断する。
func (s SSH) WaitReady(timeout time.Duration, fail func() error) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fail != nil {
			if err := fail(); err != nil {
				return err
			}
		}
		if _, err := s.Run("true"); err == nil {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("%s 以内に VM へ ssh できなかった", timeout)
}
