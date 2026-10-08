package netns

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nananek/quagent/internal/headerpolicy"
	"github.com/nananek/quagent/internal/tlsmitm"
	"golang.org/x/net/http2"
)

// testUpstream は TLS の上流サーバーを 127.0.0.1 に立てる。証明書は渡した CA が
// servername 用に署名したものを使う。返すのは接続先と、検証に使うルート。
func testUpstream(t *testing.T, ca *tlsmitm.CA, servername string, handler http.Handler) (string, *x509.CertPool) {
	t.Helper()
	cert, err := ca.Leaf(servername)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}}))
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String(), poolFor(t, ca)
}

// testUpstreamH2 は h2 を話す TLS の上流サーバーを 127.0.0.1 に立てる。
func testUpstreamH2(t *testing.T, ca *tlsmitm.CA, servername string, handler http.Handler) (string, *x509.CertPool) {
	t.Helper()
	cert, err := ca.Leaf(servername)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// ServeTLS は既定で h2 を有効にする (NextProtos に h2 を足す)。
	srv := &http.Server{Handler: handler, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}}}
	go srv.ServeTLS(ln, "", "")
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String(), poolFor(t, ca)
}

// testProxy は terminate を直に叩く透明プロキシを立てる (originalDst は使わない)。
// 上流への接続は holderPid を使わず、渡したテストサーバーへ張る。
func testProxy(t *testing.T, proxyCA *tlsmitm.CA, upstreamAddr string, roots *x509.CertPool, headers *headerpolicy.Policy) string {
	t.Helper()
	p := &webProxy{
		allowed:       func(name string) bool { return name == "allowed.example" },
		mitm:          proxyCA,
		headers:       headers,
		upstreamRoots: roots,
		sem:           make(chan struct{}, 8),
		dial: func(network, addr string, mark int) (net.Conn, error) {
			return net.Dial(network, upstreamAddr)
		},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.terminate(c, "203.0.113.9:443", "allowed.example")
		}
	}()
	return ln.Addr().String()
}

// dialProxy は proxyAddr へ繋ぎ、proxy CA を信頼する HTTP クライアントを作る。
// SNI は allowed.example に固定する。
func dialProxy(t *testing.T, proxyAddr string, proxyCA *tlsmitm.CA) *http.Transport {
	t.Helper()
	return &http.Transport{
		ForceAttemptHTTP2: false,
		TLSClientConfig:   &tls.Config{RootCAs: poolFor(t, proxyCA), ServerName: "allowed.example"},
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("tcp", proxyAddr)
		},
	}
}

func poolFor(t *testing.T, ca *tlsmitm.CA) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("CA を読めない")
	}
	return pool
}

// testProxyFn は handler を接続ごとに呼ぶ生のプロキシを立てる。平文 HTTP の経路を
// 確かめるのに使う。
func testProxyFn(t *testing.T, handler func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				handler(c)
			}()
		}
	}()
	return ln.Addr().String()
}

// dialProxyH2 は proxyAddr 経由で h2 を話す HTTP クライアントの Transport を作る。
// SNI は allowed.example に固定する。
func dialProxyH2(t *testing.T, proxyAddr string, proxyCA *tlsmitm.CA) *http2.Transport {
	t.Helper()
	roots := poolFor(t, proxyCA)
	return &http2.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
			c, err := net.Dial("tcp", proxyAddr)
			if err != nil {
				return nil, err
			}
			tc := tls.Client(c, &tls.Config{RootCAs: roots, ServerName: "allowed.example", NextProtos: []string{"h2"}})
			if err := tc.HandshakeContext(ctx); err != nil {
				c.Close()
				return nil, err
			}
			return tc, nil
		},
	}
}

func newPolicy() *headerpolicy.Policy {
	return &headerpolicy.Policy{Enabled: true, UserAgent: "fixed/1", Allow: []string{"X-Allowed"}}
}

// seenHeaders は上流に届いたヘッダを覚える。
type seenHeaders struct {
	mu sync.Mutex
	h  http.Header
}

func (s *seenHeaders) handler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.h = r.Header.Clone()
		s.mu.Unlock()
		io.WriteString(w, body)
	})
}

func (s *seenHeaders) get() http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.h
}

