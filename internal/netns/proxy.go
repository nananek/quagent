package netns

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// webProxy は、許可した IP への Web 接続 (TCP 80/443) を横取りし、TLS の SNI (443)
// と HTTP の Host (80) が許可名に一致することを確かめる透明プロキシ。
//
// 許可は IP 単位なので、allow set に入った IP を共有する別のホスト (共有 CDN) や、
// DNS を操って任意の公開 IP を指した許可名へ、そのまま届いてしまう。ここで接続先が
// 実際に言ってきた名前を見て、許可名と一致しなければ止める。TLS は終端しない
// (最初の ClientHello を読むだけで、あとは素通しする)。
type webProxy struct {
	holderPid int
	allowed   func(name string) bool
	blocked   func(reason string)
	// sem は同時に扱う Web 接続の上限。溢れた分は切って guest に任せる。
	sem chan struct{}
}

const (
	// proxyTLSPort / proxyHTTPPort は透明プロキシが子 netns の loopback で待ち受ける
	// ポート (nft の redirect 先)。
	proxyTLSPort  = 8443
	proxyHTTPPort = 8080
	// proxyMark はプロキシ自身の外向き接続に付ける印 (SO_MARK)。nft はこれを
	// redirect しない (付けないと自分の転送を自分で横取りしてしまう)。
	proxyMark = 1
	// proxyMaxConns は同時に扱う Web 接続の上限。
	proxyMaxConns = 64
	// proxyHandshake は ClientHello / リクエストヘッダを読み終えるまでの上限。
	proxyHandshake = 30 * time.Second
	// proxyMaxHead は読み取る ClientHello / ヘッダの上限。
	proxyMaxHead = 64 << 10
)

func newWebProxy(holderPid int, allowed func(name string) bool, blocked func(reason string)) *webProxy {
	return &webProxy{holderPid: holderPid, allowed: allowed, blocked: blocked,
		sem: make(chan struct{}, proxyMaxConns)}
}

// serve は listener で受けた接続を点検する。tls なら 443 の接続として SNI を、
// そうでなければ 80 の接続として Host を見る。
func (p *webProxy) serve(l net.Listener, tls bool) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		select {
		case p.sem <- struct{}{}:
		default:
			_ = c.Close()
			continue
		}
		go func() {
			defer func() { <-p.sem }()
			p.handle(c, tls)
		}()
	}
}

func (p *webProxy) handle(c net.Conn, tls bool) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(proxyHandshake))
	dst, err := originalDst(c)
	if err != nil {
		log.Printf("web: 元の宛先を取得できない: %v", err)
		return
	}
	// 点検で読んだ分 (ClientHello / ヘッダ) を覚えておき、そのまま転送する。
	cr := &captureReader{r: c}
	var name string
	if tls {
		name, err = readSNI(cr)
	} else {
		name, err = readHost(bufio.NewReader(cr))
	}
	if err != nil {
		p.block(fmt.Sprintf("%s %s (%v)", protoName(tls), dst, err))
		return
	}
	if !p.allowed(name) {
		p.block(fmt.Sprintf("%s %s", protoName(tls), name))
		return
	}
	// 元の宛先へ、子 netns の中から (mark を付けて) 張り直す。mark が無いと
	// nft がこの転送をまた redirect してしまう。
	up, err := dialInNetns(p.holderPid, "tcp4", dst, proxyMark)
	if err != nil {
		log.Printf("web: %s への転送に失敗: %v", name, err)
		return
	}
	defer up.Close()
	// 点検で読んだ分も含めてそのまま流す。
	_ = c.SetDeadline(time.Time{})
	errc := make(chan error, 2)
	go func() { _, err := io.Copy(up, io.MultiReader(bytes.NewReader(cr.buf), c)); errc <- err }()
	go func() { _, err := io.Copy(c, up); errc <- err }()
	<-errc
}

// captureReader は点検で読んだバイトを覚え、点検後にそのまま転送できるようにする。
type captureReader struct {
	r   io.Reader
	buf []byte
}

func (cr *captureReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	if n > 0 {
		cr.buf = append(cr.buf, p[:n]...)
	}
	return n, err
}

func (p *webProxy) block(reason string) {
	if p.blocked != nil {
		p.blocked(reason)
	}
}

