package netns

import (
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 許可した行き先でも、新規接続は Web ポート (80/443) 以外へ張れないこと。
func TestEgressRulesRestrictPorts(t *testing.T) {
	rules := egressRules()
	if !strings.Contains(rules, "ip daddr @allow4 tcp dport { 80, 443 } accept") {
		t.Fatalf("許可 IP への接続に宛先ポートの制限が無い:\n%s", rules)
	}
	// 宛先 IP だけで無条件に通す行が残っていないこと (ポート制限の回避)。
	if strings.Contains(rules, "ip daddr @allow4 accept") {
		t.Fatalf("許可 IP を宛先だけで通す行が残っている:\n%s", rules)
	}
	// loopback と確立済みの接続は今までどおり通す。
	if !strings.Contains(rules, `oifname "lo" accept`) {
		t.Fatalf("loopback が通らない:\n%s", rules)
	}
	if !strings.Contains(rules, "ct state established,related accept") {
		t.Fatalf("確立済みの接続が切れる:\n%s", rules)
	}
}

func TestEgressPortsNoPrivilegedOrExtra(t *testing.T) {
	for _, p := range egressPorts {
		if p != 80 && p != 443 {
			t.Errorf("egressPorts に想定外のポート %d がある", p)
		}
	}
}

// 許可した行き先への Web 接続は透明プロキシ (SNI/Host 点検) を通すこと。
func TestEgressRulesRedirectWebPorts(t *testing.T) {
	rules := egressRules()
	if !strings.Contains(rules, "ip daddr @allow4 tcp dport 443 redirect to :8443") {
		t.Fatalf("443 が透明プロキシへ回らない:\n%s", rules)
	}
	if !strings.Contains(rules, "ip daddr @allow4 tcp dport 80 redirect to :8080") {
		t.Fatalf("80 が透明プロキシへ回らない:\n%s", rules)
	}
	// プロキシ自身の外向き接続 (mark) を redirect しないこと (転送の無限ループ防止)。
	if !strings.Contains(rules, "meta mark 1 return") {
		t.Fatalf("プロキシ自身の接続を redirect から除外していない:\n%s", rules)
	}
	// redirect 後は宛先が loopback のプロキシポートになる。uplink が lo でなくても
	// プロキシへ届くよう、書き換え後の宛先を filter で通すこと (これが無いと
	// 許可した Web 接続がプロキシに届く前に reject される)。
	if !strings.Contains(rules, "ip daddr 127.0.0.1 tcp dport { 8080, 8443 } accept") {
		t.Fatalf("redirect 後の宛先 (loopback のプロキシポート) が通らない:\n%s", rules)
	}
}

func TestWaitFor(t *testing.T) {
	// 1. 即座に true
	if !waitFor(time.Second, func() bool { return true }) {
		t.Fatal("expected waitFor to return true immediately")
	}

	// 2. タイムアウト
	start := time.Now()
	if waitFor(150*time.Millisecond, func() bool { return false }) {
		t.Fatal("expected waitFor to return false on timeout")
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Fatal("waitFor returned too quickly")
	}
}

func TestAddHostfwd(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "slirp.sock")

	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// 1. 正常系: slirp4netns QMP API モック
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 1024)
			n, _ := c.Read(buf)
			if strings.Contains(string(buf[:n]), "add_hostfwd") {
				_, _ = c.Write([]byte(`{"return":{}}`))
			} else {
				_, _ = c.Write([]byte(`{"error":{"desc":"failed"}}`))
			}
			_ = c.Close()
		}
	}()

	if err := addHostfwd(sock, 8080, "10.0.2.15"); err != nil {
		t.Fatalf("addHostfwd failed: %v", err)
	}
}
