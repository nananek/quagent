package netns

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/http2"
)

// terminate は許可済みの TLS 接続を終端し、平文になった HTTP のヘッダを絞って
// から、元の宛先へ TLS で張り直して転送する。クライアントには CA が署名した SNI 用の
// 証明書を提示する。ALPN で h2 が選ばれれば h2 として、そうでなければ HTTP/1.1 として
// 扱う (h2 しか使わないクライアントも扱える)。
func (p *webProxy) terminate(client net.Conn, dst, sni string) {
	cert, err := p.mitm.Leaf(sni)
	if err != nil {
		log.Printf("tlsmitm: %s の証明書を作れない: %v", sni, err)
		return
	}
	tc := tls.Server(client, &tls.Config{
		Certificates: []tls.Certificate{cert},
		// h2 と 1.1 の両方を名乗り、クライアントに選ばせる。選ばれた方で中身を
		// 解析する (h2 に対応していないクライアントは 1.1 に落ちる)。
		NextProtos: []string{"h2", "http/1.1"},
		MinVersion: tls.VersionTLS12,
	})
	defer tc.Close()
	_ = tc.SetDeadline(time.Now().Add(proxyHandshake))
	if err := tc.Handshake(); err != nil {
		log.Printf("tlsmitm: %s のハンドシェイクに失敗: %v", sni, err)
		return
	}
	_ = tc.SetDeadline(time.Time{})
	if tc.ConnectionState().NegotiatedProtocol == "h2" {
		p.serveHTTP2(tc, dst, sni)
		return
	}
	p.serveHTTP1(tc, bufio.NewReader(tc), dst, sni, true)
}

// serveHTTP2 は h2 で来た 1 本の接続を、ストリームごとに点検して上流へ h2 で
// 中継する。h2 しか使わないクライアント (gRPC など) も点検できるようにするため。
// 上流へも h2 で張り直すので、上流が h2 を選べないときは中継できない。
func (p *webProxy) serveHTTP2(client net.Conn, dst, name string) {
	tr := &http2.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
			return p.dialUpstreamH2(dst, name)
		},
	}
	defer tr.CloseIdleConnections()
	h2 := &http2.Server{}
	h2.ServeConn(client, &http2.ServeConnOpts{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p.serveH2Stream(w, r, tr, name)
		}),
	})
}

// serveH2Stream は h2 の 1 ストリームをヘッダを絞って上流へ中継する。:authority が
// 接続時の SNI と違うものは、張り直す先が違うことになるので通さない。
func (p *webProxy) serveH2Stream(w http.ResponseWriter, r *http.Request, tr *http2.Transport, name string) {
	if h, err := normalizeHost(r.Host); err != nil || h != name {
		p.block(fmt.Sprintf("Host %s (SNI %s と一致しない)", r.Host, name))
		http.Error(w, "quagent: SNI と Host が一致しない", http.StatusForbidden)
		return
	}
	normalizeRequest(r, name, true)
	removeHopHeaders(r.Header)
	if p.headers != nil || p.hasRelaxations() {
		rules := p.currentRules(name)
		if code, reason := rules.CheckRequest(r); code != 0 {
			p.block(fmt.Sprintf("%s %s (%s)", r.Method, name, reason))
			http.Error(w, "quagent: "+reason, code)
			return
		}
	}
	if p.dlp != nil {
		if detected, pattern, preview, err := p.dlp.InspectRequest(r); err == nil && detected {
			p.block(fmt.Sprintf("DLP %s %s (%s: %s)", r.Method, name, pattern, preview))
			http.Error(w, fmt.Sprintf("quagent: DLP violation (%s)", pattern), http.StatusForbidden)
			return
		}
	}
	p.filter(r, name)
	out, err := tr.RoundTrip(r)
	if err != nil {
		log.Printf("web: %s へ h2 で中継できない: %v", name, err)
		http.Error(w, "quagent: upstream error", http.StatusBadGateway)
		return
	}
	defer out.Body.Close()
	removeHopHeaders(out.Header)
	for k, vs := range out.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(out.StatusCode)
	_, _ = io.Copy(w, out.Body)
}