func protoName(tls bool) string {
	if tls {
		return "SNI"
	}
	return "Host"
}

// originalDst は nft の redirect で書き換えられる前の宛先 (SO_ORIGINAL_DST) を返す。
func originalDst(c net.Conn) (string, error) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return "", errors.New("TCP の接続ではない")
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		return "", err
	}
	var dst string
	var serr error
	if err := raw.Control(func(fd uintptr) {
		m, e := unix.GetsockoptIPv6Mreq(int(fd), unix.SOL_IP, unix.SO_ORIGINAL_DST)
		if e != nil {
			serr = e
			return
		}
		// sockaddr_in: family(2)・port(2, network order)・addr(4)。
		ip := net.IPv4(m.Multiaddr[4], m.Multiaddr[5], m.Multiaddr[6], m.Multiaddr[7])
		port := int(binary.BigEndian.Uint16(m.Multiaddr[2:4]))
		dst = net.JoinHostPort(ip.String(), strconv.Itoa(port))
	}); err != nil {
		return "", err
	}
	return dst, serr
}

// dialInNetns は子 netns の中に socket を作って addr へ接続する。mark は nft に
// redirect させないための印 (SO_MARK)。userns の root なので CAP_NET_ADMIN がある。
func dialInNetns(holderPid int, network, addr string, mark int) (net.Conn, error) {
	type result struct {
		c   net.Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		// 子 netns へ setns したスレッドは捨てる (UnlockOSThread せず goroutine を終える)。
		runtime.LockOSThread()
		fd, err := unix.Open(fmt.Sprintf("/proc/%d/ns/net", holderPid), unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			ch <- result{err: err}
			return
		}
		err = unix.Setns(fd, unix.CLONE_NEWNET)
		unix.Close(fd)
		if err != nil {
			ch <- result{err: fmt.Errorf("setns: %w", err)}
			return
		}
		d := net.Dialer{
			Timeout: 10 * time.Second,
			Control: func(network, address string, rc syscall.RawConn) error {
				var serr error
				if err := rc.Control(func(fd uintptr) {
					serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, mark)
				}); err != nil {
					return err
				}
				return serr
			},
		}
		conn, err := d.Dial(network, addr)
		ch <- result{c: conn, err: err}
	}()
	r := <-ch
	return r.c, r.err
}

// tcpListenerInNetns は子 netns の中に TCP listener を作る (listenInNetns と同じ理屈)。
func tcpListenerInNetns(holderPid int, addr string) (net.Listener, error) {
	type result struct {
		l   net.Listener
		err error
	}
	ch := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		fd, err := unix.Open(fmt.Sprintf("/proc/%d/ns/net", holderPid), unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			ch <- result{err: err}
			return
		}
		defer unix.Close(fd)
		if err := unix.Setns(fd, unix.CLONE_NEWNET); err != nil {
			ch <- result{err: fmt.Errorf("setns: %w", err)}
			return
		}
		l, err := net.Listen("tcp4", addr)
		ch <- result{l: l, err: err}
	}()
	r := <-ch
	return r.l, r.err
}

// readSNI は TLS の ClientHello を読み、SNI を取り出す。ClientHello は複数の
// TLS レコードに分かれうるので、handshake メッセージが揃うまで読む。
func readSNI(r io.Reader) (string, error) {
	hs, err := readHandshake(r)
	if err != nil {
		return "", err
	}
	name, ok := parseClientHello(hs)
	if !ok {
		return "", errors.New("SNI が無い")
	}
	return name, nil
}

// readHandshake は TLS レコードを繋いで最初の handshake メッセージ (ClientHello) を返す。
func readHandshake(r io.Reader) ([]byte, error) {
	var hs []byte
	var hdr [5]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return nil, err
		}
		if hdr[0] != 0x16 {
			return nil, fmt.Errorf("TLS のハンドシェイクではない (content type %d)", hdr[0])
		}
		n := int(binary.BigEndian.Uint16(hdr[3:5]))
		if n == 0 || len(hs)+n > proxyMaxHead {
			return nil, errors.New("ClientHello が大きすぎる")
		}
		rec := make([]byte, n)
		if _, err := io.ReadFull(r, rec); err != nil {
			return nil, err
		}
		hs = append(hs, rec...)
		if len(hs) < 4 {
			continue
		}
		if hs[0] != 0x01 {
			return nil, fmt.Errorf("ClientHello ではない (handshake type %d)", hs[0])
		}
		if l := int(hs[1])<<16 | int(hs[2])<<8 | int(hs[3]); len(hs) >= 4+l {
			return hs[:4+l], nil
		}
	}
}

