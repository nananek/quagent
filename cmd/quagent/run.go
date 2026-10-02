package main

import (
	"bytes"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nananek/quagent/internal/access"
	"github.com/nananek/quagent/internal/authproxy"
	"github.com/nananek/quagent/internal/config"
	"github.com/nananek/quagent/internal/console"
	"github.com/nananek/quagent/internal/guest"
	"github.com/nananek/quagent/internal/hostsvc"
	"github.com/nananek/quagent/internal/image"
	"github.com/nananek/quagent/internal/mcpsrv"
	"github.com/nananek/quagent/internal/netns"
	"github.com/nananek/quagent/internal/paths"
	"github.com/nananek/quagent/internal/pr"
	"github.com/nananek/quagent/internal/vm"
)

type runOpts struct {
	Repo   string
	Recipe string
	CPUs   int
	MemMiB int
	Allow  []string
	// Interactive は端末から使うとき true。tmux でエージェントと承認コンソールを開く。
	Interactive bool
	// SSH は人が ssh で VM に入れるようにする (quagent 自身の操作は常に vsock)。
	SSH bool
	// MountTmp は host の repo の .tmp を VM の /work/.tmp に読み書き可能でマウントする。
	MountTmp bool
	// Agent は VM 内で動かすエージェント (agents のキー)。
	Agent string
}

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "\033[1;34m[quagent]\033[0m "+format+"\n", a...)
}

