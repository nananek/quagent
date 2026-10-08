package headerpolicy

import (
	"net/http"
	"reflect"
	"testing"
)

func TestApplyDropsUnknownAndFixesUserAgent(t *testing.T) {
	p := &Policy{Enabled: true}
	h := http.Header{}
	h.Set("User-Agent", "me@example.com")
	h.Set("Accept", "*/*")
	h.Set("Content-Type", "application/json")
	h.Set("Referer", "https://x.example/")
	h.Set("Cookie", "a=b")
	h.Set("X-Custom", "1")
	h.Set("Sec-WebSocket-Key", "k")
	dropped := p.Rules("a.example").Apply(h)

	if got := h.Get("User-Agent"); got != DefaultUserAgent {
		t.Fatalf("User-Agent = %q", got)
	}
	for _, k := range []string{"Referer", "Cookie", "X-Custom"} {
		if h.Get(k) != "" {
			t.Errorf("%s が残っている", k)
		}
	}
	for _, k := range []string{"Accept", "Content-Type", "Sec-Websocket-Key"} {
		if h.Get(k) == "" {
			t.Errorf("%s が落ちている", k)
		}
	}
	if len(dropped) != 3 {
		t.Fatalf("dropped = %v", dropped)
	}
}

func TestApplyAddsUserAgentWhenMissing(t *testing.T) {
	h := http.Header{}
	(&Policy{Enabled: true, UserAgent: "fixed/1"}).Rules("a.example").Apply(h)
	if h.Get("User-Agent") != "fixed/1" {
		t.Fatalf("%v", h)
	}
}

func TestGlobalAndHostRelaxation(t *testing.T) {
	p := &Policy{
		Enabled: true,
		Allow:   []string{"Accept-Language"},
		Hosts: map[string]HostRule{
			"*.pkg.example": {Allow: []string{"X-Pkg-*"}, UserAgent: "pkg-client"},
			"keep.example":  {KeepUserAgent: true},
		},
	}
	mk := func() http.Header {
		h := http.Header{}
		h.Set("Accept-Language", "ja")
		h.Set("X-Pkg-Token", "t")
		h.Set("X-Other", "o")
		h.Set("User-Agent", "agent/9")
		return h
	}

	h := mk()
	p.Rules("a.pkg.example").Apply(h)
	if h.Get("Accept-Language") != "ja" || h.Get("X-Pkg-Token") != "t" || h.Get("X-Other") != "" || h.Get("User-Agent") != "pkg-client" {
		t.Fatalf("pkg: %v", h)
	}

	h = mk()
	p.Rules("other.example").Apply(h)
	if h.Get("Accept-Language") != "ja" || h.Get("X-Pkg-Token") != "" || h.Get("User-Agent") != DefaultUserAgent {
		t.Fatalf("other: %v", h)
	}

	h = mk()
	p.Rules("keep.example").Apply(h)
	if h.Get("User-Agent") != "agent/9" {
		t.Fatalf("keep: %v", h)
	}

	// 緩和は他の行き先へ漏れない。
	if reflect.DeepEqual(p.Rules("a.pkg.example"), p.Rules("other.example")) {
		t.Fatal("行き先ごとの規則が同じ")
	}
}

func TestKeepUserAgentSuppressesDefaultWhenAbsent(t *testing.T) {
	h := http.Header{}
	(&Policy{Hosts: map[string]HostRule{"k.example": {KeepUserAgent: true}}}).Rules("k.example").Apply(h)
	if v, ok := h["User-Agent"]; !ok || len(v) != 1 || v[0] != "" {
		t.Fatalf("%v", h)
	}
}
