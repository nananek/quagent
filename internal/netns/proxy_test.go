package netns

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"
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

func TestReadHost(t *testing.T) {
	// 1. 正常な HTTP リクエスト
	r1 := bufio.NewReader(strings.NewReader("GET / HTTP/1.1\r\nHost: example.org\r\n\r\n"))
	host, err := readHost(r1)
	if err != nil || host != "example.org" {
		t.Fatalf("readHost: got %q, %v; want example.org", host, err)
	}

	// 2. CONNECT リクエスト
	r2 := bufio.NewReader(strings.NewReader("CONNECT api.example.com:443 HTTP/1.1\r\n\r\n"))
	host, err = readHost(r2)
	if err != nil || host != "api.example.com" {
		t.Fatalf("readHost CONNECT: got %q, %v; want api.example.com", host, err)
	}

	// 3. 途中で切れた (EOF)
	r3 := bufio.NewReader(strings.NewReader("GET / HTTP/1.1\r\nHost: truncated"))
	if _, err := readHost(r3); err == nil {
		t.Fatal("expected error on truncated input")
	}

	// 4. ヘッダが大きすぎる (> proxyMaxHead)
	bigHeader := "GET / HTTP/1.1\r\nX-Big: " + strings.Repeat("A", proxyMaxHead+10) + "\r\n\r\n"
	r4 := bufio.NewReader(strings.NewReader(bigHeader))
	if _, err := readHost(r4); err == nil {
		t.Fatal("expected error on oversized header")
	}
}

func TestCaptureReader(t *testing.T) {
	src := strings.NewReader("hello world")
	cr := &captureReader{r: src}
	buf := make([]byte, 5)
	n, err := cr.Read(buf)
	if err != nil || n != 5 {
		t.Fatalf("first read: %d, %v", n, err)
	}
	if string(cr.buf) != "hello" {
		t.Errorf("cr.buf = %q, want 'hello'", string(cr.buf))
	}

	buf2 := make([]byte, 10)
	n, err = cr.Read(buf2)
	if err != nil || n != 6 {
		t.Fatalf("second read: %d, %v", n, err)
	}
	if string(cr.buf) != "hello world" {
		t.Errorf("cr.buf = %q, want 'hello world'", string(cr.buf))
	}
}

func TestReplayConn(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	prefix := strings.NewReader("prefix-")
	rc := &replayConn{
		Conn: c1,
		r:    io.MultiReader(prefix, c1),
	}

	go func() {
		_, _ = c2.Write([]byte("rest"))
	}()

	buf := make([]byte, 11)
	n, err := io.ReadFull(rc, buf)
	if err != nil || n != 11 {
		t.Fatalf("io.ReadFull failed: %d, %v", n, err)
	}
	if string(buf) != "prefix-rest" {
		t.Errorf("read = %q, want 'prefix-rest'", string(buf))
	}
}

func TestWebProxyPipe(t *testing.T) {
	clientSide, clientServerSide := net.Pipe()
	defer clientSide.Close()
	defer clientServerSide.Close()

	upstreamClientSide, upstreamServerSide := net.Pipe()
	defer upstreamClientSide.Close()
	defer upstreamServerSide.Close()

	proxy := &webProxy{
		dial: func(network, addr string, mark int) (net.Conn, error) {
			if addr != "example.com:80" {
				t.Errorf("dial addr = %q, want 'example.com:80'", addr)
			}
			return upstreamClientSide, nil
		},
	}

	// pipe をバックグラウンドで動かす
	pipeDone := make(chan struct{})
	go func() {
		proxy.pipe(clientServerSide, "example.com:80", "example.com", clientServerSide)
		close(pipeDone)
	}()

	// 1. クライアント -> 上流
	go func() {
		_, _ = clientSide.Write([]byte("ping"))
	}()
	upBuf := make([]byte, 4)
	if _, err := io.ReadFull(upstreamServerSide, upBuf); err != nil {
		t.Fatalf("upstream read error: %v", err)
	}
	if string(upBuf) != "ping" {
		t.Errorf("upstream got %q, want 'ping'", string(upBuf))
	}

	// 2. 上流 -> クライアント
	go func() {
		_, _ = upstreamServerSide.Write([]byte("pong"))
	}()
	clientBuf := make([]byte, 4)
	if _, err := io.ReadFull(clientSide, clientBuf); err != nil {
		t.Fatalf("client read error: %v", err)
	}
	if string(clientBuf) != "pong" {
		t.Errorf("client got %q, want 'pong'", string(clientBuf))
	}

	// クローズして pipe 終了を待つ
	clientSide.Close()
	upstreamServerSide.Close()
	select {
	case <-pipeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("pipe did not terminate")
	}
}