func run(o runOpts) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if o.Agent == "" {
		o.Agent = DefaultAgent
	}
	ag, ok := agents[o.Agent]
	if !ok {
		return fmt.Errorf("不明なエージェント %q (使えるもの: %s)", o.Agent, strings.Join(agentNames(), ", "))
	}
	repo, err := repoRoot(o.Repo)
	if err != nil {
		return err
	}
	img, err := image.Latest(o.Recipe)
	if err != nil {
		return err
	}
	base := img.Path
	dns, err := vm.HostDNS()
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := checkStatic(self); err != nil {
		return err
	}
	if err := guest.Available(); err != nil {
		return err
	}
	g := vmGuest{cid: newCID(), self: self}
	var shares []vm.Share
	if o.MountTmp {
		dir, err := prepareTmp(repo)
		if err != nil {
			return err
		}
		shares = append(shares, vm.Share{Tag: tmpTag, Path: dir})
	}

	if err := os.MkdirAll(paths.RunsDir(), 0o755); err != nil {
		return err
	}
	sweepRuns()
	id := time.Now().Format("20060102-150405")
	work, err := os.MkdirTemp(paths.RunsDir(), id+"-")
	if err != nil {
		return err
	}
	// run のあいだ作業ディレクトリをロックしておく (強制終了で残ったものを次回掃除するため)
	lock, err := lockRun(work)
	if err != nil {
		return err
	}
	defer lock.Close()
	defer func() {
		saveLogs(work)
		os.RemoveAll(work)
		logf("VM を破棄した (ログ: %s)", filepath.Join(paths.LogsDir(), filepath.Base(work)))
	}()

	// guest からの唯一の窓口 (host の vsock。VM 内の受け口が 127.0.0.1 から中継する)
	hostLog, err := os.Create(filepath.Join(work, "host.log"))
	if err != nil {
		return err
	}
	defer hostLog.Close()
	logger := log.New(hostLog, "", log.Ltime)
	svc, err := hostsvc.New(g.cid)
	if err != nil {
		return err
	}
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

	// VM の受け口 (quagent 自身) を seed に入れ、cloud-init で作業ユーザーとして常駐させる。
	// ssh は既定で止める。--ssh のときだけ使い捨ての鍵で人が入れるようにする。
	userData := fmt.Sprintf(`#cloud-config
bootcmd:
  - echo '127.0.0.1 %s' >> /etc/hosts
  # 外へは出られないので NTP は使えない (時計は KVM が合わせる)。拒否の記録が並ぶだけなので止める
  - [sh, -c, "systemctl mask --now systemd-timesyncd.service 2>/dev/null; true"]
  - [sh, -c, "%s"]
runcmd:
%s  - [sh, -c, "mkdir -p /run/quagent-seed && mount -o ro /dev/disk/by-label/cidata /run/quagent-seed && install -m 755 /run/quagent-seed/quagent-guest /usr/local/bin/quagent-guest && umount /run/quagent-seed"]
  - [systemd-run, --unit=quagent-guest, --uid=%s, -p, Restart=always, /usr/local/bin/quagent-guest, %s, "%d"]
`, hostsvc.GuestHost, maskCmd(sshUnits(o.SSH)), mountCmds(shares), vm.GuestUser, guestCommand, svc.Port)
	var sshPort int
	var sshKey string
	if o.SSH {
		if sshPort, err = vm.FreePort(); err != nil {
			return err
		}
		sshKey = filepath.Join(work, "id_ed25519")
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "quagent", "-f", sshKey).CombinedOutput(); err != nil {
			return fmt.Errorf("ssh 鍵の生成に失敗: %v: %s", err, out)
		}
		pub, err := os.ReadFile(sshKey + ".pub")
		if err != nil {
			return err
		}
		userData += fmt.Sprintf("ssh_pwauth: false\nusers:\n  - name: %s\n    ssh_authorized_keys: [%q]\n", vm.GuestUser, strings.TrimSpace(string(pub)))
	}
	seed, err := vm.MakeSeed(work, "quagent-"+filepath.Base(work), "quagent", userData,
		map[string]string{"quagent-guest": self})
	if err != nil {
		return err
	}
	overlay := filepath.Join(work, "overlay.qcow2")
	if err := vm.MakeOverlay(base, overlay); err != nil {
		return err
	}

	// DNS は qemu 既定の 10.0.2.3 -> 子 netns の自前 DNS。子 netns は IPv4 のみ。
	netdev := "ipv6=off"
	if o.SSH {
		// qemu の hostfwd は子 netns 側 (slirp4netns の tap0 = 10.0.2.100) で受ける。
		netdev += fmt.Sprintf(",hostfwd=tcp:10.0.2.100:%d-:22", sshPort)
	}
	qemu := vm.AsGuestUID(vm.QemuArgv(vm.QemuOpts{
		Disk: overlay, Seed: seed, CPUs: o.CPUs, MemMiB: o.MemMiB,
		ConsoleLog: filepath.Join(work, "console.log"),
		Netdev:     netdev, VsockCID: g.cid, Shares: shares,
	}))
	logf("VM を起動 (base=%s, allow=%v)", filepath.Base(base), o.Allow)
	l, err := netns.Start(netns.Spec{
		WorkDir: work, SSHPort: sshPort, DNS: dns, Allow: o.Allow, QemuArgv: qemu,
	})
	if err != nil {
		return err
	}
	defer l.Stop()

	// 接続先の申請と承認 (MCP -> Manager -> launcher の nft)
	mgr, err := access.NewManager(l, access.AlwaysPath())
	if err != nil {
		return err
	}
	if err := mgr.Preallow(o.Allow); err != nil {
		return err
	}
	con, err := console.NewServer(mgr, filepath.Join(work, "console.sock"))
	if err != nil {
		return err
	}
	defer con.Close()
	g.consoleSock = filepath.Join(work, "console.sock")
	con.Clipboard, err = clipboardSink(cfg.Clipboard)
	if err != nil {
		return err
	}
	go relayDenied(l, con)
	protected := cfg.PR.ProtectedBranches
	if len(protected) == 0 {
		protected = pr.DefaultProtected
	}
	// VM 内のコミットに付けさせる印 (run ごとの捨て鍵)。PR 化のとき、この鍵で
	// 署名されたコミットだけを利用者の鍵で署名し直す (他人のコミットには触らない)。
	markKey := filepath.Join(work, "mark_ed25519")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "quagent-mark", "-f", markKey).CombinedOutput(); err != nil {
		return fmt.Errorf("捨て鍵の生成に失敗: %v: %s", err, out)
	}
	markPub, err := os.ReadFile(markKey + ".pub")
	if err != nil {
		return err
	}
	publisher := &pr.Publisher{
		Repo: repo, Work: work, Protected: protected, GH: pr.RunGH,
		GuestURL: g.gitURL(), GitConfig: []string{"protocol.ext.allow=always"},
		MarkPub: strings.TrimSpace(string(markPub)), MinInterval: 10 * time.Second,
	}
	svc.Mux.Handle(mcpsrv.Path, mcpsrv.Handler(mgr, publisher, con.Log))

	// 待機中の Ctrl-C でも後始末を通す。対話中の入力は tmux の端末が受けるので届かない。
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
	if err := guest.WaitReady(g.cid, 3*time.Minute, interrupted); err != nil {
		return err
	}
	if out, err := g.sh("cloud-init status --wait", nil); err != nil {
		return fmt.Errorf("cloud-init が失敗: %v: %s", err, out)
	}
	if err := checkSSHOff(g, sshUnits(o.SSH), !o.SSH); err != nil {
		return err
	}

	logf("repo を /work へコピー: %s", repo)
	if err := copyRepo(g, repo, work, markKey); err != nil {
		return err
	}
	if err := ag.setup(g, cfg, providers, svc.Token); err != nil {
		return err
	}
	if o.SSH {
		con.Log(fmt.Sprintf("ssh: ssh -i %s -p %d -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null %s@127.0.0.1",
			sshKey, sshPort, vm.GuestUser))
	}

	if !o.Interactive {
		// 端末が無い (自動テスト等): tmux を使わず stdin をそのまま VM のシェルへ流す。
		// 出力先が端末のこともあるので、OSC 52 (クリップボード操作) は抜き取って捨てる
		_ = g.stream("cd /work && exec bash -l", os.Stdin, guest.StripClipboard(os.Stdout), guest.StripClipboard(os.Stderr))
		return nil
	}

	agent := g.interactiveArgv(ag.command)
	session := "quagent-" + filepath.Base(work)
	return runTmux(session, agent, []string{self, consoleCommand, filepath.Join(work, "console.sock")}, con.Quit)
}