func setLeakyHeaders(h http.Header) {
	h.Set("User-Agent", "me@example.com")
	h.Set("Content-Type", "application/json")
	h.Set("Referer", "https://elsewhere.example/")
	h.Set("Cookie", "a=b")
	h.Set("X-Leak", "secret")
	h.Set("X-Allowed", "yes")
}

func checkFiltered(t *testing.T, seen http.Header) {
	t.Helper()
	if got := seen.Get("User-Agent"); got != "fixed/1" {
		t.Fatalf("User-Agent = %q", got)
	}
	for _, k := range []string{"Referer", "Cookie", "X-Leak"} {
		if seen.Get(k) != "" {
			t.Errorf("%s が上流へ届いている: %v", k, seen)
		}
	}
	if seen.Get("X-Allowed") != "yes" || seen.Get("Content-Type") != "application/json" {
		t.Fatalf("許可したヘッダが落ちている: %v", seen)
	}
}

func TestMITMFiltersHeaders(t *testing.T) {
	upCA, _ := tlsmitm.NewCA()
	var seen seenHeaders
	addr, roots := testUpstream(t, upCA, "allowed.example", seen.handler("ok"))
	proxyCA, _ := tlsmitm.NewCA()
	proxyAddr := testProxy(t, proxyCA, addr, roots, newPolicy())

	req, _ := http.NewRequest("POST", "https://allowed.example/", strings.NewReader("{}"))
	setLeakyHeaders(req.Header)
	client := &http.Client{Timeout: 10 * time.Second, Transport: dialProxy(t, proxyAddr, proxyCA)}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	checkFiltered(t, seen.get())
}

func TestMITMHTTP2FiltersHeaders(t *testing.T) {
	upCA, _ := tlsmitm.NewCA()
	var seen seenHeaders
	addr, roots := testUpstreamH2(t, upCA, "allowed.example", seen.handler("ok"))
	proxyCA, _ := tlsmitm.NewCA()
	proxyAddr := testProxy(t, proxyCA, addr, roots, newPolicy())

	req, _ := http.NewRequest("POST", "https://allowed.example/path?q=1", strings.NewReader("{}"))
	setLeakyHeaders(req.Header)
	client := &http.Client{Timeout: 10 * time.Second, Transport: dialProxyH2(t, proxyAddr, proxyCA)}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("クライアント側のプロトコル = %s, want HTTP/2", resp.Proto)
	}
	checkFiltered(t, seen.get())
}

func TestMITMStripsHopByHopHeaders(t *testing.T) {
	upCA, _ := tlsmitm.NewCA()
	var seen seenHeaders
	addr, roots := testUpstream(t, upCA, "allowed.example", seen.handler("ok"))
	proxyCA, _ := tlsmitm.NewCA()
	policy := newPolicy()
	policy.Allow = append(policy.Allow, "X-Drop-Me", "X-Keep")
	proxyAddr := testProxy(t, proxyCA, addr, roots, policy)

	req, _ := http.NewRequest("GET", "https://allowed.example/", nil)
	req.Header.Set("Proxy-Connection", "keep-alive")
	req.Header.Set("X-Drop-Me", "yes")
	req.Header.Set("Connection", "X-Drop-Me")
	req.Header.Set("X-Keep", "yes")
	client := &http.Client{Timeout: 10 * time.Second, Transport: dialProxy(t, proxyAddr, proxyCA)}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got := seen.get()
	if got.Get("X-Keep") != "yes" {
		t.Fatalf("転送するヘッダが落ちている: %v", got)
	}
	if got.Get("X-Drop-Me") != "" || got.Get("Proxy-Connection") != "" {
		t.Fatalf("接続ごとのヘッダが転送されている: %v", got)
	}
}

func TestMITMHostRelaxation(t *testing.T) {
	upCA, _ := tlsmitm.NewCA()
	var seen seenHeaders
	addr, roots := testUpstream(t, upCA, "allowed.example", seen.handler("ok"))
	proxyCA, _ := tlsmitm.NewCA()
	policy := newPolicy()
	policy.Hosts = map[string]headerpolicy.HostRule{"allowed.example": {Allow: []string{"X-Leak"}, UserAgent: "relaxed/2"}}
	proxyAddr := testProxy(t, proxyCA, addr, roots, policy)

	req, _ := http.NewRequest("GET", "https://allowed.example/", nil)
	setLeakyHeaders(req.Header)
	client := &http.Client{Timeout: 10 * time.Second, Transport: dialProxy(t, proxyAddr, proxyCA)}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got := seen.get()
	if got.Get("X-Leak") != "secret" || got.Get("User-Agent") != "relaxed/2" || got.Get("Cookie") != "" {
		t.Fatalf("緩和の結果 = %v", got)
	}
}