func TestWebProxyBlock(t *testing.T) {
	var blockedReason string
	p := &webProxy{
		blocked: func(reason string) {
			blockedReason = reason
		},
	}
	p.block("test block reason")
	if blockedReason != "test block reason" {
		t.Errorf("blockedReason = %q, want 'test block reason'", blockedReason)
	}
}

func TestHandleTLS(t *testing.T) {
	// 1. 壊れたデータ -> block
	var blockedReason string
	p := &webProxy{
		blocked: func(reason string) { blockedReason = reason },
		allowed: func(name string) bool { return true },
	}
	c1, c2 := net.Pipe()
	go func() {
		_, _ = c2.Write([]byte("not a tls record"))
		_ = c2.Close()
	}()
	p.handleTLS(c1, "203.0.113.1:443")
	_ = c1.Close()
	if !strings.Contains(blockedReason, "SNI 203.0.113.1") {
		t.Errorf("expected blocked reason for bad TLS, got %q", blockedReason)
	}

	// 2. 不許可な SNI
	blockedReason = ""
	p = &webProxy{
		blocked: func(reason string) { blockedReason = reason },
		allowed: func(name string) bool { return name == "good.com" },
	}
	c1, c2 = net.Pipe()
	go func() {
		_, _ = c2.Write(tlsRecord(clientHello("evil.com")))
		_ = c2.Close()
	}()
	p.handleTLS(c1, "203.0.113.1:443")
	_ = c1.Close()
	if blockedReason != "SNI evil.com" {
		t.Errorf("expected blocked SNI evil.com, got %q", blockedReason)
	}

	// 3. 許可された SNI (passthrough で pipe 呼び出し)
	dialed := false
	p = &webProxy{
		allowed:     func(name string) bool { return name == "good.com" },
		passthrough: []string{"good.com"},
		dial: func(network, addr string, mark int) (net.Conn, error) {
			dialed = true
			up1, up2 := net.Pipe()
			go func() { _ = up2.Close() }()
			return up1, nil
		},
	}
	c1, c2 = net.Pipe()
	go func() {
		_, _ = c2.Write(tlsRecord(clientHello("good.com")))
		_ = c2.Close()
	}()
	p.handleTLS(c1, "203.0.113.1:443")
	_ = c1.Close()
	if !dialed {
		t.Error("expected dial to be called for allowed passthrough SNI")
	}
}

func TestHandleHTTP(t *testing.T) {
	// 1. 壊れた HTTP -> block
	var blockedReason string
	p := &webProxy{
		blocked: func(reason string) { blockedReason = reason },
		allowed: func(name string) bool { return true },
	}
	c1, c2 := net.Pipe()
	go func() {
		_, _ = c2.Write([]byte("GARBAGE\r\n\r\n"))
		_ = c2.Close()
	}()
	p.handleHTTP(c1, "203.0.113.1:80")
	_ = c1.Close()
	if !strings.Contains(blockedReason, "Host 203.0.113.1") {
		t.Errorf("expected blocked reason for bad HTTP, got %q", blockedReason)
	}

	// 2. 不許可な Host
	blockedReason = ""
	p = &webProxy{
		blocked: func(reason string) { blockedReason = reason },
		allowed: func(name string) bool { return name == "good.com" },
	}
	c1, c2 = net.Pipe()
	go func() {
		_, _ = c2.Write([]byte("GET / HTTP/1.1\r\nHost: evil.com\r\n\r\n"))
		_ = c2.Close()
	}()
	p.handleHTTP(c1, "203.0.113.1:80")
	_ = c1.Close()
	if blockedReason != "Host evil.com" {
		t.Errorf("expected blocked Host evil.com, got %q", blockedReason)
	}

	// 3. 許可された Host (headers == nil で pipe 呼び出し)
	dialed := false
	p = &webProxy{
		allowed: func(name string) bool { return name == "good.com" },
		dial: func(network, addr string, mark int) (net.Conn, error) {
			dialed = true
			up1, up2 := net.Pipe()
			go func() { _ = up2.Close() }()
			return up1, nil
		},
	}
	c1, c2 = net.Pipe()
	go func() {
		_, _ = c2.Write([]byte("GET / HTTP/1.1\r\nHost: good.com\r\n\r\n"))
		_ = c2.Close()
	}()
	p.handleHTTP(c1, "203.0.113.1:80")
	_ = c1.Close()
	if !dialed {
		t.Error("expected dial to be called for allowed Host")
	}
}