// sshUnits は VM で止める ssh のユニット。distro で名前が違う (Debian は ssh.*、
// Arch は sshd.*) ので両方挙げる。systemd-ssh-generator が vsock や unix ソケットを
// 見つけて作る sshd-vsock.socket / sshd-unix-local.socket は --ssh でも要らない。
func sshUnits(sshOn bool) []string {
	units := []string{"sshd-vsock.socket", "sshd-unix-local.socket"}
	if !sshOn {
		units = append(units, "ssh.service", "ssh.socket", "sshd.service", "sshd.socket")
	}
	return units
}

// maskCmd は units を 1 つずつ mask して止める sh スクリプト。まとめて渡すと
// 存在しないユニットや別名 (Debian の sshd.service) が 1 つあるだけで全体が止まらない。
// 止まったかは host が checkSSHOff で確かめるので、ここでは失敗を問わない。
func maskCmd(units []string) string {
	return "for u in " + strings.Join(units, " ") + "; do systemctl mask --now $u; done 2>/dev/null; true"
}

// checkSSHOff は units が動いていないこと (noPort なら 22 番の待ち受けも無いこと) を
// 確かめる。止められていなければ、ssh を開けたまま進めずに起動をやめる。
func checkSSHOff(g vmGuest, units []string, noPort bool) error {
	script := "for u in " + strings.Join(units, " ") + "; do systemctl is-active --quiet $u && echo $u; done"
	if noPort {
		script += "; ss -Hln -A inet,vsock | awk '$5 ~ /:22$/ {print \"listen \" $5}'"
	}
	out, err := g.sh(script+"; true", nil)
	if err != nil {
		return fmt.Errorf("ssh の停止を確かめられない: %v: %s", err, out)
	}
	if s := strings.TrimSpace(string(out)); s != "" {
		return fmt.Errorf("VM の ssh を止められなかった:\n%s", s)
	}
	return nil
}