func TestMITMRejectsUntrustedUpstream(t *testing.T) {
	// 上流の証明書を検証できないとき (roots が system のまま) は通さない。
	upCA, _ := tlsmitm.NewCA()
	addr, _ := testUpstream(t, upCA, "allowed.example", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "should not reach")
	}))
	proxyCA, _ := tlsmitm.NewCA()
	proxyAddr := testProxy(t, proxyCA, addr, nil, newPolicy())

	client := &http.Client{Timeout: 10 * time.Second, Transport: dialProxy(t, proxyAddr, proxyCA)}
	if _, err := client.Get("https://allowed.example/"); err == nil {
		t.Fatal("検証できない上流への転送が通ってしまった")
	}
}

func TestMITMKeepAlive(t *testing.T) {
	upCA, _ := tlsmitm.NewCA()
	var seen seenHeaders
	addr, roots := testUpstream(t, upCA, "allowed.example", seen.handler("ok"))
	proxyCA, _ := tlsmitm.NewCA()
	proxyAddr := testProxy(t, proxyCA, addr, roots, newPolicy())

	client := &http.Client{Timeout: 10 * time.Second, Transport: dialProxy(t, proxyAddr, proxyCA)}
	for i := 0; i < 3; i++ {
		resp, err := client.Get("https://allowed.example/")
		if err != nil {
			t.Fatalf("%d 回目: %v", i, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != "ok" {
			t.Fatalf("%d 回目の本文 = %q", i, body)
		}
		if seen.get().Get("User-Agent") != "fixed/1" {
			t.Fatalf("%d 回目の User-Agent = %v", i, seen.get())
		}
	}
}

func TestMITMUpgradeTunnel(t *testing.T) {
	upCA, _ := tlsmitm.NewCA()
	var seen seenHeaders
	addr, roots := testUpstream(t, upCA, "allowed.example", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "" {
			http.Error(w, "no upgrade", http.StatusBadRequest)
			return
		}
		seen.mu.Lock()
		seen.h = r.Header.Clone()
		seen.mu.Unlock()
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		brw.Flush()
		// 受け取ったバイトをそのまま返す
		io.Copy(conn, conn)
	}))
	proxyCA, _ := tlsmitm.NewCA()
	wsPolicy := &headerpolicy.Policy{
		Enabled:   true,
		UserAgent: "fixed/1",
		Allow:     []string{"X-Allowed", "Connection", "Upgrade", "Sec-WebSocket-Key"},
	}
	proxyAddr := testProxy(t, proxyCA, addr, roots, wsPolicy)

	raw, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	tc := tls.Client(raw, &tls.Config{RootCAs: poolFor(t, proxyCA), ServerName: "allowed.example"})
	if err := tc.Handshake(); err != nil {
		t.Fatal(err)
	}
	_ = tc.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(tc, "GET /ws HTTP/1.1\r\nHost: allowed.example\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: k\r\nX-Leak: secret\r\n\r\n")
	br := bufio.NewReader(tc)
	resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 101 {
		t.Fatalf("状態 = %d, want 101", resp.StatusCode)
	}
	got := seen.get()
	if got.Get("Sec-WebSocket-Key") != "k" || got.Get("X-Leak") != "" || got.Get("User-Agent") != "fixed/1" {
		t.Fatalf("Upgrade の要求のヘッダ = %v", got)
	}
	if _, err := io.WriteString(tc, "ping"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(br, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ping" {
		t.Fatalf("トンネルの往復 = %q", buf)
	}
}

func TestPlainHTTPFiltersHeaders(t *testing.T) {
	upLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var seen seenHeaders
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen.mu.Lock()
		seen.h = r.Header.Clone()
		seen.mu.Unlock()
		fmt.Fprintf(w, "%s %s body=%s", r.Method, r.URL.RequestURI(), body)
	})}
	go srv.Serve(upLn)
	t.Cleanup(func() { _ = srv.Close() })

	p := &webProxy{
		allowed: func(name string) bool { return name == "allowed.example" },
		headers: newPolicy(),
		sem:     make(chan struct{}, 8),
		dial:    func(network, addr string, mark int) (net.Conn, error) { return net.Dial(network, upLn.Addr().String()) },
	}
	proxyAddr := testProxyFn(t, func(c net.Conn) {
		p.serveHTTP1(c, bufio.NewReader(c), "203.0.113.9:80", "", false)
	})

	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprint(c, "POST /p?q=1 HTTP/1.1\r\nHost: allowed.example\r\nUser-Agent: me@example.com\r\nReferer: https://x.example/\r\nX-Allowed: yes\r\nContent-Type: application/json\r\nContent-Length: 3\r\n\r\nabc")
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: "POST"})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "POST /p?q=1 body=abc" {
		t.Fatalf("応答 = %q", body)
	}
	got := seen.get()
	if got.Get("User-Agent") != "fixed/1" || got.Get("Referer") != "" || got.Get("X-Allowed") != "yes" {
		t.Fatalf("平文のヘッダ = %v", got)
	}
}