// serveHTTP1 は 1 本のクライアント接続の HTTP/1.1 を、リクエストごとに点検して
// 上流へ中継する。secure なら上流へ TLS で、そうでなければ平文で張り直す。name は
// 確認済みの接続先 (secure なら SNI、平文なら最初の Host から引き継ぐ)。
func (p *webProxy) serveHTTP1(client net.Conn, br *bufio.Reader, dst, name string, secure bool) {
	var up net.Conn
	var upBR *bufio.Reader
	defer func() {
		if up != nil {
			up.Close()
		}
	}()
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		host := name
		if !secure {
			// 平文は接続ごとに Host が変わりうるので、毎回確かめる。
			host, err = normalizeHost(req.Host)
			if err != nil {
				p.block(fmt.Sprintf("Host %s (%v)", dst, err))
				return
			}
			if !p.allowed(host) {
				p.block("Host " + host)
				return
			}
		}
		normalizeRequest(req, host, secure)
		upgrade := isUpgrade(req)
		// 転送するヘッダだけを点検に見せる。接続ごとのヘッダ (Connection が名指し
		// したもの、Proxy-Connection、Te など) は転送しないので、点検の対象からも
		// 外す。Upgrade の要求はハンドシェイクに要るので落とさない。
		if !upgrade {
			removeHopHeaders(req.Header)
		}
		if p.headers != nil || p.hasRelaxations() {
			rules := p.currentRules(host)
			if code, reason := rules.CheckRequest(req); code != 0 {
				p.block(fmt.Sprintf("%s %s (%s)", req.Method, host, reason))
				_ = writeHTTP1Error(client, code, "quagent: "+reason)
				return
			}
		}
		if p.dlp != nil {
			if detected, pattern, preview, err := p.dlp.InspectRequest(req); err == nil && detected {
				p.block(fmt.Sprintf("DLP %s %s (%s: %s)", req.Method, host, pattern, preview))
				_ = writeHTTP1Error(client, http.StatusForbidden, fmt.Sprintf("quagent: DLP violation (%s)", pattern))
				return
			}
		}
		// Expect: 100-continue のクライアントは本文を待っているので、点検で本文を
		// 読む前に続行を伝える。応答済みなので転送するときは Expect を落とす。
		if hasToken(req.Header, "Expect", "100-continue") {
			if _, err := io.WriteString(client, "HTTP/1.1 100 Continue\r\n\r\n"); err != nil {
				return
			}
			req.Header.Del("Expect")
		}
		p.filter(req, host)
		if up == nil {
			if secure {
				up, err = p.dialUpstream(dst, host)
			} else {
				up, err = p.dial("tcp4", dst, proxyMark)
			}
			if err != nil {
				log.Printf("web: %s (%s) へ張り直せない: %v", host, dst, err)
				return
			}
			upBR = bufio.NewReader(up)
		}
		if upgrade {
			// WebSocket など。ハンドシェイクの要求は点検したので、あとは素通しする。
			p.tunnel(client, up, br, upBR, req)
			return
		}
		if err := req.Write(up); err != nil {
			return
		}
		// 1xx の中間応答 (100 Continue など) は読み飛ばして最終応答を待つ。
		var resp *http.Response
		for {
			resp, err = http.ReadResponse(upBR, req)
			if err != nil {
				return
			}
			if resp.StatusCode == http.StatusSwitchingProtocols || resp.StatusCode >= 200 {
				break
			}
			resp.Body.Close()
		}
		removeHopHeaders(resp.Header)
		if err := resp.Write(client); err != nil {
			resp.Body.Close()
			return
		}
		resp.Body.Close()
		if req.Close || resp.Close {
			return
		}
	}
}

// normalizeRequest は点検と転送のために、リクエストの行き先を確認済みの名前に揃える。
func normalizeRequest(req *http.Request, name string, secure bool) {
	req.RequestURI = ""
	if req.URL == nil {
		req.URL = &url.URL{}
	}
	if secure {
		req.URL.Scheme = "https"
	} else {
		req.URL.Scheme = "http"
	}
	req.URL.Host = name
	if req.Host == "" {
		req.Host = name
	}
}

// dialUpstream は元の IP (dst) へ子 netns の中から張り直し、SNI を確かめたうえで
// TLS を張る。証明書は system のルートで検証する (終端したからといって上流の検証を
// 緩めない)。
func (p *webProxy) dialUpstream(dst, sni string) (net.Conn, error) {
	raw, err := p.dial("tcp4", dst, proxyMark)
	if err != nil {
		return nil, err
	}
	name := sni
	if name == "" {
		name, _, _ = net.SplitHostPort(dst)
	}
	tc := tls.Client(raw, &tls.Config{
		ServerName: name,
		NextProtos: []string{"http/1.1"},
		MinVersion: tls.VersionTLS12,
		RootCAs:    p.upstreamRoots,
	})
	if err := tc.Handshake(); err != nil {
		raw.Close()
		return nil, err
	}
	return tc, nil
}

