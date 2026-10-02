package netns

import (
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"
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

func TestEgressCNAMEFollowsParent(t *testing.T) {
	e, _ := newTestEgress([]Grant{{Pattern: "deb.debian.org"}})
	if e.allowed("cdn.fastly.net") {
		t.Fatal("CNAME を見る前から許可されている")
	}
	e.onAnswer("deb.debian.org", nil, []string{"cdn.fastly.net"})
	if !e.allowed("cdn.fastly.net") {
		t.Fatal("許可した名前の CNAME 先が許可されない")
	}
	e.setGrants(nil)
	if e.allowed("cdn.fastly.net") {
		t.Fatal("元の許可を外しても CNAME 先が許可されたまま")
	}
}

func TestEgressSetFollowsGrants(t *testing.T) {
	e, scripts := newTestEgress([]Grant{{Pattern: "a.example"}, {Pattern: "b.example"}})
	e.onAnswer("a.example", []net.IP{net.ParseIP("192.0.2.1"), net.ParseIP("192.0.2.9")}, nil)
	e.onAnswer("b.example", []net.IP{net.ParseIP("192.0.2.9")}, nil)
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
	e.onAnswer("a.example", []net.IP{net.ParseIP("192.0.2.1")}, nil)
	if !strings.Contains(lastSet(*scripts), "192.0.2.1") {
		t.Fatal("有効な許可の IP が set に無い")
	}
	e.grants[0].Expires = time.Now().Add(-time.Second).Unix()
	e.prune()
	if s := lastSet(*scripts); strings.Contains(s, "192.0.2.1") {
		t.Fatalf("期限切れ後も IP が set に残る: %q", s)
	}
}
