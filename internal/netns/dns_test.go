package netns

import (
	"net"
	"strings"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func buildQuery(name string, qtype dnsmessage.Type) []byte {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:               0x1234,
		RecursionDesired: true,
	})
	_ = b.StartQuestions()
	_ = b.Question(dnsmessage.Question{
		Name:  dnsmessage.MustNewName(name + "."),
		Type:  qtype,
		Class: dnsmessage.ClassINET,
	})
	msg, _ := b.Finish()
	return msg
}

func parseRCode(t *testing.T, resp []byte) dnsmessage.RCode {
	t.Helper()
	var p dnsmessage.Parser
	hdr, err := p.Start(resp)
	if err != nil {
		t.Fatalf("parse response header failed: %v", err)
	}
	return hdr.RCode
}

func TestDNSHandleDenied(t *testing.T) {
	var deniedName string
	srv := &dnsServer{
		allowed: func(name string) bool {
			return name == "allowed.com"
		},
		onDenied: func(name string) {
			deniedName = name
		},
	}

	q := buildQuery("blocked.com", dnsmessage.TypeA)
	resp := srv.handle(q, "udp")
	if resp == nil {
		t.Fatal("handle returned nil response")
	}

	if got := parseRCode(t, resp); got != dnsmessage.RCodeRefused {
		t.Errorf("RCode = %v, want RCodeRefused", got)
	}
	if deniedName != "blocked.com" {
		t.Errorf("deniedName = %q, want blocked.com", deniedName)
	}
}

func TestDNSHandleAAAA(t *testing.T) {
	srv := &dnsServer{
		allowed: func(name string) bool { return true },
	}

	q := buildQuery("allowed.com", dnsmessage.TypeAAAA)
	resp := srv.handle(q, "udp")
	if resp == nil {
		t.Fatal("handle returned nil response")
	}

	if got := parseRCode(t, resp); got != dnsmessage.RCodeSuccess {
		t.Errorf("RCode = %v, want RCodeSuccess", got)
	}
}

func TestDNSHandleUpstreamFail(t *testing.T) {
	srv := &dnsServer{
		upstream: "127.0.0.1",
		allowed:  func(name string) bool { return true },
		onAnswer: func(name string, ips []net.IP) {},
	}

	q := buildQuery("allowed.com", dnsmessage.TypeA)
	resp := srv.handle(q, "tcp")
	if resp == nil {
		t.Fatal("handle returned nil response")
	}

	if got := parseRCode(t, resp); got != dnsmessage.RCodeServerFailure {
		t.Errorf("RCode = %v, want RCodeServerFailure", got)
	}
}

func TestDNSHandleMalformed(t *testing.T) {
	srv := &dnsServer{}

	// 1. 完全なゴミ
	if got := srv.handle([]byte("garbage"), "udp"); got != nil {
		t.Errorf("expected nil for garbage query, got %v", got)
	}

	// 2. 質問が0個のクエリ
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 0x1234})
	msg, _ := b.Finish()
	resp := srv.handle(msg, "udp")
	if resp == nil {
		t.Fatal("expected reply for query with no questions")
	}
	if got := parseRCode(t, resp); got != dnsmessage.RCodeFormatError {
		t.Errorf("RCode = %v, want RCodeFormatError", got)
	}
}

func TestDNSReply(t *testing.T) {
	hdr := dnsmessage.Header{ID: 42, RecursionDesired: true}
	qs := []dnsmessage.Question{
		{
			Name:  dnsmessage.MustNewName("test.example.com."),
			Type:  dnsmessage.TypeA,
			Class: dnsmessage.ClassINET,
		},
	}

	out := reply(hdr, qs, dnsmessage.RCodeNameError)
	if out == nil {
		t.Fatal("reply returned nil")
	}

	var p dnsmessage.Parser
	respHdr, err := p.Start(out)
	if err != nil {
		t.Fatalf("parse reply failed: %v", err)
	}
	if respHdr.ID != 42 || !respHdr.Response || respHdr.RCode != dnsmessage.RCodeNameError {
		t.Errorf("reply header mismatch: %+v", respHdr)
	}
}

func TestDetectTunneling(t *testing.T) {
	tests := []struct {
		name        string
		query       string
		qtype       dnsmessage.Type
		wantBlocked bool
		wantReason  string
	}{
		{
			name:        "Normal query",
			query:       "api.github.com",
			qtype:       dnsmessage.TypeA,
			wantBlocked: false,
		},
		{
			name:        "Overly long query (>180 chars)",
			query:       strings.Repeat("a", 40) + "." + strings.Repeat("b", 40) + "." + strings.Repeat("c", 40) + "." + strings.Repeat("d", 40) + "." + strings.Repeat("e", 30) + ".example.com",
			qtype:       dnsmessage.TypeA,
			wantBlocked: true,
			wantReason:  "クエリ名総長過大",
		},
		{
			name:        "Overly long single label (>45 chars)",
			query:       "abcdefghijklmnopqrstuvwxyz0123456789abcdefghijkl.example.com",
			qtype:       dnsmessage.TypeA,
			wantBlocked: true,
			wantReason:  "ラベル長過大",
		},
		{
			name:        "Excessive subdomain depth (>6 levels)",
			query:       "a.b.c.d.e.f.g.example.com",
			qtype:       dnsmessage.TypeA,
			wantBlocked: true,
			wantReason:  "サブドメイン階層過大",
		},
		{
			name:        "High entropy label (>4.2 bits/char, len >= 25)",
			query:       "v28q9x7zp4m1c5j8k2t6r3y0f.example.com",
			qtype:       dnsmessage.TypeA,
			wantBlocked: true,
			wantReason:  "高エントロピーラベル検知",
		},
		{
			name:        "Suspicious query type TXT with long label",
			query:       "data-exfiltration-payload-chunk-01.example.com",
			qtype:       dnsmessage.TypeTXT,
			wantBlocked: true,
			wantReason:  "異常なクエリタイプ",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			blocked, reason := detectTunneling(tc.query, tc.qtype)
			if blocked != tc.wantBlocked {
				t.Fatalf("detectTunneling(%q) blocked=%v, want %v (reason=%s)", tc.query, blocked, tc.wantBlocked, reason)
			}
			if tc.wantBlocked && !strings.Contains(reason, tc.wantReason) {
				t.Errorf("reason = %q, want substring %q", reason, tc.wantReason)
			}
		})
	}
}

func TestDNSHandleTunnelingBlocked(t *testing.T) {
	var blockedName, blockedReason string
	srv := &dnsServer{
		allowed: func(name string) bool { return true },
		onTunnelBlocked: func(name, reason string) {
			blockedName = name
			blockedReason = reason
		},
	}

	// 高エントロピーなトンネリングクエリ
	q := buildQuery("v28q9x7zp4m1c5j8k2t6r3y0f.example.com", dnsmessage.TypeA)
	resp := srv.handle(q, "udp")
	if resp == nil {
		t.Fatal("handle returned nil")
	}

	if got := parseRCode(t, resp); got != dnsmessage.RCodeRefused {
		t.Fatalf("RCode = %v, want RCodeRefused", got)
	}
	if blockedName != "v28q9x7zp4m1c5j8k2t6r3y0f.example.com" {
		t.Errorf("blockedName = %q", blockedName)
	}
	if !strings.Contains(blockedReason, "高エントロピー") {
		t.Errorf("blockedReason = %q", blockedReason)
	}
}
