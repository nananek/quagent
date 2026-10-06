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
)

// GuestUser はベースイメージに焼き込むユーザー。
const GuestUser = "agent"

// GuestUID は GuestUser の uid (レシピは最初の一般ユーザーとして作るので 1000)。
const GuestUID = 1000

// DataDisk は guest に追加で見せる raw のディスク。
type DataDisk struct {
	Serial string // guest での識別名 (/dev/disk/by-id/virtio-<Serial>)
	Path   string // host のイメージ
}

// MakeSeed は NoCloud の seed ISO を dir に作り、そのパスを返す。
// extra (ISO 上の名前 -> host のパス) も同梱する (guest の受け口のバイナリなど)。
func MakeSeed(dir, instanceID, hostname, userData string, extra map[string]string) (string, error) {
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
	args := []string{"-as", "mkisofs", "-quiet", "-output", iso, "-volid", "cidata", "-joliet", "-rock",
		"-graft-points",
		"user-data=" + filepath.Join(seedDir, "user-data"), "meta-data=" + filepath.Join(seedDir, "meta-data")}
	for name, src := range extra {
		args = append(args, name+"="+src)
	}
	cmd := exec.Command("xorriso", args...)
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
	// VsockCID が 0 でなければ vsock デバイスを付ける (host との操作経路)。
	VsockCID uint32
	// DataDisks は追加で見せる raw のディスク。guest では
	// /dev/disk/by-id/virtio-<Serial> に現れる。
	DataDisks []DataDisk
	// NestedVirt は guest に CPU の仮想化支援 (svm / vmx) を見せ、VM の中で KVM を
	// 使えるようにする。既定では隠す (入れ子の KVM を攻撃面として出さない)。
	NestedVirt bool
	// UEFI は BIOS (SeaBIOS) の代わりに OVMF で起動する (UEFI 専用のイメージ向け)。
	// FirmwarePath が空なら OVMFPath() が探す。
	UEFI bool
	// FirmwarePath は UEFI のとき使う OVMF の統合イメージ。通常は空。
	FirmwarePath string
	// Extra は追加の qemu 引数。
	Extra []string
}

// UEFI に使う OVMF の統合イメージ (-bios に渡せるもの) の置き場。配布物によって
// 場所が違うので順に探す。OVMF_CODE.fd / OVMF_VARS.fd に分かれたものは使わない。
var ovmfPaths = []string{
	"/usr/share/edk2/x64/OVMF.4m.fd",      // Arch Linux
	"/usr/share/edk2-ovmf/x64/OVMF.4m.fd", // Arch Linux (別の置き場)
	"/usr/share/edk2/ovmf/OVMF.fd",        // Fedora / RHEL
	"/usr/share/OVMF/OVMF.fd",             // Debian / Ubuntu
	"/usr/share/ovmf/OVMF.fd",             // Ubuntu (古い)
}

// OVMFPath は UEFI 起動に使う OVMF の統合イメージを返す。
func OVMFPath() (string, error) { return ovmfPath(ovmfPaths) }

func ovmfPath(candidates []string) (string, error) {
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p, nil
		}
	}
	return "", fmt.Errorf("UEFI ファームウェア (OVMF の統合イメージ OVMF.fd / OVMF.4m.fd) が見つからない。OVMF (edk2-ovmf / ovmf) を入れるか、レシピの firmware を bios にする")
}

// QemuArgv は qemu-system-x86_64 のコマンドラインを返す。
func QemuArgv(o QemuOpts) ([]string, error) {
	netdev := "user,id=n0"
	if o.Netdev != "" {
		netdev += "," + o.Netdev
	}
	cpu := "host,-svm,-vmx"
	if o.NestedVirt {
		cpu = "host"
	}
	argv := []string{
		"qemu-system-x86_64",
		"-machine", "q35,accel=kvm", "-cpu", cpu,
		"-smp", strconv.Itoa(o.CPUs), "-m", strconv.Itoa(o.MemMiB),
		"-nographic", "-serial", "file:" + o.ConsoleLog, "-monitor", "none",
		// 追加のディスクがあっても起動はこのディスクから
		"-drive", "file=" + o.Disk + ",if=none,id=root,format=qcow2",
		"-device", "virtio-blk-pci,drive=root,bootindex=0",
		"-drive", "file=" + o.Seed + ",if=virtio,format=raw,readonly=on",
		"-netdev", netdev,
		"-device", "virtio-net-pci,netdev=n0",
		"-device", "virtio-rng-pci",
	}
	if o.UEFI {
		fw := o.FirmwarePath
		if fw == "" {
			var err error
			if fw, err = OVMFPath(); err != nil {
				return nil, err
			}
		}
		// 統合イメージなので EFI 変数の置き場は読めるだけで、書き込みは残らない
		// (起動に要る既定のローダーはイメージの /EFI 以下から読む)。
		argv = append(argv, "-bios", fw)
	}
	if o.VsockCID != 0 {
		argv = append(argv, "-device", fmt.Sprintf("vhost-vsock-pci,guest-cid=%d", o.VsockCID))
	}
	for _, d := range o.DataDisks {
		// オプション値の中のカンマは二重にしてエスケープする
		path := strings.ReplaceAll(d.Path, ",", ",,")
		argv = append(argv,
			"-drive", fmt.Sprintf("file=%s,if=none,id=%s,format=raw", path, d.Serial),
			"-device", fmt.Sprintf("virtio-blk-pci,drive=%s,serial=%s", d.Serial, d.Serial))
	}
	return append(argv, o.Extra...), nil
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
