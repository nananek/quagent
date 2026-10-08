package netns

import (
	"net"
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
