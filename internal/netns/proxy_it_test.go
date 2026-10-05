package netns

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestProxyIntegration は子 netns を作り、nft の redirect から透明プロキシ、
// 元の宛先への張り直しまでを実際に通す。unshare -n と nft が要るので既定では
// 実行しない。userns の中で次のように実行する:
//
//	go test -c -o /tmp/netns.test ./internal/netns
//	QUAGENT_PROXY_IT=1 unshare -Urm /tmp/netns.test -test.run TestProxyIntegration -test.v
func TestProxyIntegration(t *testing.T) {
	if os.Getenv("QUAGENT_PROXY_IT") == "" {
		t.Skip("QUAGENT_PROXY_IT=1 で実行")
	}
	holder := exec.Command("unshare", "-n", "sh", "-c", "ip link set lo up; exec sleep infinity")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Process.Kill(); _, _ = holder.Process.Wait() }()
	pid := holder.Process.Pid
	ns := strconv.Itoa(pid)

	// 子 netns ができ、lo が上がるまで待つ (child.go と同じ)。
	selfNS, _ := os.Readlink("/proc/self/ns/net")
	if !waitFor(5*time.Second, func() bool {
		got, err := os.Readlink("/proc/" + ns + "/ns/net")
		return err == nil && got != selfNS
	}) {
		t.Fatal("子 netns が作成されない")
	}
	if !waitFor(5*time.Second, func() bool {
		out, err := exec.Command("nsenter", "-t", ns, "-n", "--", "ip", "-o", "link", "show", "lo").Output()
		return err == nil && strings.Contains(string(out), "UP")
	}) {
		t.Fatal("子 netns の lo が上がらない")
	}

	must := func(args ...string) {
		t.Helper()
		out, err := exec.Command("nsenter", append([]string{"-t", ns, "-n", "--"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	must("ip", "addr", "add", "203.0.113.5/32", "dev", "lo")

	nft := func(script string) error {
		cmd := exec.Command("nsenter", "-t", ns, "-n", "--", "nft", "-f", "-")
		cmd.Stdin = strings.NewReader(script)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
		return nil
	}
	if err := nft(egressRules()); err != nil {
		t.Fatal(err)
	}
	eg := newEgress(nft, &eventWriter{enc: json.NewEncoder(io.Discard)}, []Grant{{Pattern: "allowed.example"}})
	eg.onAnswer("allowed.example", []net.IP{net.ParseIP("203.0.113.5")})

	cert := selfSignedCert(t, "allowed.example")
	upLn, err := tcpListenerInNetns(pid, "203.0.113.5:443")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := upLn.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}})
				if err := tc.Handshake(); err != nil {
					return
				}
				_, _ = io.Copy(tc, tc) // そのまま返す
			}()
		}
	}()

	p := newWebProxy(pid, eg.allowed, eg.webBlocked)
	pxLn, err := tcpListenerInNetns(pid, "127.0.0.1:"+strconv.Itoa(proxyTLSPort))
	if err != nil {
		t.Fatal(err)
	}
	go p.serve(pxLn, true)

	// 上流の HTTP サーバー (203.0.113.5:80) とプロキシの HTTP 待ち受け
	httpLn, err := tcpListenerInNetns(pid, "203.0.113.5:80")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := httpLn.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					if line == "\r\n" || line == "\n" {
						break
					}
				}
				_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
			}()
		}
	}()
	pxHTTP, err := tcpListenerInNetns(pid, "127.0.0.1:"+strconv.Itoa(proxyHTTPPort))
	if err != nil {
		t.Fatal(err)
	}
	go p.serve(pxHTTP, false)

	// 許可した SNI は元の宛先まで通り、往復できる
	if got := proxyRoundTrip(t, pid, "allowed.example"); got != "hello" {
		t.Fatalf("許可した SNI の往復 = %q, want hello", got)
	}
	// 許可外の SNI は止まる
	if err := proxyTryHandshake(t, pid, "evil.example"); err == nil {
		t.Fatal("許可外の SNI が通った")
	}
	// 許可した Host は通る
	if got := httpRequest(t, pid, "allowed.example"); got != "HTTP/1.1 200 OK" {
		t.Fatalf("許可した Host の応答 = %q", got)
	}
	// 許可外の Host は止まる
	if got, err := httpRequestErr(t, pid, "evil.example"); err == nil {
		t.Fatalf("許可外の Host が通った: %q", got)
	}
}

func httpConn(pid int) (net.Conn, error) {
	return dialInNetns(pid, "tcp4", "203.0.113.5:80", 0)
}

func httpRequest(t *testing.T, pid int, host string) string {
	t.Helper()
	got, err := httpRequestErr(t, pid, host)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func httpRequestErr(t *testing.T, pid int, host string) (string, error) {
	t.Helper()
	c, err := httpConn(pid)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", host)
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func proxyDial(pid int, sni string) (*tls.Conn, error) {
	conn, err := dialInNetns(pid, "tcp4", "203.0.113.5:443", 0)
	if err != nil {
		return nil, err
	}
	tc := tls.Client(conn, &tls.Config{ServerName: sni, InsecureSkipVerify: true})
	_ = tc.SetDeadline(time.Now().Add(10 * time.Second))
	return tc, nil
}

func proxyRoundTrip(t *testing.T, pid int, sni string) string {
	t.Helper()
	tc, err := proxyDial(pid, sni)
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	if err := tc.Handshake(); err != nil {
		t.Fatal(err)
	}
	if _, err := tc.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(tc, buf); err != nil {
		t.Fatal(err)
	}
	return string(buf)
}

func proxyTryHandshake(t *testing.T, pid int, sni string) error {
	t.Helper()
	tc, err := proxyDial(pid, sni)
	if err != nil {
		return err
	}
	defer tc.Close()
	return tc.Handshake()
}

func selfSignedCert(t *testing.T, name string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