// dialUpstreamH2 は元の IP (dst) へ h2 で張り直す。証明書は system のルートで
// 検証し、上流が h2 を選ばなければエラーにする。
func (p *webProxy) dialUpstreamH2(dst, sni string) (net.Conn, error) {
	raw, err := p.dial("tcp4", dst, proxyMark)
	if err != nil {
		return nil, err
	}
	tc := tls.Client(raw, &tls.Config{
		ServerName: sni,
		NextProtos: []string{"h2"},
		MinVersion: tls.VersionTLS12,
		RootCAs:    p.upstreamRoots,
	})
	if err := tc.Handshake(); err != nil {
		raw.Close()
		return nil, err
	}
	if tc.ConnectionState().NegotiatedProtocol != "h2" {
		tc.Close()
		return nil, fmt.Errorf("上流 %s は h2 を選ばない", sni)
	}
	return tc, nil
}

// filter は 1 リクエストのヘッダを header_policy どおりに絞る (ポリシーが無ければ
// 何もしない)。
func (p *webProxy) filter(req *http.Request, host string) {
	if p.headers != nil || p.hasRelaxations() {
		p.currentRules(host).Apply(req.Header)
	}
}

func (p *webProxy) hasRelaxations() bool {
	p.dynamicMu.RLock()
	defer p.dynamicMu.RUnlock()
	return len(p.dynamic) > 0
}

// tunnel は Upgrade (WebSocket など) の要求を上流へ渡し、101 が返れば以後は
// 双方向に素通しする。101 でなければ普通の応答として返して終わる。
func (p *webProxy) tunnel(client net.Conn, up net.Conn, br *bufio.Reader, upBR *bufio.Reader, req *http.Request) {
	if err := req.Write(up); err != nil {
		return
	}
	resp, err := http.ReadResponse(upBR, req)
	if err != nil {
		return
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		removeHopHeaders(resp.Header)
		_ = resp.Write(client)
		resp.Body.Close()
		return
	}
	// 101 は Go の Response.Write に任せず、ステータス行とヘッダだけをそのまま返す。
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP/1.1 %s\r\n", resp.Status)
	_ = resp.Header.Write(&b)
	b.WriteString("\r\n")
	if _, err := io.WriteString(client, b.String()); err != nil {
		return
	}
	errc := make(chan error, 2)
	go func() { _, err := io.Copy(up, br); errc <- err }()
	go func() { _, err := io.Copy(client, upBR); errc <- err }()
	<-errc
}

// isUpgrade はリクエストがプロトコルの昇格 (WebSocket など) を求めるかを返す。
func isUpgrade(req *http.Request) bool {
	return req.Header.Get("Upgrade") != "" && hasToken(req.Header, "Connection", "upgrade")
}

// hasToken はヘッダ name の値 (カンマ区切り) に token があるかを返す。
func hasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, f := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(f), token) {
				return true
			}
		}
	}
	return false
}

// removeHopHeaders は接続ごとのヘッダ (次のホップまでしか意味が無いもの) を落とす。
// Connection に並べた名前も落とす。
func removeHopHeaders(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, f := range strings.Split(v, ",") {
			if f = strings.TrimSpace(f); f != "" {
				h.Del(f)
			}
		}
	}
	for _, k := range []string{"Connection", "Proxy-Connection", "Keep-Alive",
		"Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Upgrade"} {
		h.Del(k)
	}
}

// writeHTTP1Error はクライアントへエラー応答を書き込み、接続を閉じる。
func writeHTTP1Error(w io.Writer, code int, msg string) error {
	statusText := http.StatusText(code)
	if statusText == "" {
		statusText = "Error"
	}
	body := msg + "\n"
	resp := &http.Response{
		Status:        fmt.Sprintf("%d %s", code, statusText),
		StatusCode:    code,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        make(http.Header),
		Close:         true,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
	}
	resp.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp.Header.Set("Connection", "close")
	return resp.Write(w)
}
