package netns

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nananek/quagent/internal/tlsmitm"
)

// RunChild は `unshare -Urm` の内側 (userns root・独自 mount ns) で動く本体。
// stdin で親から許可の一覧を受け取り、stdout で Event を返す。ログは stderr。
func RunChild(specPath string) error {
	// Pdeathsig は子を作ったスレッドの終了で発火するので、メインスレッドに固定する。
	runtime.LockOSThread()
	log.SetPrefix("[quagent:netns] ")
	log.SetFlags(log.Ltime)

	b, err := os.ReadFile(specPath)
	if err != nil {
		return err
	}
	var spec Spec
	if err := json.Unmarshal(b, &spec); err != nil {
		return err
	}

	// qemu の DNS 転送先 (resolv.conf) を、この mount ns の中だけで
	// 子 netns 内の自前 DNS サーバーに向ける。host の resolv.conf には触らない。
	resolv := spec.file("resolv.conf")
	if err := os.WriteFile(resolv, []byte("nameserver 127.0.0.1\n"), 0o644); err != nil {
		return err
	}
	if err := syscall.Mount(resolv, "/etc/resolv.conf", "", syscall.MS_BIND, ""); err != nil {
		return fmt.Errorf("resolv.conf の bind mount に失敗: %w", err)
	}

	c := &child{spec: spec, events: &eventWriter{enc: json.NewEncoder(os.Stdout)}}
	defer c.cleanup()
	return c.run()
}

type child struct {
	spec   Spec
	events *eventWriter
	holder *exec.Cmd
	slirp  *exec.Cmd
	qemu   *exec.Cmd
}

func (c *child) cleanup() {
	for _, p := range []*exec.Cmd{c.qemu, c.slirp, c.holder} {
		if p != nil && p.Process != nil {
			_ = p.Process.Kill()
			_, _ = p.Process.Wait()
		}
	}
}

func deathCmd(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	return cmd
}

// inNS は子 netns 内でコマンドを実行する。
func (c *child) inNS(args ...string) *exec.Cmd {
	pid := strconv.Itoa(c.holder.Process.Pid)
	return exec.Command("nsenter", append([]string{"-t", pid, "-n", "--"}, args...)...)
}

func (c *child) nft(script string) error {
	cmd := c.inNS("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nft: %v: %s", err, out)
	}
	return nil
}

// egressPorts は許可した行き先へ張ってよい新規接続の宛先ポート (TCP)。HTTP/HTTPS
// 以外は通さない。任意ポートへ届く経路を残すと、コンテンツガードの外で持ち出せるうえ、
// IP 単位の許可 (共有 CDN の同じ IP を指す別のホスト) で穴が広がるため。
var egressPorts = []int{80, 443}

// egressRules は子 netns に張る nftables のルールを返す。qemu を起動する前に
// 張るので、guest は最初から閉じた網で起きる。出てよいのは loopback (自前 DNS と
// 透明プロキシ) と、許可ドメインの応答で見た IP への TCP 80/443 だけ (確立済みの
// 接続は切らない)。80/443 は nat で透明プロキシへ回し、接続先が言ってきた名前
// (SNI/Host) が許可名と一致するかを確かめる。プロキシ自身の外向き接続は mark で
// redirect から除く。
//
// nat の redirect は宛先を 127.0.0.1:プロキシポートへ書き換えるが、output の
// oif は uplink (tap0) のままなので、oifname "lo" では拾えない。書き換え後の宛先
// (loopback のプロキシポート) を明示的に通す。これが無いと、許可した IP への
// Web 接続がプロキシに届く前に filter の reject (tcp reset) で切られる。
func egressRules() string {
	ports := make([]string, 0, len(egressPorts))
	redirects := make([]string, 0, len(egressPorts))
	proxyPorts := make([]string, 0, len(egressPorts))
	for _, p := range egressPorts {
		ports = append(ports, strconv.Itoa(p))
		redirects = append(redirects, fmt.Sprintf("    ip daddr @allow4 tcp dport %d redirect to :%d", p, proxyListenPort(p)))
		proxyPorts = append(proxyPorts, strconv.Itoa(proxyListenPort(p)))
	}
	return fmt.Sprintf(`table inet quagent {
  set allow4 { type ipv4_addr; }
  chain output {
    type filter hook output priority 0; policy accept;
    oifname "lo" accept
    ct state established,related accept
    ip daddr @allow4 tcp dport { %s } accept
    ip daddr 127.0.0.1 tcp dport { %s } accept
    meta l4proto tcp counter reject with tcp reset
    counter reject
  }
  chain dstnat {
    type nat hook output priority -100; policy accept;
    meta mark %d return
%s
  }
}
`, strings.Join(ports, ", "), strings.Join(proxyPorts, ", "), proxyMark, strings.Join(redirects, "\n"))
}

