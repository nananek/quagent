// Package image はベースイメージ (Debian + rootless docker + opencode) を焼く。
//
// 焼くときだけ host の網をそのまま使う (信頼できる工程)。実行時の VM は
// このイメージの overlay で起動し、cloud-init では ssh 鍵を入れるだけにする。
// OS 固有の部分はこのパッケージに閉じ込める (Debian/Arch の比較は未決)。
package image

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nananek/quagent/internal/paths"
	"github.com/nananek/quagent/internal/vm"
)

const (
	cloudImageURL = "https://cloud.debian.org/images/cloud/trixie/latest/debian-13-generic-amd64.qcow2"
	diskSize      = "40G"
	buildOKMarker = "QUAGENT_BUILD_OK"
)

// buildUserData は焼き込み用の cloud-config。最後に成功マーカーを
// シリアルコンソールへ出して電源を切る。
var buildUserData = `#cloud-config
users:
  - name: ` + vm.GuestUser + `
    shell: /bin/bash
    lock_passwd: true
package_update: true
package_upgrade: true
packages:
  - ca-certificates
  - curl
  - git
  - jq
  - unzip
  - sqlite3
  - tmux
  - uidmap
  - dbus-user-session
  - slirp4netns
  - fuse-overlayfs
  - iptables
write_files:
  - path: /usr/local/sbin/quagent-build.sh
    permissions: '0755'
    content: |
      #!/bin/bash
      set -euxo pipefail
      U=` + vm.GuestUser + `
      UID_=$(id -u "$U")

      # docker 公式 apt リポジトリから導入。rootful デーモンは止めてマスクする。
      install -m 0755 -d /etc/apt/keyrings
      curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc
      echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
        > /etc/apt/sources.list.d/docker.list
      apt-get update
      DEBIAN_FRONTEND=noninteractive apt-get install -y \
        docker-ce docker-ce-cli containerd.io docker-ce-rootless-extras \
        docker-buildx-plugin docker-compose-plugin
      systemctl disable --now docker.service docker.socket containerd.service
      systemctl mask docker.service docker.socket containerd.service

      # rootless docker を agent のユーザー systemd で常駐させる。
      loginctl enable-linger "$U"
      for _ in $(seq 1 60); do [ -S "/run/user/$UID_/bus" ] && break; sleep 1; done
      runuser -u "$U" -- env XDG_RUNTIME_DIR="/run/user/$UID_" \
        DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$UID_/bus" \
        dockerd-rootless-setuptool.sh install
      echo "DOCKER_HOST=unix:///run/user/$UID_/docker.sock" >> /etc/environment
      runuser -u "$U" -- env XDG_RUNTIME_DIR="/run/user/$UID_" \
        DOCKER_HOST="unix:///run/user/$UID_/docker.sock" \
        docker info --format '{{.SecurityOptions}}' | grep -q rootless

      # opencode (v2)
      runuser -l "$U" -c 'curl -fsSL https://opencode.ai/v2/install | bash'
      test -x "/home/$U/.opencode/bin/opencode"
      echo 'export PATH="$HOME/.opencode/bin:$PATH"' >> "/home/$U/.profile"

      # 作業ディレクトリ
      install -d -o "$U" -g "$U" /work

      echo ` + buildOKMarker + ` > /dev/ttyS0
runcmd:
  - [/usr/local/sbin/quagent-build.sh]
power_state:
  mode: poweroff
  timeout: 30
`

// Latest は最新のベースイメージのパスを返す。
func Latest() (string, error) {
	matches, _ := filepath.Glob(filepath.Join(paths.ImagesDir(), "base-*.qcow2"))
	if len(matches) == 0 {
		return "", fmt.Errorf("ベースイメージが無い。先に `quagent image build` を実行する")
	}
	sort.Strings(matches)
	return matches[len(matches)-1], nil
}

// Build はベースイメージを新しく焼き、そのパスを返す。
func Build(cpus, memMiB int, progress io.Writer) (string, error) {
	cloud, err := fetchCloudImage(progress)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(paths.ImagesDir(), 0o755); err != nil {
		return "", err
	}
	work, err := os.MkdirTemp(paths.ImagesDir(), "build-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)

	disk := filepath.Join(work, "disk.qcow2")
	if out, err := exec.Command("qemu-img", "convert", "-O", "qcow2", cloud, disk).CombinedOutput(); err != nil {
		return "", fmt.Errorf("イメージの複製に失敗: %v: %s", err, out)
	}
	if out, err := exec.Command("qemu-img", "resize", "-q", disk, diskSize).CombinedOutput(); err != nil {
		return "", fmt.Errorf("リサイズに失敗: %v: %s", err, out)
	}
	stamp := time.Now().Format("20060102-150405")
	seed, err := vm.MakeSeed(work, "quagent-build-"+stamp, "quagent-build", buildUserData)
	if err != nil {
		return "", err
	}

	console := filepath.Join(work, "console.log")
	argv := vm.QemuArgv(vm.QemuOpts{
		Disk: disk, Seed: seed, CPUs: cpus, MemMiB: memMiB,
		ConsoleLog: console,
	})
	fmt.Fprintf(progress, "VM で焼き込み中 (数分かかる)。コンソール: %s\n", console)
	cmd := exec.Command(argv[0], argv[1:]...)
	if out, err := runWithTimeout(cmd, 45*time.Minute); err != nil {
		return "", fmt.Errorf("焼き込み VM が異常終了: %v: %s", err, out)
	}

	log, _ := os.ReadFile(console)
	if !strings.Contains(string(log), buildOKMarker) {
		keep := filepath.Join(paths.ImagesDir(), "failed-"+stamp+"-console.log")
		_ = os.WriteFile(keep, log, 0o644)
		return "", fmt.Errorf("焼き込みに失敗した。コンソールログ: %s", keep)
	}

	// 履歴を潰して単独で使える形にする
	out := filepath.Join(paths.ImagesDir(), "base-"+stamp+".qcow2")
	if b, err := exec.Command("qemu-img", "convert", "-O", "qcow2", disk, out).CombinedOutput(); err != nil {
		return "", fmt.Errorf("イメージの書き出しに失敗: %v: %s", err, b)
	}
	return out, nil
}

func fetchCloudImage(progress io.Writer) (string, error) {
	dst := filepath.Join(paths.CacheDir(), filepath.Base(cloudImageURL))
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	if err := os.MkdirAll(paths.CacheDir(), 0o755); err != nil {
		return "", err
	}
	fmt.Fprintf(progress, "クラウドイメージを取得: %s\n", cloudImageURL)
	resp, err := http.Get(cloudImageURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("取得に失敗: %s", resp.Status)
	}
	f, err := os.Create(dst + ".part")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return dst, os.Rename(dst+".part", dst)
}

func runWithTimeout(cmd *exec.Cmd, d time.Duration) ([]byte, error) {
	var buf strings.Builder
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return []byte(buf.String()), err
	case <-time.After(d):
		_ = cmd.Process.Kill()
		<-done
		return []byte(buf.String()), fmt.Errorf("%s でタイムアウト", d)
	}
}
