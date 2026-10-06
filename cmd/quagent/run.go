package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nananek/quagent/internal/access"
	"github.com/nananek/quagent/internal/authproxy"
	"github.com/nananek/quagent/internal/config"
	"github.com/nananek/quagent/internal/console"
	"github.com/nananek/quagent/internal/guard"
	"github.com/nananek/quagent/internal/guest"
	"github.com/nananek/quagent/internal/hostsvc"
	"github.com/nananek/quagent/internal/image"
	"github.com/nananek/quagent/internal/mcpsrv"
	"github.com/nananek/quagent/internal/netns"
	"github.com/nananek/quagent/internal/paths"
	"github.com/nananek/quagent/internal/pr"
	"github.com/nananek/quagent/internal/sandbox"
	"github.com/nananek/quagent/internal/tlsmitm"
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
	// MountTmp は host の repo の .tmp と VM の /work/.tmp を受け渡す (tmpdisk.go)。
	MountTmp bool
	// NestedVirt は VM の中で KVM を使えるようにする (VM の中で VM を動かす開発用)。
	NestedVirt bool
	// LocalHead は checkout 中のブランチをローカルの先頭 (未 push のコミットを含む) で
	// VM に渡す。既定では upstream (fetch 済みの先頭) で渡す。
	LocalHead bool
	// Agent は VM 内で動かすエージェント (agents のキー)。
	Agent string
	// PRApproval は PR の作成を承認コンソールで確認してから push する。
	PRApproval bool
}

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "\033[1;34m[quagent]\033[0m "+format+"\n", a...)
}

// caGuestPath は TLS 終端に使う使い捨て CA を guest の信頼ストアへ入れる場所。
const caGuestPath = "/usr/local/share/ca-certificates/quagent-mitm.crt"

// caEnv は、システムの信頼ストアを使わず自前の束 (certifi など) を見る実装にも
// 使い捨て CA を教える。Node / Bun は NODE_EXTRA_CA_CERTS を見る。curl や git など
// システムの束を見るものは update-ca-certificates が更新した束で足りる。
const caEnv = `export NODE_EXTRA_CA_CERTS=` + caGuestPath

// injectCA はエージェントの entrypoint に使い捨て CA を教える export を差し込み、
// cloud-init の write_files に足す CA の断片と、信頼ストアを更新する runcmd の断片を
// 返す。shebang は先頭に残す。caCert が空なら何も変えない。
func injectCA(entrypoint, caCert string) (newEntrypoint, writeFile, runCmd string) {
	if caCert == "" {
		return entrypoint, "", ""
	}
	head, rest, _ := strings.Cut(entrypoint, "\n")
	if rest == "" {
		// 1 行しかない entrypoint でも export が shebang の次に来るようにする。
		return entrypoint + "\n" + caEnv, caWriteFile(caCert), caRunCmd
	}
	return head + "\n" + caEnv + "\n" + rest, caWriteFile(caCert), caRunCmd
}

// caRunCmd は書き込んだ CA を信頼ストアに反映する runcmd の行。
const caRunCmd = "  - [update-ca-certificates]\n"

// caWriteFile は CA の証明書を guest に置く write_files の断片 (YAML のリスト項目)。
func caWriteFile(caCert string) string {
	return fmt.Sprintf("  - path: %s\n    permissions: '0644'\n    content: |\n%s",
		caGuestPath, indentBlock(caCert, "      "))
}

// sandboxWriteFile は sandbox の方針を guest に置く write_files の断片
// (YAML のリスト項目)。root 所有 0644 で置くので、作業ユーザーは読めるが書き換えられない。
func sandboxWriteFile(policy []byte) string {
	return fmt.Sprintf("  - path: %s\n    permissions: '0644'\n    content: |\n%s",
		sandbox.ConfigPath, indentBlock(string(policy), "      "))
}

// normalizePassthrough は TLS 終端しない行き先のパターンを検証・正規化する
// ("example.com" か "*.example.com")。空なら nil を返す。
func normalizePassthrough(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	return access.NormalizeDomains(in)
}

