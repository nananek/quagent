package netns

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// clientHello は SNI 付き (serverName が空なら SNI 無し) の最小の ClientHello を返す。
func clientHello(serverName string) []byte {
	var ext []byte
	if serverName != "" {
		name := []byte(serverName)
		sn := []byte{0} // name_type: host_name
		sn = binary.BigEndian.AppendUint16(sn, uint16(len(name)))
		sn = append(sn, name...)
		list := binary.BigEndian.AppendUint16(nil, uint16(len(sn)))
		list = append(list, sn...)
		ext = append(ext, 0x00, 0x00) // extension_type: server_name
		ext = binary.BigEndian.AppendUint16(ext, uint16(len(list)))
		ext = append(ext, list...)
	}
	body := []byte{0x03, 0x03}                  // client_version
	body = append(body, make([]byte, 32)...)    // random
	body = append(body, 0x00)                   // session_id 長
	body = append(body, 0x00, 0x02, 0x13, 0x01) // cipher_suites
	body = append(body, 0x01, 0x00)             // compression_methods
	body = binary.BigEndian.AppendUint16(body, uint16(len(ext)))
	body = append(body, ext...)
	hs := []byte{0x01, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}
	return append(hs, body...)
}

func tlsRecord(hs []byte) []byte {
	r := []byte{0x16, 0x03, 0x03}
	r = binary.BigEndian.AppendUint16(r, uint16(len(hs)))
	return append(r, hs...)
}

func TestReadSNI(t *testing.T) {
	got, err := readSNI(bytes.NewReader(tlsRecord(clientHello("Www.Example.COM"))))
	if err != nil {
		t.Fatalf("readSNI: %v", err)
	}
	if got != "www.example.com" {
		t.Fatalf("SNI = %q, want www.example.com", got)
	}
}

// ClientHello が複数の TLS レコードに分かれていても SNI を読めること。
func TestReadSNISplitRecords(t *testing.T) {
	hs := clientHello("api.example.com")
	for _, cut := range []int{1, 5, 40, len(hs) - 1} {
		rec := append(tlsRecord(hs[:cut]), tlsRecord(hs[cut:])...)
		got, err := readSNI(bytes.NewReader(rec))
		if err != nil || got != "api.example.com" {
			t.Fatalf("cut=%d: SNI = %q, err = %v", cut, got, err)
		}
	}
}

func TestReadSNIRejects(t *testing.T) {
	// SNI 拡張が無い ClientHello
	if _, err := readSNI(bytes.NewReader(tlsRecord(clientHello("")))); err == nil {
		t.Fatal("SNI が無い ClientHello を通した")
	}
	// TLS のハンドシェイクではない (平文 HTTP など)
	if _, err := readSNI(bytes.NewReader([]byte("GET / HTTP/1.1\r\n"))); err == nil {
		t.Fatal("ハンドシェイクでないものを通した")
	}
	// 途中で切れた ClientHello
	if _, err := readSNI(bytes.NewReader([]byte{0x16, 0x03, 0x03, 0x00, 0x10, 0x01, 0x00, 0x00})); err == nil {
		t.Fatal("切れた ClientHello を通した")
	}
}

func TestParseClientHelloTruncated(t *testing.T) {
	for _, b := range [][]byte{
		{},
		{0x01},
		{0x01, 0x00, 0x00, 0x64, 0x03, 0x03},
		{0x02, 0x00, 0x00, 0x00},
	} {
		if _, ok := parseClientHello(b); ok {
			t.Errorf("parseClientHello(%v) が ok を返した", b)
		}
	}
}

func TestHostFromRequest(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"GET / HTTP/1.1\r\nHost: Example.COM:8080\r\n\r\n", "example.com", false},
		{"GET / HTTP/1.1\r\nhost: a.b.example\r\n\r\n", "a.b.example", false},
		{"GET / HTTP/1.1\r\nX: y\r\nHost: z.example\r\n\r\n", "z.example", false},
		{"CONNECT example.com:443 HTTP/1.1\r\n\r\n", "example.com", false},
		{"GET / HTTP/1.0\r\n\r\n", "", true},
		{"GET /\r\n\r\n", "", true},
		{"", "", true},
	}
	for _, c := range cases {
		got, err := hostFromRequest([]byte(c.in))
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("hostFromRequest(%q) = %q, %v; want %q, err=%v", c.in, got, err, c.want, c.wantErr)
		}
	}
}

func TestNormalizeHost(t *testing.T) {
	cases := []struct{ in, want string }{
		{"example.com", "example.com"},
		{"Example.com:8080", "example.com"},
		{"[2001:db8::1]:80", "2001:db8::1"},
		{" Host.Example. ", "host.example"},
	}
	for _, c := range cases {
		got, err := normalizeHost(c.in)
		if err != nil || got != c.want {
			t.Errorf("normalizeHost(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"", ":80", "   "} {
		if _, err := normalizeHost(bad); err == nil {
			t.Errorf("normalizeHost(%q) がエラーを返さない", bad)
		}
	}
}

// 許可外の SNI は allowed で弾けること (判定そのものは egress 側)。
func TestProxyNameDecision(t *testing.T) {
	e, _ := newTestEgress([]Grant{{Pattern: "*.example.com"}})
	for name, want := range map[string]bool{
		"api.example.com": true,
		"example.com":     false,
		"evil.test":       false,
		"a.b.example.com": true,
	} {
		if got := e.allowed(name); got != want {
			t.Errorf("allowed(%q) = %v, want %v", name, got, want)
		}
	}
}

// passthrough に挙げた行き先は TLS 終端しない (証明書を固定するクライアント向け)。
// "*.example.com" はサブドメインのみで、apex は一致しない。
func TestProxyTerminates(t *testing.T) {
	p := &webProxy{passthrough: []string{"pinned.example", "*.cdn.example"}}
	for name, want := range map[string]bool{
		"pinned.example":   false,
		"other.example":    true,
		"a.cdn.example":    false,
		"cdn.example":      true,
		"x.pinned.example": true,
	} {
		if got := p.terminates(name); got != want {
			t.Errorf("terminates(%q) = %v, want %v", name, got, want)
		}
	}
	// passthrough が無ければ常に終端する。
	if !(&webProxy{}).terminates("anything.example") {
		t.Error("passthrough 無しで終端しないと判定した")
	}
}
