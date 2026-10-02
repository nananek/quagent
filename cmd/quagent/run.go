package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nananek/quagent/internal/authproxy"
	"github.com/nananek/quagent/internal/config"
	"github.com/nananek/quagent/internal/hostsvc"
	"github.com/nananek/quagent/internal/image"
	"github.com/nananek/quagent/internal/netns"
	"github.com/nananek/quagent/internal/paths"
	"github.com/nananek/quagent/internal/vm"
)

type runOpts struct {
	Repo   string
	CPUs   int
	MemMiB int
	Allow  []string
}

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "\033[1;34m[quagent]\033[0m "+format+"\n", a...)
}

func run(o runOpts) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	repo, err := repoRoot(o.Repo)
	if err != nil {
		return err
	}
	base, err := image.Latest()
	if err != nil {
		return err
	}
	dns, err := vm.HostDNS()
	if err != nil {
		return err
	}
	port, err := vm.FreePort()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(paths.RunsDir(), 0o755); err != nil {
		return err
	}
	id := time.Now().Format("20060102-150405")
	work, err := os.MkdirTemp(paths.RunsDir(), id+"-")
	if err != nil {
		return err
	}
	defer func() {
		saveLogs(work)
		os.RemoveAll(work)
		logf("VM を破棄した (ログ: %s)", filepath.Join(paths.LogsDir(), filepath.Base(work)))
	}()

	// run ごとに使い捨ての ssh 鍵
	key := filepath.Join(work, "id_ed25519")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "quagent", "-f", key).CombinedOutput(); err != nil {
		return fmt.Errorf("ssh 鍵の生成に失敗: %v: %s", err, out)
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		return err
	}
	userData := fmt.Sprintf(`#cloud-config
users:
  - name: %s
    ssh_authorized_keys: [%q]
bootcmd:
  - echo '%s %s' >> /etc/hosts
`, vm.GuestUser, strings.TrimSpace(string(pub)), hostsvc.GuestAddr, hostsvc.GuestHost)
	seed, err := vm.MakeSeed(work, "quagent-"+filepath.Base(work), "quagent", userData)
	if err != nil {
		return err
	}
	overlay := filepath.Join(work, "overlay.qcow2")
	if err := vm.MakeOverlay(base, overlay); err != nil {
		return err
	}

	// guest からの唯一の窓口 (guestfwd -> unix socket)
	hostLog, err := os.Create(filepath.Join(work, "host.log"))
	if err != nil {
		return err
	}
	defer hostLog.Close()
	logger := log.New(hostLog, "", log.Ltime)
	svc := hostsvc.New(filepath.Join(work, "host.sock"))
	providers, err := authproxy.Register(svc.Mux, cfg.Providers, logger)
	if err != nil {
		return err
	}
	if len(providers) == 0 {
		logf("認証プロキシの provider が未設定 (%s)。VM から LLM API は使えない", config.Path())
	}
	if err := svc.Start(); err != nil {
		return err
	}
	defer svc.Stop()

	// qemu の hostfwd は子 netns 側 (slirp4netns の tap0 = 10.0.2.100) で受ける。
	qemu := vm.QemuArgv(vm.QemuOpts{
		Disk: overlay, Seed: seed, CPUs: o.CPUs, MemMiB: o.MemMiB,
		ConsoleLog: filepath.Join(work, "console.log"),
		Netdev: fmt.Sprintf("dns=%s,hostfwd=tcp:10.0.2.100:%d-:22,%s",
			dns, port, hostsvc.Guestfwd(filepath.Join(work, "host.sock"))),
	})
	logf("VM を起動 (base=%s, allow=%v)", filepath.Base(base), o.Allow)
	l, err := netns.Start(netns.Spec{
		WorkDir: work, SSHPort: port, DNS: dns, Allow: o.Allow, QemuArgv: qemu,
	})
	if err != nil {
		return err
	}
	defer l.Stop()

	// 待機中の Ctrl-C でも後始末を通す。対話中は ssh が pty で受けるので届かない。
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	interrupted := func() error {
		select {
		case <-sigs:
			return fmt.Errorf("中断された")
		default:
			return l.Failed()
		}
	}

	if err := l.WaitReady(30 * time.Second); err != nil {
		return err
	}
	ssh := vm.SSH{Port: port, Key: key}
	if err := ssh.WaitReady(3*time.Minute, interrupted); err != nil {
		return err
	}
	if out, err := ssh.Run("cloud-init", "status", "--wait"); err != nil {
		return fmt.Errorf("cloud-init が失敗: %v: %s", err, out)
	}

	logf("repo を /work へコピー: %s", repo)
	if err := copyRepo(ssh, repo, work); err != nil {
		return err
	}

	if err := writeOpencodeConfig(ssh, providers); err != nil {
		return err
	}

	logf("VM に接続 (exit で破棄)")
	sh := ssh.Command([]string{"-t"}, "cd /work && exec bash -l")
	sh.Stdin, sh.Stdout, sh.Stderr = os.Stdin, os.Stdout, os.Stderr
	_ = sh.Run()
	return nil
}

func repoRoot(dir string) (string, error) {
	if dir == "" {
		dir = "."
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("git repo ではない: %s", dir)
	}
	return strings.TrimSpace(string(out)), nil
}

// copyRepo は履歴ごと (git bundle) VM の /work に clone する。
// 未コミットの変更は渡らない。
func copyRepo(ssh vm.SSH, repo, work string) error {
	bundle := filepath.Join(work, "repo.bundle")
	if out, err := exec.Command("git", "-C", repo, "bundle", "create", "-q", bundle, "--all").CombinedOutput(); err != nil {
		return fmt.Errorf("git bundle に失敗: %v: %s", err, out)
	}
	if err := ssh.CopyTo(bundle, "/tmp/repo.bundle"); err != nil {
		return err
	}
	script := `set -e
git clone -q /tmp/repo.bundle /work
git -C /work remote remove origin
rm /tmp/repo.bundle
git config --global user.name quagent
git config --global user.email quagent@localhost`
	if out, err := ssh.Run("bash", "-c", shellQuote(script)); err != nil {
		return fmt.Errorf("VM 内の clone に失敗: %v: %s", err, out)
	}
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// writeOpencodeConfig は guest の opencode が provider を認証プロキシ経由で使うよう設定する。
// apiKey はダミー (本物はプロキシが host 側で付ける)。
func writeOpencodeConfig(ssh vm.SSH, providers []string) error {
	prov := map[string]any{}
	for _, id := range providers {
		prov[id] = map[string]any{"options": map[string]any{
			"baseURL": authproxy.GuestBaseURL(hostsvc.GuestHost, id),
			"apiKey":  "quagent-proxy",
		}}
	}
	b, err := json.MarshalIndent(map[string]any{
		"$schema":  "https://opencode.ai/config.json",
		"provider": prov,
	}, "", "  ")
	if err != nil {
		return err
	}
	return ssh.WriteFile("~/.config/opencode/opencode.json", b)
}

// saveLogs は host 側のログだけを残す (VM のディスクや鍵は残さない)。
func saveLogs(work string) {
	dst := filepath.Join(paths.LogsDir(), filepath.Base(work))
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return
	}
	for _, name := range []string{"host.log", "launcher.log", "console.log"} {
		if b, err := os.ReadFile(filepath.Join(work, name)); err == nil {
			_ = os.WriteFile(filepath.Join(dst, name), b, 0o600)
		}
	}
}