// proxyListenPort は宛先ポートに対応する透明プロキシの待ち受けポートを返す。
func proxyListenPort(dst int) int {
	if dst == 443 {
		return proxyTLSPort
	}
	return proxyHTTPPort
}

func (c *child) run() error {
	// 1. 子 netns を保持するプロセス
	c.holder = deathCmd("unshare", "-n", "sh", "-c", "ip link set lo up; exec sleep infinity")
	if err := c.holder.Start(); err != nil {
		return fmt.Errorf("子 netns の作成に失敗: %w", err)
	}
	selfNS, _ := os.Readlink("/proc/self/ns/net")
	nsPath := fmt.Sprintf("/proc/%d/ns/net", c.holder.Process.Pid)
	if !waitFor(5*time.Second, func() bool {
		ns, err := os.Readlink(nsPath)
		return err == nil && ns != selfNS
	}) {
		return fmt.Errorf("子 netns が作成されない")
	}
	// lo が上がるのを待つ (DNS を 127.0.0.1 で待ち受けるため)
	if !waitFor(5*time.Second, func() bool {
		out, err := c.inNS("ip", "-o", "link", "show", "lo").Output()
		return err == nil && bytes.Contains(out, []byte("UP"))
	}) {
		return fmt.Errorf("子 netns の lo が上がらない")
	}

	// 2. egress ルール。qemu を起動する前に張るので、guest は最初から閉じた網で起きる。
	// 出てよいのは loopback (自前 DNS) と allow set (許可ドメインの応答で見た IP) の
	// Web ポートだけ。
	if err := c.nft(egressRules()); err != nil {
		return fmt.Errorf("nftables の適用に失敗: %w", err)
	}
	initial := make([]Grant, 0, len(c.spec.Allow))
	for _, d := range c.spec.Allow {
		initial = append(initial, Grant{Pattern: d})
	}
	eg := newEgress(c.nft, c.events, initial)

	// 親からの指示 (許可の差し替え) を早くから受ける。
	stdinClosed := make(chan error, 1)
	go func() {
		err := readControl(os.Stdin, func(ctl control) {
			eg.setGrants(ctl.Grants)
			c.events.send(Event{Applied: ctl.Seq})
		})
		stdinClosed <- err
	}()

	// 3. 子 netns 内の DNS サーバー (上流へは host の網から出る)
	pc, tl, err := listenInNetns(c.holder.Process.Pid, "127.0.0.1:53")
	if err != nil {
		return fmt.Errorf("DNS の待ち受けに失敗: %w", err)
	}
	dns := &dnsServer{sem: make(chan struct{}, 64), upstream: c.spec.DNS, allowed: eg.allowed, onAnswer: eg.onAnswer, onDenied: eg.denied}
	go dns.serveUDP(pc)
	go dns.serveTCP(tl)

	// 3.5 透明プロキシ: 許可した IP への Web 接続の SNI/Host を確かめる。nft の
	// redirect 先が無いと接続が弾かれるので、qemu を起動する前に待ち受ける。
	// TerminateHTTPS なら TLS を終端し、平文に戻した HTTP のヘッダを絞る。
	web := newWebProxy(c.holder.Process.Pid, eg.allowed, eg.webBlocked)
	if c.spec.TerminateHTTPS {
		ca, err := tlsmitm.FromPEM([]byte(c.spec.CACertPEM), []byte(c.spec.CAKeyPEM))
		if err != nil {
			return fmt.Errorf("TLS 終端の CA を読めない: %w", err)
		}
		web.mitm = ca
		web.passthrough = c.spec.PassthroughHTTPS
	}
	if c.spec.HeaderPolicy != nil && c.spec.HeaderPolicy.Enabled {
		web.headers = c.spec.HeaderPolicy
	}
	for _, lp := range []struct {
		port int
		tls  bool
	}{{proxyTLSPort, true}, {proxyHTTPPort, false}} {
		ln, err := tcpListenerInNetns(c.holder.Process.Pid, fmt.Sprintf("127.0.0.1:%d", lp.port))
		if err != nil {
			return fmt.Errorf("Web プロキシの待ち受けに失敗: %w", err)
		}
		go web.serve(ln, lp.tls)
	}

	// 4. uplink (slirp4netns)。API socket は hostfwd の追加に使う。
	sock := c.spec.file("slirp.sock")
	c.slirp = deathCmd("slirp4netns", "-c", "-m", "65520", "--disable-host-loopback",
		"-a", sock, strconv.Itoa(c.holder.Process.Pid), "tap0")
	c.slirp.Stdout, c.slirp.Stderr = os.Stderr, os.Stderr
	if err := c.slirp.Start(); err != nil {
		return fmt.Errorf("slirp4netns の起動に失敗: %w", err)
	}
	var tapIP string
	if !waitFor(20*time.Second, func() bool {
		out, err := c.inNS("ip", "-4", "-o", "addr", "show", "tap0").Output()
		if err != nil {
			return false
		}
		f := strings.Fields(string(out))
		if len(f) >= 4 {
			tapIP = strings.Split(f[3], "/")[0]
		}
		return tapIP != ""
	}) {
		return fmt.Errorf("tap0 が上がらない")
	}
	log.Printf("子 netns up: tap0=%s", tapIP)

	// 5. ssh を使うときだけ host 127.0.0.1:SSHPort -> 子 netns tapIP:SSHPort (qemu hostfwd が受ける)
	if c.spec.SSHPort != 0 {
		if err := addHostfwd(sock, c.spec.SSHPort, tapIP); err != nil {
			return err
		}
		log.Printf("ssh 中継: 127.0.0.1:%d -> %s:%d", c.spec.SSHPort, tapIP, c.spec.SSHPort)
	}

	// 6. qemu を子 netns で起動
	pid := strconv.Itoa(c.holder.Process.Pid)
	c.qemu = deathCmd("nsenter", append([]string{"-t", pid, "-n", "--"}, c.spec.QemuArgv...)...)
	c.qemu.Stdout, c.qemu.Stderr = os.Stderr, os.Stderr
	if err := c.qemu.Start(); err != nil {
		return fmt.Errorf("qemu の起動に失敗: %w", err)
	}
	if err := os.WriteFile(c.spec.file("ready"), nil, 0o644); err != nil {
		return err
	}
	log.Printf("qemu 起動 (pid=%d)", c.qemu.Process.Pid)

	// 7. 終了待ち: qemu 終了 / 親が stdin を閉じる / シグナル。その間、許可の更新を受ける
	qemuDone := make(chan error, 1)
	go func() { qemuDone <- c.qemu.Wait() }()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case err := <-qemuDone:
			c.qemu = nil
			log.Printf("qemu が終了した (%v)", err)
			return nil
		case err := <-stdinClosed:
			log.Printf("親から終了指示 (%v)", err)
			return nil
		case s := <-sigs:
			log.Printf("シグナル %v で終了", s)
			return nil
		case <-tick.C:
			eg.prune()
		}
	}
}

func addHostfwd(sock string, port int, guestAddr string) error {
	var conn net.Conn
	if !waitFor(5*time.Second, func() bool {
		var err error
		conn, err = net.Dial("unix", sock)
		return err == nil
	}) {
		return fmt.Errorf("slirp4netns の API socket に接続できない")
	}
	defer conn.Close()
	req := map[string]any{
		"execute": "add_hostfwd",
		"arguments": map[string]any{
			"proto": "tcp", "host_addr": "127.0.0.1", "host_port": port,
			"guest_addr": guestAddr, "guest_port": port,
		},
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return err
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 8192)
	n, err := conn.Read(buf)
	if err != nil {
		return fmt.Errorf("add_hostfwd の応答がない: %w", err)
	}
	if !bytes.Contains(buf[:n], []byte(`"return"`)) {
		return fmt.Errorf("add_hostfwd に失敗: %s", buf[:n])
	}
	return nil
}

func waitFor(timeout time.Duration, ok func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
