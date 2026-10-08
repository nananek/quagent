package headerpolicy

import (
	"bufio"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestApplyDropsUnknownAndFixesUserAgent(t *testing.T) {
	p := &Policy{Enabled: true}
	h := http.Header{}
	h.Set("User-Agent", "me@example.com")
	h.Set("Accept", "*/*")
	h.Set("Content-Type", "application/json")
	h.Set("Range", "bytes=0-10")
	h.Set("If-Modified-Since", "Wed, 21 Oct 2015 07:28:00 GMT")
	h.Set("Referer", "https://x.example/")
	h.Set("Cookie", "a=b")
	h.Set("X-Custom", "1")
	h.Set("Authorization", "Bearer secret")
	h.Set("Sec-WebSocket-Key", "k")
	h.Set("If-Match", `"xyz"`)
	h.Set("Grpc-Metadata-Token", "t")
	dropped := p.Rules("a.example").Apply(h)

	if got := h.Get("User-Agent"); got != DefaultUserAgent {
		t.Fatalf("User-Agent = %q", got)
	}
	for _, k := range []string{"Referer", "Cookie", "X-Custom", "Authorization", "Sec-WebSocket-Key", "If-Match", "Grpc-Metadata-Token"} {
		if h.Get(k) != "" {
			t.Errorf("%s が残っている", k)
		}
	}
	for _, k := range []string{"Accept", "Content-Type", "Range", "If-Modified-Since"} {
		if h.Get(k) == "" {
			t.Errorf("%s が落ちている", k)
		}
	}
	if len(dropped) != 7 {
		t.Fatalf("dropped = %v (len=%d, want 7)", dropped, len(dropped))
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

func TestCheckRequestDenyBody(t *testing.T) {
	p := &Policy{
		Enabled:         true,
		DenyRequestBody: true,
	}
	rules := p.Rules("api.example")

	// GET without body -> ok
	req, _ := http.NewRequest("GET", "https://api.example/", nil)
	if code, reason := rules.CheckRequest(req); code != 0 {
		t.Fatalf("GET without body was blocked: %d %s", code, reason)
	}

	// GET with empty body -> ok
	req, _ = http.NewRequest("GET", "https://api.example/", strings.NewReader(""))
	if code, reason := rules.CheckRequest(req); code != 0 {
		t.Fatalf("GET with empty body was blocked: %d %s", code, reason)
	}

	// GET with body -> 400
	req, _ = http.NewRequest("GET", "https://api.example/", strings.NewReader("payload"))
	if code, reason := rules.CheckRequest(req); code != http.StatusBadRequest {
		t.Fatalf("GET with body code = %d, want 400 (%s)", code, reason)
	}

	// POST with body -> 400
	req, _ = http.NewRequest("POST", "https://api.example/", strings.NewReader("data"))
	if code, reason := rules.CheckRequest(req); code != http.StatusBadRequest {
		t.Fatalf("POST with body code = %d, want 400 (%s)", code, reason)
	}

	// POST without body (Content-Length: 0) -> ok (since allowed_methods not restricted)
	req, _ = http.NewRequest("POST", "https://api.example/", nil)
	if code, reason := rules.CheckRequest(req); code != 0 {
		t.Fatalf("POST without body was blocked: %d %s", code, reason)
	}

	// Chunked body -> 400
	req, _ = http.NewRequest("POST", "https://api.example/", nil)
	req.TransferEncoding = []string{"chunked"}
	req.ContentLength = -1
	if code, reason := rules.CheckRequest(req); code != http.StatusBadRequest {
		t.Fatalf("chunked body code = %d, want 400 (%s)", code, reason)
	}
}

func TestCheckRequestAllowedMethods(t *testing.T) {
	p := &Policy{
		Enabled:        true,
		AllowedMethods: []string{"GET", "HEAD"},
	}
	rules := p.Rules("api.example")

	req, _ := http.NewRequest("GET", "https://api.example/", nil)
	if code, _ := rules.CheckRequest(req); code != 0 {
		t.Fatalf("GET was blocked: %d", code)
	}

	req, _ = http.NewRequest("HEAD", "https://api.example/", nil)
	if code, _ := rules.CheckRequest(req); code != 0 {
		t.Fatalf("HEAD was blocked: %d", code)
	}

	req, _ = http.NewRequest("POST", "https://api.example/", nil)
	if code, reason := rules.CheckRequest(req); code != http.StatusMethodNotAllowed {
		t.Fatalf("POST code = %d, want 405 (%s)", code, reason)
	}

	req, _ = http.NewRequest("DELETE", "https://api.example/", nil)
	if code, reason := rules.CheckRequest(req); code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE code = %d, want 405 (%s)", code, reason)
	}
}

func TestCheckRequestHostRelaxation(t *testing.T) {
	p := &Policy{
		Enabled:         true,
		DenyRequestBody: true,
		AllowedMethods:  []string{"GET", "HEAD"},
		Hosts: map[string]HostRule{
			"upload.example": {
				AllowRequestBody: true,
				AllowedMethods:   []string{"POST"},
			},
		},
	}

	// General host: POST blocked by method
	otherRules := p.Rules("other.example")
	req, _ := http.NewRequest("POST", "https://other.example/", strings.NewReader("data"))
	if code, reason := otherRules.CheckRequest(req); code != http.StatusMethodNotAllowed {
		t.Fatalf("other POST code = %d, want 405 (%s)", code, reason)
	}

	// General host: GET with body blocked by deny_body
	req, _ = http.NewRequest("GET", "https://other.example/", strings.NewReader("data"))
	if code, reason := otherRules.CheckRequest(req); code != http.StatusBadRequest {
		t.Fatalf("other GET with body code = %d, want 400 (%s)", code, reason)
	}

	// Relaxed host: POST with body allowed!
	uploadRules := p.Rules("upload.example")
	req, _ = http.NewRequest("POST", "https://upload.example/", strings.NewReader("data"))
	if code, reason := uploadRules.CheckRequest(req); code != 0 {
		t.Fatalf("upload POST with body code = %d, want 0 (%s)", code, reason)
	}
}

func TestReadRequestParsing(t *testing.T) {
	p := &Policy{
		Enabled:         true,
		DenyRequestBody: true,
	}
	rules := p.Rules("example.com")

	tests := []struct {
		name       string
		raw        string
		wantBlock  bool
		wantStatus int
	}{
		{
			name:      "GET without headers",
			raw:       "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n",
			wantBlock: false,
		},
		{
			name:      "HEAD without headers",
			raw:       "HEAD / HTTP/1.1\r\nHost: example.com\r\n\r\n",
			wantBlock: false,
		},
		{
			name:      "POST Content-Length 0",
			raw:       "POST / HTTP/1.1\r\nHost: example.com\r\nContent-Length: 0\r\n\r\n",
			wantBlock: false,
		},
		{
			name:      "POST without Content-Length (empty body)",
			raw:       "POST / HTTP/1.1\r\nHost: example.com\r\n\r\n",
			wantBlock: false,
		},
		{
			name:       "POST Content-Length 5",
			raw:        "POST / HTTP/1.1\r\nHost: example.com\r\nContent-Length: 5\r\n\r\nhello",
			wantBlock:  true,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "GET with Content-Length 5",
			raw:        "GET / HTTP/1.1\r\nHost: example.com\r\nContent-Length: 5\r\n\r\nhello",
			wantBlock:  true,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "POST chunked non-empty",
			raw:        "POST / HTTP/1.1\r\nHost: example.com\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n\r\n",
			wantBlock:  true,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "POST chunked empty (0 chunk only)",
			raw:        "POST / HTTP/1.1\r\nHost: example.com\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n",
			wantBlock:  true,
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(tc.raw)))
			if err != nil {
				t.Fatalf("ReadRequest failed: %v", err)
			}
			code, reason := rules.CheckRequest(req)
			if tc.wantBlock {
				if code != tc.wantStatus {
					t.Fatalf("got code %d (%s), want %d", code, reason, tc.wantStatus)
				}
			} else {
				if code != 0 {
					t.Fatalf("unexpected block: %d (%s)", code, reason)
				}
			}
		})
	}
}