func TestPlainHTTPDisallowedHost(t *testing.T) {
	var blocked []string
	p := &webProxy{
		allowed: func(name string) bool { return name == "allowed.example" },
		blocked: func(reason string) { blocked = append(blocked, reason) },
		headers: newPolicy(),
		sem:     make(chan struct{}, 8),
		dial: func(network, addr string, mark int) (net.Conn, error) {
			t.Error("許可外の Host なのに上流へ張ろうとした")
			return nil, fmt.Errorf("no")
		},
	}
	proxyAddr := testProxyFn(t, func(c net.Conn) {
		p.serveHTTP1(c, bufio.NewReader(c), "203.0.113.9:80", "", false)
	})
	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: evil.example\r\n\r\n")
	if _, err := io.ReadAll(c); err != nil {
		t.Fatal(err)
	}
	if len(blocked) == 0 {
		t.Fatal("拒否が通知されていない")
	}
}

func TestMITMDenyRequestBody(t *testing.T) {
	upCA, _ := tlsmitm.NewCA()
	var seen seenHeaders
	addr, roots := testUpstream(t, upCA, "allowed.example", seen.handler("ok"))
	proxyCA, _ := tlsmitm.NewCA()
	policy := newPolicy()
	policy.DenyRequestBody = true

	var mu sync.Mutex
	var blocked []string
	p := &webProxy{
		allowed:       func(name string) bool { return name == "allowed.example" },
		mitm:          proxyCA,
		headers:       policy,
		upstreamRoots: roots,
		sem:           make(chan struct{}, 8),
		blocked: func(reason string) {
			mu.Lock()
			blocked = append(blocked, reason)
			mu.Unlock()
		},
		dial: func(network, addr string, mark int) (net.Conn, error) {
			return net.Dial(network, addr)
		},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.terminate(c, addr, "allowed.example")
		}
	}()
	proxyAddr := ln.Addr().String()
	client := &http.Client{Timeout: 10 * time.Second, Transport: dialProxy(t, proxyAddr, proxyCA)}

	// 1. GET without body should succeed
	resp, err := client.Get("https://allowed.example/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", resp.StatusCode)
	}

	// 2. POST with body should be rejected with 400
	req, _ := http.NewRequest("POST", "https://allowed.example/", strings.NewReader("secret payload"))
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST status = %d, want 400", resp.StatusCode)
	}

	// 3. GET with body should be rejected with 400
	req, _ = http.NewRequest("GET", "https://allowed.example/", strings.NewReader("exfil"))
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET with body status = %d, want 400", resp.StatusCode)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(blocked) != 2 {
		t.Fatalf("blocked count = %d, want 2 (%v)", len(blocked), blocked)
	}
}

func TestMITMAllowedMethods(t *testing.T) {
	upCA, _ := tlsmitm.NewCA()
	var seen seenHeaders
	addr, roots := testUpstream(t, upCA, "allowed.example", seen.handler("ok"))
	proxyCA, _ := tlsmitm.NewCA()
	policy := newPolicy()
	policy.AllowedMethods = []string{"GET", "HEAD"}
	proxyAddr := testProxy(t, proxyCA, addr, roots, policy)
	client := &http.Client{Timeout: 10 * time.Second, Transport: dialProxy(t, proxyAddr, proxyCA)}

	// GET -> 200
	resp, err := client.Get("https://allowed.example/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", resp.StatusCode)
	}

	// POST without body -> 405
	req, _ := http.NewRequest("POST", "https://allowed.example/", nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", resp.StatusCode)
	}
}

