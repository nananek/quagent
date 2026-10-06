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

	"github.com/nananek/quagent/internal/tlsmitm"
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

// testProxy は terminate を直に叩く透明プロキシを立てる (originalDst は使わない)。
// 上流への接続は holderPid を使わず、渡したテストサーバーへ張る。
func testProxy(t *testing.T, proxyCA *tlsmitm.CA, upstreamAddr string, roots *x509.CertPool, inspect func(InspectRequest) error, limit int) string {
	t.Helper()
	p := &webProxy{
		allowed:       func(name string) bool { return name == "allowed.example" },
		mitm:          proxyCA,
		inspect:       inspect,
		inspectLimit:  limit,
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

func TestPlainHTTPInspects(t *testing.T) {
	upLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "%s %s body=%s", r.Method, r.URL.RequestURI(), body)
	})}
	go srv.Serve(upLn)
	t.Cleanup(func() { _ = srv.Close() })

	var mu sync.Mutex
	var seen []InspectRequest
	p := &webProxy{
		allowed:      func(name string) bool { return name == "allowed.example" },
		inspect:      func(req InspectRequest) error { mu.Lock(); seen = append(seen, req); mu.Unlock(); return nil },
		inspectLimit: 1 << 20,
		sem:          make(chan struct{}, 8),
		dial:         func(network, addr string, mark int) (net.Conn, error) { return net.Dial(network, upLn.Addr().String()) },
	}
	proxyAddr := testProxyFn(t, func(c net.Conn) {
		p.serveInspect(c, bufio.NewReader(c), "203.0.113.9:80", "", false)
	})

	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprint(c, "POST /p?q=1 HTTP/1.1\r\nHost: allowed.example\r\nContent-Length: 3\r\n\r\nabc")
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: "POST"})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "POST /p?q=1 body=abc" {
		t.Fatalf("応答 = %q", body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0].Provider != "http" || seen[0].Host != "allowed.example" {
		t.Fatalf("点検内容 = %+v", seen)
	}
}

func TestPlainHTTPBlocks(t *testing.T) {
	upLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "should not reach")
	})}
	go srv.Serve(upLn)
	t.Cleanup(func() { _ = srv.Close() })

	p := &webProxy{
		allowed:      func(name string) bool { return name == "allowed.example" },
		inspect:      func(req InspectRequest) error { return fmt.Errorf("秘密の持ち出し") },
		inspectLimit: 1 << 20,
		sem:          make(chan struct{}, 8),
		dial:         func(network, addr string, mark int) (net.Conn, error) { return net.Dial(network, upLn.Addr().String()) },
	}
	proxyAddr := testProxyFn(t, func(c net.Conn) {
		p.serveInspect(c, bufio.NewReader(c), "203.0.113.9:80", "", false)
	})

	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprint(c, "GET /secret HTTP/1.1\r\nHost: allowed.example\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: "GET"})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("状態 = %d, want 403", resp.StatusCode)
	}
}

func TestPlainHTTPDisallowedHost(t *testing.T) {
	p := &webProxy{
		allowed:      func(name string) bool { return false },
		inspect:      func(req InspectRequest) error { return nil },
		inspectLimit: 1 << 20,
		sem:          make(chan struct{}, 8),
		dial:         func(network, addr string, mark int) (net.Conn, error) { return net.Dial(network, addr) },
	}
	proxyAddr := testProxyFn(t, func(c net.Conn) {
		p.serveInspect(c, bufio.NewReader(c), "203.0.113.9:80", "", false)
	})
	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: evil.example\r\n\r\n")
	if _, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: "GET"}); err == nil {
		t.Fatal("許可外の Host が通った")
	}
}

func TestMITMForwardsAndInspects(t *testing.T) {
	upCA, _ := tlsmitm.NewCA()
	addr, roots := testUpstream(t, upCA, "allowed.example", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "%s %s body=%s", r.Method, r.URL.RequestURI(), body)
	}))
	proxyCA, _ := tlsmitm.NewCA()

	var mu sync.Mutex
	var got []InspectRequest
	proxyAddr := testProxy(t, proxyCA, addr, roots, func(req InspectRequest) error {
		mu.Lock()
		got = append(got, req)
		mu.Unlock()
		return nil
	}, 1<<20)

	client := &http.Client{Timeout: 10 * time.Second, Transport: dialProxy(t, proxyAddr, proxyCA)}
	resp, err := client.Get("https://allowed.example/path?q=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "GET /path?q=1") {
		t.Fatalf("応答 = %d %q", resp.StatusCode, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("点検回数 = %d, want 1", len(got))
	}
	r := got[0]
	if r.Method != "GET" || r.Host != "allowed.example" || r.Path != "/path" || r.Query != "q=1" {
		t.Fatalf("点検内容 = %+v", r)
	}
	if r.Headers.Get("Host") != "allowed.example" {
		t.Fatalf("Host ヘッダが点検に見えていない: %v", r.Headers)
	}
}