// parseClientHello は handshake メッセージ (先頭が type/length) から SNI を探す。
// 見つかればホスト名 (小文字)、無ければ ok=false。
func parseClientHello(b []byte) (string, bool) {
	if len(b) < 4 || b[0] != 0x01 {
		return "", false
	}
	l := int(b[1])<<16 | int(b[2])<<8 | int(b[3])
	if len(b) < 4+l {
		return "", false
	}
	body := b[4 : 4+l]
	// client_version(2) + random(32)
	p := 34
	// session_id
	if p >= len(body) {
		return "", false
	}
	p += 1 + int(body[p])
	// cipher_suites
	if p+2 > len(body) {
		return "", false
	}
	p += 2 + int(binary.BigEndian.Uint16(body[p:]))
	// compression_methods
	if p+1 > len(body) {
		return "", false
	}
	p += 1 + int(body[p])
	// extensions
	if p+2 > len(body) {
		return "", false
	}
	p += 2
	end := p + int(binary.BigEndian.Uint16(body[p-2:]))
	if end > len(body) {
		return "", false
	}
	for p+4 <= end {
		typ := binary.BigEndian.Uint16(body[p:])
		el := int(binary.BigEndian.Uint16(body[p+2:]))
		p += 4
		if p+el > end {
			return "", false
		}
		if typ == 0x0000 {
			return parseServerName(body[p : p+el])
		}
		p += el
	}
	return "", false
}

// parseServerName は server_name 拡張から host_name (type 0) を取り出す。
func parseServerName(b []byte) (string, bool) {
	if len(b) < 2 {
		return "", false
	}
	n := int(binary.BigEndian.Uint16(b))
	b = b[2:]
	if n > len(b) {
		return "", false
	}
	for len(b) >= 3 {
		typ := b[0]
		l := int(binary.BigEndian.Uint16(b[1:]))
		b = b[3:]
		if l > len(b) {
			return "", false
		}
		if typ == 0 {
			return normalize(string(b[:l])), true
		}
		b = b[l:]
	}
	return "", false
}

// readHost は HTTP リクエストヘッダを読み、Host (CONNECT なら要求先) を取り出す。
func readHost(r *bufio.Reader) (string, error) {
	var b []byte
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return "", err
		}
		if len(b)+len(line) > proxyMaxHead {
			return "", errors.New("HTTP ヘッダが大きすぎる")
		}
		b = append(b, line...)
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	return hostFromRequest(b)
}

// hostFromRequest は HTTP/1.x のリクエストから接続先の名前を取り出す。Host が
// 無ければ止める側に倒す (HTTP/1.0 などでも黙って通さない)。
func hostFromRequest(b []byte) (string, error) {
	lines := strings.Split(string(b), "\n")
	if len(lines) == 0 {
		return "", errors.New("空のリクエスト")
	}
	first := strings.Fields(strings.TrimRight(lines[0], "\r"))
	if len(first) < 2 {
		return "", errors.New("リクエスト行が不正")
	}
	if first[0] == "CONNECT" {
		return normalizeHost(first[1])
	}
	for _, ln := range lines[1:] {
		ln = strings.TrimRight(ln, "\r")
		if ln == "" {
			break
		}
		k, v, ok := strings.Cut(ln, ":")
		if ok && strings.EqualFold(strings.TrimSpace(k), "Host") {
			return normalizeHost(strings.TrimSpace(v))
		}
	}
	return "", errors.New("Host ヘッダが無い")
}

// normalizeHost は Host からポートを落として小文字にする。
func normalizeHost(h string) (string, error) {
	h = strings.TrimSpace(h)
	if h == "" {
		return "", errors.New("Host が空")
	}
	if strings.HasPrefix(h, "[") {
		if i := strings.IndexByte(h, ']'); i >= 0 {
			h = h[1:i]
		}
	} else if i := strings.LastIndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	if h == "" {
		return "", errors.New("Host が空")
	}
	return normalize(h), nil
}
