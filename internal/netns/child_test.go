package netns

import (
	"strings"
	"testing"
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
}