func TestMITMHostRelaxationBodyAndMethod(t *testing.T) {
	upCA, _ := tlsmitm.NewCA()
	var seen seenHeaders
	addr, roots := testUpstream(t, upCA, "allowed.example", seen.handler("ok"))
	proxyCA, _ := tlsmitm.NewCA()
	policy := newPolicy()
	policy.DenyRequestBody = true
	policy.AllowedMethods = []string{"GET"}
	policy.Hosts = map[string]headerpolicy.HostRule{
		"allowed.example": {
			AllowRequestBody: true,
			AllowedMethods:   []string{"POST"},
		},
	}
	proxyAddr := testProxy(t, proxyCA, addr, roots, policy)
	client := &http.Client{Timeout: 10 * time.Second, Transport: dialProxy(t, proxyAddr, proxyCA)}

	// POST with body to relaxed host should succeed (200)
	req, _ := http.NewRequest("POST", "https://allowed.example/", strings.NewReader("payload"))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST status = %d, want 200", resp.StatusCode)
	}
}

func TestMITMHTTP2DenyRequestBody(t *testing.T) {
	upCA, _ := tlsmitm.NewCA()
	var seen seenHeaders
	addr, roots := testUpstreamH2(t, upCA, "allowed.example", seen.handler("ok"))
	proxyCA, _ := tlsmitm.NewCA()
	policy := newPolicy()
	policy.DenyRequestBody = true
	proxyAddr := testProxy(t, proxyCA, addr, roots, policy)
	client := &http.Client{Timeout: 10 * time.Second, Transport: dialProxyH2(t, proxyAddr, proxyCA)}

	// GET -> 200
	resp, err := client.Get("https://allowed.example/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", resp.StatusCode)
	}

	// POST with body -> 400
	req, _ := http.NewRequest("POST", "https://allowed.example/", strings.NewReader("secret"))
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST status = %d, want 400", resp.StatusCode)
	}
}

func TestPlainHTTPDenyRequestBody(t *testing.T) {
	upLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})}
	go srv.Serve(upLn)
	t.Cleanup(func() { _ = srv.Close() })

	policy := newPolicy()
	policy.DenyRequestBody = true
	p := &webProxy{
		allowed: func(name string) bool { return name == "allowed.example" },
		headers: policy,
		sem:     make(chan struct{}, 8),
		dial:    func(network, addr string, mark int) (net.Conn, error) { return net.Dial(network, upLn.Addr().String()) },
	}
	proxyAddr := testProxyFn(t, func(c net.Conn) {
		p.serveHTTP1(c, bufio.NewReader(c), "203.0.113.9:80", "", false)
	})

	// POST with body -> 400
	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprint(c, "POST / HTTP/1.1\r\nHost: allowed.example\r\nContent-Length: 4\r\n\r\ndata")
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: "POST"})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("平文 POST status = %d, want 400", resp.StatusCode)
	}
}

func TestMITMExpect100ContinueRejectedWithoutContinue(t *testing.T) {
	upCA, _ := tlsmitm.NewCA()
	var seen seenHeaders
	addr, roots := testUpstream(t, upCA, "allowed.example", seen.handler("ok"))
	proxyCA, _ := tlsmitm.NewCA()
	policy := newPolicy()
	policy.DenyRequestBody = true
	proxyAddr := testProxy(t, proxyCA, addr, roots, policy)

	raw, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	tc := tls.Client(raw, &tls.Config{RootCAs: poolFor(t, proxyCA), ServerName: "allowed.example"})
	if err := tc.Handshake(); err != nil {
		t.Fatal(err)
	}
	_ = tc.SetDeadline(time.Now().Add(5 * time.Second))

	// Send POST with Expect: 100-continue and body declared
	fmt.Fprint(tc, "POST / HTTP/1.1\r\nHost: allowed.example\r\nExpect: 100-continue\r\nContent-Length: 100\r\n\r\n")

	br := bufio.NewReader(tc)
	resp, err := http.ReadResponse(br, &http.Request{Method: "POST"})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (Expect: 100-continue with body should be rejected without 100 Continue)", resp.StatusCode)
	}
}