// lockRun は作業ディレクトリのロックを取る。プロセスが終われば (強制終了でも) 外れる。
func lockRun(work string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(work, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// sweepRuns は強制終了などで残った作業ディレクトリ (ロックが外れているもの) を、
// host 側のログだけ残して消す。
func sweepRuns() {
	entries, err := os.ReadDir(paths.RunsDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		dir := filepath.Join(paths.RunsDir(), e.Name())
		// 作られた直後 (まだロックを取っていない) の run を消さない
		if info, err := e.Info(); err != nil || time.Since(info.ModTime()) < time.Minute {
			continue
		}
		f, err := lockRun(dir)
		if err != nil {
			continue // 動いている run
		}
		f.Close()
		saveLogs(dir)
		if os.RemoveAll(dir) == nil {
			logf("前回の残骸を掃除した: %s", e.Name())
		}
	}
}

// clipboardSink は承認したクリップボードの中身の入れ方を設定から選ぶ。
func clipboardSink(c config.Clipboard) (func([]byte) error, error) {
	switch c.Method {
	case "", "tmux":
		return console.TmuxClipboard, nil
	case "osc52":
		return console.OSC52Clipboard("/dev/tty"), nil
	case "command":
		if len(c.Command) == 0 {
			return nil, fmt.Errorf("clipboard.method が command なら clipboard.command (例: [\"wl-copy\"]) が要る")
		}
		return console.CommandClipboard(c.Command), nil
	default:
		return nil, fmt.Errorf("clipboard.method は tmux / osc52 / command のどれか: %q", c.Method)
	}
}

// tmpTag は .tmp を 9p で見せるときのタグ。
const tmpTag = "quagent-tmp"

// prepareTmp は host の repo の .tmp を用意し、マウントしてよいかを確かめる。
func prepareTmp(repo string) (string, error) {
	dir := filepath.Join(repo, ".tmp")
	// git で管理しているファイルがあると、VM の checkout が host に書き込んでしまう
	if out, _ := exec.Command("git", "-C", repo, "ls-files", "--", ".tmp").Output(); len(strings.TrimSpace(string(out))) > 0 {
		return "", fmt.Errorf(".tmp に git で管理しているファイルがあるのでマウントしない")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if exec.Command("git", "-C", repo, "check-ignore", "-q", ".tmp/").Run() != nil {
		logf("注意: %s は .gitignore されていない", dir)
	}
	logf("%s を VM の /work/.tmp にマウントする (VM から読み書きできる)", dir)
	return dir, nil
}

// mountCmds は shares を guest の /work/<.tmp> にマウントする cloud-init の runcmd を返す。
// repo の取り込みより前 (受け口の起動前) にマウントしておく。
func mountCmds(shares []vm.Share) string {
	var b strings.Builder
	for _, sh := range shares {
		if sh.Tag == tmpTag {
			fmt.Fprintf(&b, "  - [sh, -c, \"mkdir -p /work/.tmp && mount -t 9p -o trans=virtio,version=9p2000.L,msize=262144 %s /work/.tmp\"]\n", sh.Tag)
		}
	}
	return b.String()
}

// newCID は vsock の guest CID を選ぶ (3 以上。host 上で他の VM と被らないよう乱数)。
func newCID() uint32 {
	return 3 + uint32(rand.Int64N(1<<31-3))
}

// relayDenied は DNS で拒否したドメインを承認コンソールに流す (同じ名前は 1 分に 1 回、
// 全体でも 1 分に 10 件まで)。
func relayDenied(l *netns.Launcher, con *console.Server) {
	last := map[string]time.Time{}
	var window time.Time
	var shown, dropped int
	for name := range l.Denied {
		now := time.Now()
		if now.Sub(window) >= time.Minute {
			if dropped > 0 {
				con.Log(fmt.Sprintf("DNS で拒否: ほか %d 件 (多すぎるので省略)", dropped))
			}
			window, shown, dropped = now, 0, 0
			// 覚えている名前も 1 分ごとに捨てる (ランダムな名前で膨らませない)
			clear(last)
		}
		if now.Sub(last[name]) < time.Minute {
			continue
		}
		last[name] = now
		if shown >= 10 {
			dropped++
			continue
		}
		shown++
		con.Log("DNS で拒否: " + name)
	}
}

// runTmux は上にエージェント、下に承認コンソールを置いた tmux セッションを作り、
// 終わるまで待つ。エージェントのペインが終わるとセッションごと閉じる。
func runTmux(session string, agent, consoleArgv []string, quit <-chan struct{}) error {
	tmux := func(args ...string) error {
		if out, err := exec.Command("tmux", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("tmux %s: %v: %s", args[0], err, out)
		}
		return nil
	}
	agentSh := shellJoin(agent) + "; tmux kill-session -t " + shellQuote(session)
	if err := tmux("new-session", "-d", "-s", session, "-x", "200", "-y", "50", "sh", "-c", agentSh); err != nil {
		return err
	}
	defer func() { _ = exec.Command("tmux", "kill-session", "-t", session).Run() }()
	// VM の出力で窓の名前を書き換えたり、tmux を素通りして外の端末へ送ったりさせない
	_ = tmux("set-option", "-w", "-t", session+":", "allow-rename", "off")
	_ = tmux("set-option", "-w", "-t", session+":", "allow-passthrough", "off")
	if err := tmux(append([]string{"split-window", "-v", "-l", "30%", "-t", session + ":"}, consoleArgv...)...); err != nil {
		return err
	}
	_ = tmux("select-pane", "-t", session+":.0")

	logf("tmux セッション %s に接続 (エージェントを終了すると VM を破棄)", session)
	attached := make(chan struct{})
	if os.Getenv("TMUX") != "" {
		close(attached)
		if err := tmux("switch-client", "-t", session); err != nil {
			return err
		}
	} else {
		att := exec.Command("tmux", "attach-session", "-t", session)
		att.Stdin, att.Stdout, att.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := att.Start(); err != nil {
			return err
		}
		go func() { _ = att.Wait(); close(attached) }()
	}
	// 終わるまで待つ。デタッチされてもセッションが続くあいだは VM を生かしておく
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	notified := false
	for {
		select {
		case <-quit:
			// セッションを閉じれば attach も戻る。端末を返してから後始末に進む
			_ = exec.Command("tmux", "kill-session", "-t", session).Run()
			<-attached
			return nil
		case <-tick.C:
		}
		if exec.Command("tmux", "has-session", "-t", session).Run() != nil {
			<-attached
			return nil
		}
		select {
		case <-attached:
			if !notified && os.Getenv("TMUX") == "" {
				logf("デタッチ中。戻るには: tmux attach -t %s", session)
				notified = true
			}
		default:
		}
	}
}

func shellJoin(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = shellQuote(a)
	}
	return strings.Join(q, " ")
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

// copyRepo は履歴ごと (git bundle) VM の /work に取り込み、host と同じブランチを
// checkout する。未コミットの変更は渡らない。/work が空でなくても取り込めるよう
// clone ではなく init + fetch にする (.tmp のマウント先などがあってもよい)。
func copyRepo(g vmGuest, repo, work, markKey string) error {
	bundle := filepath.Join(work, "repo.bundle")
	if out, err := exec.Command("git", "-C", repo, "bundle", "create", "-q", bundle, "--all").CombinedOutput(); err != nil {
		return fmt.Errorf("git bundle に失敗: %v: %s", err, out)
	}
	head, err := exec.Command("git", "-C", repo, "symbolic-ref", "-q", "--short", "HEAD").Output()
	if err != nil {
		// detached HEAD ならコミットを直接 checkout する
		head, err = exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
		if err != nil {
			return err
		}
	}
	name, email, err := gitIdentity(repo)
	if err != nil {
		return err
	}
	f, err := os.Open(bundle)
	if err != nil {
		return err
	}
	defer f.Close()
	if out, err := g.sh("cat > /tmp/repo.bundle", f); err != nil {
		return fmt.Errorf("bundle の転送に失敗: %v: %s", err, out)
	}
	key, err := os.ReadFile(markKey)
	if err != nil {
		return err
	}
	if out, err := g.sh("mkdir -p ~/.ssh && umask 077 && cat > ~/.ssh/quagent-mark", bytes.NewReader(key)); err != nil {
		return fmt.Errorf("捨て鍵の転送に失敗: %v: %s", err, out)
	}
	pub, err := os.ReadFile(markKey + ".pub")
	if err != nil {
		return err
	}
	// VM 内の git log でも署名を確かめられるようにする (無いとエラーが出て紛らわしい)
	if out, err := g.sh("cat > ~/.ssh/quagent-allowed-signers", strings.NewReader("* "+string(pub))); err != nil {
		return fmt.Errorf("捨て鍵の公開鍵の転送に失敗: %v: %s", err, out)
	}
	// コミットは利用者の名前で作り (host と同じ user.name / user.email)、run ごとの
	// 捨て鍵で署名して「VM で作った」印にする。利用者の鍵での署名は PR 化のときに host で行う。
	script := `set -e
git config --global user.name ` + shellQuote(name) + `
git config --global user.email ` + shellQuote(email) + `
git config --global gpg.format ssh
git config --global user.signingkey ~/.ssh/quagent-mark
git config --global gpg.ssh.allowedSignersFile ~/.ssh/quagent-allowed-signers
git config --global commit.gpgsign true
git config --global tag.gpgsign false
cd /work
git init -q -b quagent-init
git fetch -q /tmp/repo.bundle '+refs/heads/*:refs/heads/*' '+refs/tags/*:refs/tags/*' '+refs/remotes/*:refs/remotes/*'
git checkout -q ` + shellQuote(strings.TrimSpace(string(head))) + `
rm /tmp/repo.bundle`
	if out, err := g.sh(script, nil); err != nil {
		return fmt.Errorf("VM 内での取り込みに失敗: %v: %s", err, out)
	}
	return nil
}

// gitIdentity は repo で使われる user.name / user.email を返す (repo ごとの設定があれば
// そちら)。VM に渡すのはこの 2 つだけで、署名や認証の設定は渡さない。
func gitIdentity(repo string) (name, email string, err error) {
	get := func(key string) string {
		out, _ := exec.Command("git", "-C", repo, "config", "--get", key).Output()
		return strings.TrimSpace(string(out))
	}
	name, email = get("user.name"), get("user.email")
	if name == "" || email == "" {
		return "", "", fmt.Errorf("host の git に user.name / user.email が設定されていない (VM 内のコミットに使う)")
	}
	return name, email, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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
