package netns

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
)

// RunChild は `unshare -Urm` の内側 (userns root・独自 mount ns) で動く本体。
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

	// resolv.conf はこの mount ns の中だけで host の実リゾルバに向ける。
	resolv := spec.file("resolv.conf")
	if err := os.WriteFile(resolv, []byte("nameserver "+spec.DNS+"\n"), 0o644); err != nil {
		return err
	}
	if err := syscall.Mount(resolv, "/etc/resolv.conf", "", syscall.MS_BIND, ""); err != nil {
		return fmt.Errorf("resolv.conf の bind mount に失敗: %w", err)
	}

	c := &child{spec: spec}
	defer c.cleanup()
	return c.run()
}

type child struct {
	spec   Spec
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

	// 2. uplink (slirp4netns)。API socket は hostfwd の追加に使う。
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

	// 3. egress ルール。qemu を起動する前に張るので、guest は最初から閉じた網で起きる。
	if err := c.applyRules(); err != nil {
		return err
	}
	c.refreshAllow()

	// 4. host 127.0.0.1:SSHPort -> 子 netns tapIP:SSHPort (qemu hostfwd が受ける)
	if err := addHostfwd(sock, c.spec.SSHPort, tapIP); err != nil {
		return err
	}
	log.Printf("ssh 中継: 127.0.0.1:%d -> %s:%d", c.spec.SSHPort, tapIP, c.spec.SSHPort)

	// 5. qemu を子 netns で起動
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

	// 6. 終了待ち: qemu 終了 / 親が stdin を閉じる / シグナル
	qemuDone := make(chan error, 1)
	go func() { qemuDone <- c.qemu.Wait() }()
	stdinClosed := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, os.Stdin); close(stdinClosed) }()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	tick := time.NewTicker(60 * time.Second)
	defer tick.Stop()
	for {
		select {
		case err := <-qemuDone:
			c.qemu = nil
			log.Printf("qemu が終了した (%v)", err)
			return nil
		case <-stdinClosed:
			log.Printf("親から終了指示")
			return nil
		case s := <-sigs:
			log.Printf("シグナル %v で終了", s)
			return nil
		case <-tick.C:
			// CDN の IP は流動するので定期的に引き直す (暫定。DNS 連動にする予定)
			c.refreshAllow()
		}
	}
}

func (c *child) applyRules() error {
	dns := c.spec.DNS
	rules := fmt.Sprintf(`table inet quagent {
  set allow4 { type ipv4_addr; flags interval; }
  chain output {
    type filter hook output priority 0; policy accept;
    oifname "lo" accept
    ct state established,related accept
    ip daddr %[1]s udp dport 53 accept
    ip daddr %[1]s tcp dport 53 accept
    ip daddr 10.0.2.0/24 accept
    ip daddr @allow4 tcp dport 443 accept
    ip daddr @allow4 udp dport 443 accept
    counter reject
  }
}
`, dns)
	cmd := c.inNS("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(rules)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nftables の適用に失敗: %v: %s", err, out)
	}
	log.Printf("egress ルール適用 (allow=%v)", c.spec.Allow)
	return nil
}

func (c *child) refreshAllow() {
	for _, d := range c.spec.Allow {
		out, err := c.inNS("getent", "ahosts", d).Output()
		if err != nil {
			log.Printf("名前解決に失敗: %s", d)
			continue
		}
		sc := bufio.NewScanner(bytes.NewReader(out))
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) < 2 || f[1] != "STREAM" {
				continue
			}
			ip := net.ParseIP(f[0])
			if ip == nil || ip.To4() == nil {
				continue
			}
			add := c.inNS("nft", "add", "element", "inet", "quagent", "allow4", "{ "+ip.String()+" }")
			if out, err := add.CombinedOutput(); err != nil {
				log.Printf("allow4 への追加に失敗 %s: %v: %s", ip, err, out)
			}
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
