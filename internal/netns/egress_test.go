package netns

import (
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestMatches(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"example.com", "example.com", true},
		{"example.com", "www.example.com", false},
		{"*.example.com", "www.example.com", true},
		{"*.example.com", "a.b.example.com", true},
		{"*.example.com", "example.com", false},
		{"*.example.com", "badexample.com", false},
		{"Example.COM.", "example.com", true},
	}
	for _, c := range cases {
		if got := Matches(c.pattern, c.name); got != c.want {
			t.Errorf("Matches(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func newTestEgress(initial []Grant) (*egress, *[]string) {
	var scripts []string
	nft := func(s string) error { scripts = append(scripts, s); return nil }
	return newEgress(nft, &eventWriter{enc: json.NewEncoder(io.Discard)}, initial), &scripts
}

func lastSet(scripts []string) string {
	if len(scripts) == 0 {
		return ""
	}
	return scripts[len(scripts)-1]
}

func TestEgressCNAMETargetNotAllowed(t *testing.T) {
	e, scripts := newTestEgress([]Grant{{Pattern: "deb.debian.org"}})
	// 応答の A は CNAME の先のものでも、問い合わせた名前の分として通る
	e.onAnswer("deb.debian.org", []net.IP{net.ParseIP("192.0.2.1")})
	if !strings.Contains(lastSet(*scripts), "192.0.2.1") {
		t.Fatalf("許可した名前の応答の IP が set に無い: %q", lastSet(*scripts))
	}
	// CNAME の先の名前そのものは許可していない
	if e.allowed("cdn.fastly.net") {
		t.Fatal("許可していない CNAME の先が許可された")
	}
}

func TestEgressRejectsInternalIPs(t *testing.T) {
	for _, bad := range []string{
		"169.254.169.254", "10.0.0.1", "172.16.0.1", "192.168.1.1", "127.0.0.1",
		"100.100.100.100", "0.0.0.0", "224.0.0.1", "255.255.255.255",
	} {
		e, scripts := newTestEgress([]Grant{{Pattern: "approved.example"}})
		e.onAnswer("approved.example", []net.IP{net.ParseIP(bad), net.ParseIP("192.0.2.1")})
		if s := lastSet(*scripts); strings.Contains(s, bad) || !strings.Contains(s, "192.0.2.1") {
			t.Errorf("%s: set = %q", bad, s)
		}
	}
}

func TestAnswersFollowsOwnerChain(t *testing.T) {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{Response: true})
	_ = b.StartQuestions()
	_ = b.StartAnswers()
	hdr := func(name string, typ dnsmessage.Type) dnsmessage.ResourceHeader {
		return dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET, TTL: 60}
	}
	_ = b.CNAMEResource(hdr("www.example.", dnsmessage.TypeCNAME), dnsmessage.CNAMEResource{CNAME: dnsmessage.MustNewName("cdn.example.")})
	_ = b.AResource(hdr("CDN.example.", dnsmessage.TypeA), dnsmessage.AResource{A: [4]byte{192, 0, 2, 1}})
	// 鎖に無い名前の A は混ぜられても使わない
	_ = b.AResource(hdr("other.example.", dnsmessage.TypeA), dnsmessage.AResource{A: [4]byte{192, 0, 2, 9}})
	msg, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	ips := answers(msg, "www.example")
	if len(ips) != 1 || ips[0].String() != "192.0.2.1" {
		t.Fatalf("answers = %v", ips)
	}
}

func TestEgressSetFollowsGrants(t *testing.T) {
	e, scripts := newTestEgress([]Grant{{Pattern: "a.example"}, {Pattern: "b.example"}})
	e.onAnswer("a.example", []net.IP{net.ParseIP("192.0.2.1"), net.ParseIP("192.0.2.9")})
	e.onAnswer("b.example", []net.IP{net.ParseIP("192.0.2.9")})
	if s := lastSet(*scripts); !strings.Contains(s, "192.0.2.1") || !strings.Contains(s, "192.0.2.9") {
		t.Fatalf("set に両方の IP が無い: %q", s)
	}
	// a を外しても、b と共有している IP は残る
	e.setGrants([]Grant{{Pattern: "b.example"}})
	if s := lastSet(*scripts); strings.Contains(s, "192.0.2.1") || !strings.Contains(s, "192.0.2.9") {
		t.Fatalf("共有 IP の扱いが誤り: %q", s)
	}
}

func TestEgressExpiry(t *testing.T) {
	e, scripts := newTestEgress([]Grant{{Pattern: "a.example", Expires: time.Now().Add(-time.Second).Unix()}})
	if e.allowed("a.example") {
		t.Fatal("期限切れの許可が有効")
	}
	e.setGrants([]Grant{{Pattern: "a.example", Expires: time.Now().Add(time.Hour).Unix()}})
	e.onAnswer("a.example", []net.IP{net.ParseIP("192.0.2.1")})
	if !strings.Contains(lastSet(*scripts), "192.0.2.1") {
		t.Fatal("有効な許可の IP が set に無い")
	}
	e.grants[0].Expires = time.Now().Add(-time.Second).Unix()
	e.prune()
	if s := lastSet(*scripts); strings.Contains(s, "192.0.2.1") {
		t.Fatalf("期限切れ後も IP が set に残る: %q", s)
	}
}

func TestWebBlocked(t *testing.T) {
	e, _ := newTestEgress(nil)
	var events []Event
	// イベントリーダー
	r, w := io.Pipe()
	e.events = &eventWriter{enc: json.NewEncoder(w)}
	go func() {
		dec := json.NewDecoder(r)
		for {
			var ev Event
			if err := dec.Decode(&ev); err != nil {
				return
			}
			events = append(events, ev)
		}
	}()

	// 1. 初回呼び出し
	e.webBlocked("test1.com (SNI)")

	// 2. maxDeniedLogs を超えるまで呼ぶ
	for i := 0; i < maxDeniedLogs+5; i++ {
		e.webBlocked("spam.com")
	}

	// 3. 1分経過後の呼び出し (dropped メモのフラッシュ)
	e.mu.Lock()
	e.webWindow = time.Now().Add(-2 * time.Minute)
	e.mu.Unlock()
	e.webBlocked("after-minute.com")

	w.Close()
	// イベントが記録されていること
	if len(events) == 0 {
		t.Fatal("no blocked events recorded")
	}
}

func TestReadControl(t *testing.T) {
	// 1. 正常な control json
	input := `{"Grants":[{"Pattern":"example.com","Expires":12345}]}` + "\n"
	var received []control
	err := readControl(strings.NewReader(input), func(c control) {
		received = append(received, c)
	})
	if err != nil {
		t.Fatalf("readControl failed: %v", err)
	}
	if len(received) != 1 || len(received[0].Grants) != 1 || received[0].Grants[0].Pattern != "example.com" {
		t.Fatalf("unexpected control received: %+v", received)
	}

	// 2. 不正な JSON
	err = readControl(strings.NewReader("invalid-json\n"), func(c control) {})
	if err == nil {
		t.Fatal("expected error on invalid json")
	}
}