func TestMITMChunkedRejected(t *testing.T) {
	upCA, _ := tlsmitm.NewCA()
	var seen seenHeaders
	addr, roots := testUpstream(t, upCA, "allowed.example", seen.handler("ok"))
	proxyCA, _ := tlsmitm.NewCA()
	policy := newPolicy()
	policy.DenyRequestBody = true
	proxyAddr := testProxy(t, proxyCA, addr, roots, policy)

	raw, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	tc := tls.Client(raw, &tls.Config{RootCAs: poolFor(t, proxyCA), ServerName: "allowed.example"})
	if err := tc.Handshake(); err != nil {
		t.Fatal(err)
	}
	_ = tc.SetDeadline(time.Now().Add(5 * time.Second))

	// Send raw chunked body
	fmt.Fprint(tc, "POST / HTTP/1.1\r\nHost: allowed.example\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n\r\n")

	br := bufio.NewReader(tc)
	resp, err := http.ReadResponse(br, &http.Request{Method: "POST"})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("chunked status = %d, want 400", resp.StatusCode)
	}
}

func TestMITMKeepAliveThenBlocked(t *testing.T) {
	upCA, _ := tlsmitm.NewCA()
	var seen seenHeaders
	addr, roots := testUpstream(t, upCA, "allowed.example", seen.handler("ok"))
	proxyCA, _ := tlsmitm.NewCA()
	policy := newPolicy()
	policy.DenyRequestBody = true
	proxyAddr := testProxy(t, proxyCA, addr, roots, policy)

	raw, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	tc := tls.Client(raw, &tls.Config{RootCAs: poolFor(t, proxyCA), ServerName: "allowed.example"})
	if err := tc.Handshake(); err != nil {
		t.Fatal(err)
	}
	_ = tc.SetDeadline(time.Now().Add(5 * time.Second))

	br := bufio.NewReader(tc)

	// 1st request: GET without body -> 200
	fmt.Fprint(tc, "GET / HTTP/1.1\r\nHost: allowed.example\r\n\r\n")
	resp1, err := http.ReadResponse(br, &http.Request{Method: "GET"})
	if err != nil {
		t.Fatalf("req 1 failed: %v", err)
	}
	io.Copy(io.Discard, resp1.Body)
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("req 1 status = %d, want 200", resp1.StatusCode)
	}

	// 2nd request on the same connection: POST with body -> 400
	fmt.Fprint(tc, "POST / HTTP/1.1\r\nHost: allowed.example\r\nContent-Length: 4\r\n\r\ndata")
	resp2, err := http.ReadResponse(br, &http.Request{Method: "POST"})
	if err != nil {
		t.Fatalf("req 2 failed: %v", err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("req 2 status = %d, want 400", resp2.StatusCode)
	}

	// Connection should now be closed by the proxy (Connection: close)
	buf := make([]byte, 1)
	n, _ := tc.Read(buf)
	if n > 0 {
		t.Fatalf("connection was not closed after blocked request")
	}
}

func TestMITMWildcardHostRelaxation(t *testing.T) {
	upCA, _ := tlsmitm.NewCA()
	var seen seenHeaders
	addr, roots := testUpstream(t, upCA, "sub.pkg.example", seen.handler("ok"))
	proxyCA, _ := tlsmitm.NewCA()
	policy := newPolicy()
	policy.DenyRequestBody = true
	policy.AllowedMethods = []string{"GET"}
	policy.Hosts = map[string]headerpolicy.HostRule{
		"*.pkg.example": {
			AllowRequestBody: true,
			AllowedMethods:   []string{"PUT"},
		},
	}

	p := &webProxy{
		allowed:       func(name string) bool { return true },
		mitm:          proxyCA,
		headers:       policy,
		upstreamRoots: roots,
		sem:           make(chan struct{}, 8),
		dial: func(network, addr string, mark int) (net.Conn, error) {
			return net.Dial(network, addr)
		},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.terminate(c, addr, "sub.pkg.example")
		}
	}()
	proxyAddr := ln.Addr().String()
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: poolFor(t, proxyCA), ServerName: "sub.pkg.example"},
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("tcp", proxyAddr)
			},
		},
	}

	// PUT with body to sub.pkg.example should succeed (200)
	req, _ := http.NewRequest("PUT", "https://sub.pkg.example/", strings.NewReader("package data"))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", resp.StatusCode)
	}
}