func run(o runOpts) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.Guard.InspectHTTPS && !cfg.Guard.Enabled {
		return fmt.Errorf("guard.inspect_https を使うには guard.enabled を true にする")
	}
	inspectHTTPS := cfg.Guard.Enabled && cfg.Guard.InspectHTTPS
	// 証明書を固定 (pinning) するクライアント向けに、TLS 終端しない行き先を名前ごとに
	// 選べる。指定した行き先は SNI/Host の確認だけ続けて素通しする。
	passthrough, err := normalizePassthrough(cfg.Guard.PassthroughHTTPS)
	if err != nil {
		return err
	}
	if len(passthrough) > 0 && !inspectHTTPS {
		return fmt.Errorf("guard.passthrough_https を使うには guard.inspect_https を true にする")
	}
	if o.Agent == "" {
		o.Agent = DefaultAgent
	}
	// VM の中のコマンドにかける一枚 (seccomp / Landlock)。未指定なら既定で有効。
	sb := cfg.Sandbox
	if sb == nil {
		sb = sandbox.Default()
	}
	if err := sb.Validate(); err != nil {
		return err
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
	var tmpDir string
	var disks []vm.DataDisk
	if o.MountTmp {
		dir, img, err := prepareTmp(repo, work)
		if err != nil {
			return err
		}
		tmpDir = dir
		disks = append(disks, vm.DataDisk{Serial: tmpSerial, Path: img})
		logf("%s を VM の /work/.tmp (容量 %d MiB) と受け渡す (終了時に回収)", dir, tmpDiskSize>>20)
	}

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
	// 窓口 (vsock) へのリクエストを host 信頼で記録する。VM が何を host に求めたかは
	// VM の中の記録と違い改変されない (host.log に残る)。
	svc.Audit = func(e hostsvc.AuditEvent) {
		logger.Print(e.LogLine())
	}
	// 許可していない LLM API の操作は、承認コンソールができてからそこに出す
	llmDenied := make(chan string, 16)
	// コンテンツガード (ローカル LLM)。承認コンソールはこの後で作るので、Reviewer は後から差す。
	var contentGuard *guard.Guard
	if cfg.Guard.Enabled {
		contentGuard, err = guard.New(cfg.Guard, logger)
		if err != nil {
			return fmt.Errorf("コンテンツガードを作れない: %w", err)
		}
		logf("コンテンツガード: %s で LLM プロキシのリクエストを点検する", contentGuard)
	}
	// HTTPS も終端して点検するなら、run ごとの使い捨て CA を 1 つ作る。証明書は
	// guest の信頼ストアに入れ、秘密鍵は host の作業ディレクトリ (0700) から出さない。
	var ca *tlsmitm.CA
	var caCert string
	if inspectHTTPS {
		ca, err = tlsmitm.NewCA()
		if err != nil {
			return fmt.Errorf("TLS 終端の CA を作れない: %w", err)
		}
		caCert = string(ca.CertPEM())
		if len(passthrough) > 0 {
			logf("HTTPS の中身も点検する (使い捨て CA で TLS を終端。%s は終端せず素通し)", strings.Join(passthrough, ", "))
		} else {
			logf("HTTPS の中身も点検する (使い捨て CA で TLS を終端。証明書を固定するクライアントは passthrough_https で除外する)")
		}
	}
	providers, err := authproxy.Register(svc.Mux, cfg.Providers, logger, func(s string) {
		select {
		case llmDenied <- s:
		default: // 溢れた分は host.log にだけ残る
		}
	}, contentGuard)
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
	tmpCmd := ""
	if tmpDir != "" {
		tmpCmd = tmpRuncmd(strconv.Itoa(vm.GuestUID))
	}
	// HTTPS を終端するときは、使い捨て CA を guest の信頼ストアに入れ、自前の
	// 証明書束を見る実装 (Node / Bun など) にも環境変数で教える。信頼を入れてから
	// エージェントを起動する (runcmd の先頭で update-ca-certificates)。
	entrypoint := ag.entrypoint
	extraFiles := ""
	trustCmd := ""
	if caCert != "" {
		entrypoint, extraFiles, trustCmd = injectCA(entrypoint, caCert)
	}
	// VM の中の一枚 (seccomp / Landlock) の方針を cloud-init で置く。root 所有 0644
	// なので、作業ユーザーは読めるが書き換えられない。受け口 (quagent-guest) が
	// 起動時に読み、以降のコマンドを起動役経由で起動する。
	sandboxFile := ""
	if sb.On() {
		sbJSON, err := sb.JSON()
		if err != nil {
			return err
		}
		sandboxFile = sandboxWriteFile(sbJSON)
		note := ""
		if sb.Landlock {
			note = " + Landlock"
		}
		logf("sandbox: VM 内のコマンドに %s の方針をかける (seccomp%s)", sb.Mode, note)
	} else {
		logf("sandbox: 無効")
	}
	userData := fmt.Sprintf(`#cloud-config
write_files:
  # エージェントの起動はここにまとめる (↑ で呼び戻せる)
  - path: /entrypoint.sh
    permissions: '0755'
    content: |
%s
%s
%s
bootcmd:
  - [mkdir, -p, /etc/quagent]
  - echo '127.0.0.1 %s' >> /etc/hosts
  # 外へは出られないので NTP は使えない (時計は KVM が合わせる)。拒否の記録が並ぶだけなので止める
  - [sh, -c, "systemctl mask --now systemd-timesyncd.service 2>/dev/null; true"]
  - [sh, -c, "%s"]
runcmd:
%s%s  - [sh, -c, "mkdir -p /run/quagent-seed && mount -o ro /dev/disk/by-label/cidata /run/quagent-seed && install -m 755 /run/quagent-seed/quagent-guest /usr/local/bin/quagent-guest && umount /run/quagent-seed"]
  - [systemd-run, --unit=quagent-guest, --uid=%s, -p, Restart=always, /usr/local/bin/quagent-guest, %s, "%d"]
`, indentBlock(entrypoint, "      "), extraFiles, sandboxFile, hostsvc.GuestHost, maskCmd(sshUnits(o.SSH)), trustCmd, tmpCmd, vm.GuestUser, guestCommand, svc.Port)
	// 時刻の表示 (承認の期限やコミットの日時) を host とそろえる
	if tz := hostTimezone(); tz != "" {
		userData += "timezone: " + tz + "\n"
	}
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
	qemu := vm.QemuArgv(vm.QemuOpts{
		Disk: overlay, Seed: seed, CPUs: o.CPUs, MemMiB: o.MemMiB,
		ConsoleLog: filepath.Join(work, "console.log"),
		Netdev:     netdev, VsockCID: g.cid, DataDisks: disks, NestedVirt: o.NestedVirt,
	})
	logf("VM を起動 (base=%s, allow=%v)", filepath.Base(base), o.Allow)
	spec := netns.Spec{WorkDir: work, SSHPort: sshPort, DNS: dns, Allow: o.Allow, QemuArgv: qemu}
	if inspectHTTPS {
		keyPEM, err := ca.KeyPEM()
		if err != nil {
			return fmt.Errorf("TLS 終端の CA の鍵を書き出せない: %w", err)
		}
		spec.InspectHTTPS = true
		spec.CACertPEM, spec.CAKeyPEM = caCert, string(keyPEM)
		spec.InspectLimit = contentGuard.InspectLimit()
		spec.PassthroughHTTPS = passthrough
		if len(passthrough) > 0 {
			logf("TLS 終端しない行き先: %s (SNI/Host の確認だけ続けて素通しする)", strings.Join(passthrough, ", "))
		}
	}
	l, err := netns.Start(spec)
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
	if contentGuard != nil {
		// 疑わしいリクエストは承認コンソールで人間が通すか止めるか決める
		contentGuard.SetReviewer(func(ctx context.Context, req guard.Request, reason string) error {
			return con.AskGuard(ctx, console.GuardInfo{
				Provider: req.Provider,
				Method:   req.Method,
				URL:      req.URL(),
				Reason:   reason,
				Evidence: req.Evidence,
				Headers:  req.HeaderLines(),
				Body:     string(req.Body),
			})
		})
		// 最初の本番リクエストがモデルの読み込み待ちで時間切れにならないよう先に載せる
		go contentGuard.Warm(context.Background())
	}
	if inspectHTTPS {
		// 子が TLS 終端して取り出した HTTPS のリクエストを、同じコンテンツガードにかける。
		// 認証プロキシと同じ Guard を使うので、拒否した該当箇所の記憶も共有される。
		l.SetInspector(func(req netns.InspectRequest) error {
			return contentGuard.Check(context.Background(), guard.Request{
				Provider:      req.Provider,
				Method:        req.Method,
				Host:          req.Host,
				Path:          req.Path,
				Query:         req.Query,
				Headers:       req.Headers,
				Body:          req.Body,
				BodyTruncated: req.Truncated,
			})
		})
	}
	go relayDenied(l, con)
	go relayBlocked(l, con)
	go func() {
		// 連打で承認コンソールを埋めないよう 1 分に 10 件まで (残りは host.log にある)
		var window time.Time
		shown := 0
		for s := range llmDenied {
			if now := time.Now(); now.Sub(window) >= time.Minute {
				window, shown = now, 0
			}
			if shown++; shown <= 10 {
				con.Log("LLM プロキシで拒否: " + s)
			}
		}
	}()
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
	if o.PRApproval {
		publisher.Approve = func(req pr.Request) error {
			return con.AskPR(console.PRInfo{Branch: req.Branch, Base: req.Base, Title: req.Title, Body: req.Body})
		}
	}
	svc.Mux.Handle(mcpsrv.Path, mcpsrv.Handler(mgr, publisher, contentGuard, con.Log))

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
	if err := copyRepo(g, repo, work, markKey, o.LocalHead); err != nil {
		return err
	}
	if tmpDir != "" {
		if err := copyInTmp(g, tmpDir); err != nil {
			return err
		}
		defer finishTmp(g, tmpDir, work)
	}
	if err := ag.setup(g, cfg, providers, svc.Token); err != nil {
		return err
	}
	if err := setupHistory(g); err != nil {
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

	agent := g.interactiveArgv(paneCommand)
	session := "quagent-" + filepath.Base(work)
	return runTmux(session, agent, []string{self, consoleCommand, filepath.Join(work, "console.sock")}, con.Quit)
}

// finishTmp は VM の /work/.tmp を host の .tmp へ回収する。回収できなければ
// ディスクのイメージをログと一緒に残す (中身を手で取り出せるように)。
func finishTmp(g vmGuest, dir, work string) {
	files, size, err := collectTmp(g, dir)
	if err == nil {
		logf("VM の /work/.tmp から %d 件 (%d KiB) を %s へ回収した", files, (size+1023)>>10, dir)
		return
	}
	keep := filepath.Join(paths.LogsDir(), filepath.Base(work))
	if merr := os.MkdirAll(keep, 0o700); merr == nil {
		if rerr := os.Rename(filepath.Join(work, "tmp.img"), filepath.Join(keep, "tmp.img")); rerr == nil {
			logf("VM の /work/.tmp を回収できなかった (%v)。%d 件は回収済み。ディスクを %s に残した (ext4)", err, files, filepath.Join(keep, "tmp.img"))
			return
		}
	}
	logf("VM の /work/.tmp を回収できなかった: %v (%d 件は回収済み)", err, files)
}

// hostTimezone は host のタイムゾーン名 (例: Asia/Tokyo)。分からなければ空。
// tz database の名前でないもの ("JST-9" など) は VM で設定できないので使わない。
func hostTimezone() string {
	tz := strings.TrimPrefix(os.Getenv("TZ"), ":")
	if tz == "" {
		link, err := os.Readlink("/etc/localtime")
		if err != nil {
			return ""
		}
		_, tz, _ = strings.Cut(link, "zoneinfo/")
	}
	if tz == "" || strings.HasPrefix(tz, "/") || strings.Contains(tz, "..") || strings.ContainsAny(tz, " \t\n\"'") {
		return ""
	}
	if fi, err := os.Stat(filepath.Join("/usr/share/zoneinfo", tz)); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	return tz
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

// newCID は vsock の guest CID を選ぶ (3 以上。host 上で他の VM と被らないよう乱数)。
func newCID() uint32 {
	return 3 + uint32(rand.Int64N(1<<31-3))
}

// relayDenied は DNS で拒否したドメインを承認コンソールに流す。
func relayDenied(l *netns.Launcher, con *console.Server) {
	relayNames(l.Denied, con, "DNS で拒否: ")
}

// relayBlocked は透明プロキシが許可外の名前 (SNI/Host) で止めた Web 接続を流す。
func relayBlocked(l *netns.Launcher, con *console.Server) {
	relayNames(l.Blocked, con, "Web で拒否: ")
}

// relayNames は名前の連打を抑えて承認コンソールに流す (同じ名前は 1 分に 1 回、
// 全体でも 1 分に 10 件まで)。
func relayNames(ch <-chan string, con *console.Server, prefix string) {
	last := map[string]time.Time{}
	var window time.Time
	var shown, dropped int
	for name := range ch {
		now := time.Now()
		if now.Sub(window) >= time.Minute {
			if dropped > 0 {
				con.Log(fmt.Sprintf("%sほか %d 件 (多すぎるので省略)", prefix, dropped))
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
		con.Log(prefix + name)
	}
}

// holdWindowName は pane のある窓の automatic-rename をいったん止め、元に戻す関数を返す。
func holdWindowName(pane string) func() {
	if pane == "" {
		return nil
	}
	out, err := exec.Command("tmux", "display-message", "-p", "-t", pane, "#{window_id}").Output()
	win := strings.TrimSpace(string(out))
	if err != nil || win == "" {
		return nil
	}
	// 窓ごとの設定 (-w、-g なし) があれば値を覚えておき、無ければ戻すときに外す
	local, _ := exec.Command("tmux", "show-options", "-wqv", "-t", win, "automatic-rename").Output()
	if exec.Command("tmux", "set-option", "-w", "-t", win, "automatic-rename", "off").Run() != nil {
		return nil
	}
	return func() {
		if v := strings.TrimSpace(string(local)); v != "" {
			_ = exec.Command("tmux", "set-option", "-w", "-t", win, "automatic-rename", v).Run()
		} else {
			_ = exec.Command("tmux", "set-option", "-wu", "-t", win, "automatic-rename").Run()
		}
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
	if err := tmux("new-session", "-d", "-s", session, "-n", "quagent", "-x", "200", "-y", "50", "sh", "-c", agentSh); err != nil {
		return err
	}
	defer func() { _ = exec.Command("tmux", "kill-session", "-t", session).Run() }()
	// 窓の名前や外の端末のタイトルを書き換えたり、tmux を素通りして外の端末へ送ったり
	// させない (ペインのタイトルはエージェントが変えてよい)。$TMUX の中から使うと利用者の
	// tmux にこのセッションが出るので、どれもこのセッション・窓だけに設定する。
	for _, opt := range [][]string{
		{"-w", "allow-rename"}, {"-w", "automatic-rename"},
		{"-w", "allow-passthrough"}, {"", "set-titles"},
	} {
		target := session
		args := []string{"set-option"}
		if opt[0] != "" {
			target += ":"
			args = append(args, opt[0])
		}
		if err := tmux(append(args, "-t", target, opt[1], "off")...); err != nil {
			logf("%v", err)
		}
	}
	// セッションが終わったら、利用者の tmux クライアントは元のセッションへ戻す
	// (既定の detach-on-destroy on だと、$TMUX の中から使ったときに利用者の tmux が閉じる)
	if err := tmux("set-option", "-t", session, "detach-on-destroy", "previous"); err != nil {
		logf("%v", err)
		_ = tmux("set-option", "-t", session, "detach-on-destroy", "off")
	}
	if err := tmux(append([]string{"split-window", "-v", "-l", "30%", "-t", session + ":"}, consoleArgv...)...); err != nil {
		return err
	}
	_ = tmux("select-pane", "-t", session+":.0")

	logf("tmux セッション %s に接続 (エージェントを終了すると VM を破棄)", session)
	attached := make(chan struct{})
	if os.Getenv("TMUX") != "" {
		close(attached)
		// 起動した窓では quagent が前面のコマンドになるので、automatic-rename で窓の
		// 名前が quagent に変わらないよう、終わるまでその窓だけ止める
		if restore := holdWindowName(os.Getenv("TMUX_PANE")); restore != nil {
			defer restore()
		}
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

// copyRepo は host で checkout しているブランチを履歴ごと (git bundle) VM の /work に
// 取り込み、同じ名前で checkout する。渡すのはそのブランチと origin の remote-tracking
// (origin に公開済みのもの) だけで、ほかのローカルブランチやタグは渡さない。ブランチの
// 先頭は既定では upstream (fetch 済みのもの) で、未 push のコミットは VM に見せない
// (localHead のときだけローカルの先頭)。未コミットの変更も渡らない。/work が空でなくても
// 取り込めるよう clone ではなく init + fetch にする (.tmp のマウント先があってもよい)。
func copyRepo(g vmGuest, repo, work, markKey string, localHead bool) error {
	src, dst, checkout, err := pickHead(repo, localHead)
	if err != nil {
		return err
	}
	bundle := filepath.Join(work, "repo.bundle")
	if out, err := exec.Command("git", "-C", repo, "bundle", "create", "-q", bundle, src, "--remotes=origin").CombinedOutput(); err != nil {
		return fmt.Errorf("git bundle に失敗: %v: %s", err, out)
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
git fetch -q /tmp/repo.bundle '+refs/remotes/*:refs/remotes/*' ` + shellQuote("+"+src+":"+dst) + `
git checkout -q ` + checkout + `
rm /tmp/repo.bundle`
	if out, err := g.sh(script, nil); err != nil {
		return fmt.Errorf("VM 内での取り込みに失敗: %v: %s", err, out)
	}
	return nil
}

// pickHead は VM に渡す先頭を決める。src は host の ref (bundle に入れるもの)、dst は
// VM で受ける ref、checkout は VM で git checkout に渡す引数。
func pickHead(repo string, localHead bool) (src, dst, checkout string, err error) {
	git := func(args ...string) string {
		out, _ := exec.Command("git", append([]string{"-C", repo}, args...)...).Output()
		return strings.TrimSpace(string(out))
	}
	ref := git("symbolic-ref", "-q", "HEAD")
	if !strings.HasPrefix(ref, "refs/heads/") {
		// detached HEAD: そのコミットを渡す。既定では origin などに公開済みのコミットに限る
		if !localHead && git("for-each-ref", "--count=1", "--contains", "HEAD", "refs/remotes") == "" {
			return "", "", "", fmt.Errorf("HEAD のコミットはどのリモートにも無い (未 push)。VM に渡すなら --local-head を付ける")
		}
		return "HEAD", "refs/quagent/head", "--detach refs/quagent/head", nil
	}
	name := strings.TrimPrefix(ref, "refs/heads/")
	if localHead {
		logf("%s をローカルの先頭で VM に渡す (未 push のコミットも渡る)", name)
		return ref, ref, shellQuote(name), nil
	}
	up := git("rev-parse", "--symbolic-full-name", name+"@{upstream}")
	if !strings.HasPrefix(up, "refs/remotes/") {
		return "", "", "", fmt.Errorf("%s に upstream (リモートのブランチ) が無い。ローカルの先頭を VM に渡すなら --local-head を付ける", name)
	}
	if n := git("rev-list", "--count", up+"..HEAD"); n != "" && n != "0" {
		logf("%s の未 push のコミット %s 件は VM に渡さない (渡すなら --local-head)", name, n)
	}
	return up, ref, shellQuote(name), nil
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

// indentBlock は s の各行を prefix で字下げする (cloud-config のブロックスカラー用)。
func indentBlock(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = prefix + line
		}
	}
	return strings.Join(lines, "\n")
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