func TestMITMStripsHopByHopHeadersBeforeInspect(t *testing.T) {
	upCA, _ := tlsmitm.NewCA()
	addr, roots := testUpstream(t, upCA, "allowed.example", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	proxyCA, _ := tlsmitm.NewCA()

	var mu sync.Mutex
	var seen http.Header
	proxyAddr := testProxy(t, proxyCA, addr, roots, func(req InspectRequest) error {
		mu.Lock()
		seen = req.Headers
		mu.Unlock()
		return nil
	}, 1<<20)

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

	mu.Lock()
	defer mu.Unlock()
	if seen.Get("X-Keep") != "yes" {
		t.Fatalf("転送するヘッダが点検に見えていない: %v", seen)
	}
	if seen.Get("Proxy-Connection") != "" || seen.Get("X-Drop-Me") != "" || seen.Get("Connection") != "" {
		t.Fatalf("転送しないヘッダが点検に見えている: %v", seen)
	}
}

func TestMITMBlocks(t *testing.T) {
	upCA, _ := tlsmitm.NewCA()
	addr, roots := testUpstream(t, upCA, "allowed.example", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "should not reach")
	}))
	proxyCA, _ := tlsmitm.NewCA()
	proxyAddr := testProxy(t, proxyCA, addr, roots, func(req InspectRequest) error {
		return fmt.Errorf("秘密の持ち出し")
	}, 1<<20)

	client := &http.Client{Timeout: 10 * time.Second, Transport: dialProxy(t, proxyAddr, proxyCA)}
	resp, err := client.Get("https://allowed.example/secret")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 403 {
		t.Fatalf("状態 = %d, want 403", resp.StatusCode)
	}
	if !strings.Contains(string(body), "blocked") {
		t.Fatalf("本文 = %q", body)
	}
}

func TestMITMPeeksBodyButForwardsAll(t *testing.T) {
	upCA, _ := tlsmitm.NewCA()
	addr, roots := testUpstream(t, upCA, "allowed.example", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		io.WriteString(w, "body="+string(body))
	}))
	proxyCA, _ := tlsmitm.NewCA()

	var mu sync.Mutex
	var seen InspectRequest
	proxyAddr := testProxy(t, proxyCA, addr, roots, func(req InspectRequest) error {
		mu.Lock()
		seen = req
		mu.Unlock()
		return nil
	}, 4) // 4 バイトだけ点検する

	client := &http.Client{Timeout: 10 * time.Second, Transport: dialProxy(t, proxyAddr, proxyCA)}
	resp, err := client.Post("https://allowed.example/upload", "text/plain", strings.NewReader("0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "body=0123456789" {
		t.Fatalf("上流に届いた本文 = %q (点検で切ってはいけない)", body)
	}
	mu.Lock()
	defer mu.Unlock()
	if string(seen.Body) != "0123" || !seen.Truncated {
		t.Fatalf("点検した本文 = %q truncated=%v", seen.Body, seen.Truncated)
	}
}

func TestMITMRejectsUntrustedUpstream(t *testing.T) {
	// 上流の証明書を検証できないとき (roots が system のまま) は通さない。
	upCA, _ := tlsmitm.NewCA()
	addr, _ := testUpstream(t, upCA, "allowed.example", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "should not reach")
	}))
	proxyCA, _ := tlsmitm.NewCA()
	proxyAddr := testProxy(t, proxyCA, addr, nil, func(req InspectRequest) error { return nil }, 1<<20)

	client := &http.Client{Timeout: 10 * time.Second, Transport: dialProxy(t, proxyAddr, proxyCA)}
	if _, err := client.Get("https://allowed.example/"); err == nil {
		t.Fatal("検証できない上流への転送が通ってしまった")
	}
}

func TestMITMKeepAlive(t *testing.T) {
	upCA, _ := tlsmitm.NewCA()
	addr, roots := testUpstream(t, upCA, "allowed.example", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	proxyCA, _ := tlsmitm.NewCA()
	var n int
	var mu sync.Mutex
	proxyAddr := testProxy(t, proxyCA, addr, roots, func(req InspectRequest) error {
		mu.Lock()
		n++
		mu.Unlock()
		return nil
	}, 1<<20)

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
	}
	mu.Lock()
	defer mu.Unlock()
	if n != 3 {
		t.Fatalf("点検回数 = %d, want 3", n)
	}
}

func TestMITMUpgradeTunnel(t *testing.T) {
	upCA, _ := tlsmitm.NewCA()
	addr, roots := testUpstream(t, upCA, "allowed.example", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "" {
			http.Error(w, "no upgrade", http.StatusBadRequest)
			return
		}
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
	var inspected bool
	proxyAddr := testProxy(t, proxyCA, addr, roots, func(req InspectRequest) error {
		inspected = true
		return nil
	}, 1<<20)

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
	io.WriteString(tc, "GET /ws HTTP/1.1\r\nHost: allowed.example\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	br := bufio.NewReader(tc)
	resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 101 {
		t.Fatalf("状態 = %d, want 101", resp.StatusCode)
	}
	if !inspected {
		t.Fatal("Upgrade の要求が点検されていない")
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
