package ws

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func echoServer(t *testing.T, limit int64) *httptest.Server {
	t.Helper()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(w, r)
		if err != nil {
			return
		}
		defer c.CloseNow()
		if limit > 0 {
			c.SetReadLimit(limit)
		}
		for {
			mt, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if err := c.Write(r.Context(), mt, data); err != nil {
				return
			}
		}
	})
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

type testClient struct {
	conn net.Conn
	br   *bufio.Reader
}

func dial(t *testing.T, ts *httptest.Server, origin string) (*testClient, *http.Response) {
	t.Helper()
	host := strings.TrimPrefix(ts.URL, "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	keyRaw := make([]byte, 16)
	_, _ = rand.Read(keyRaw)
	key := base64.StdEncoding.EncodeToString(keyRaw)
	var req strings.Builder
	fmt.Fprintf(&req, "GET / HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n", host, key)
	if origin != "" {
		fmt.Fprintf(&req, "Origin: %s\r\n", origin)
	}
	req.WriteString("\r\n")
	if _, err := io.WriteString(conn, req.String()); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &testClient{conn: conn, br: br}, resp
}

func (c *testClient) writeFrame(t *testing.T, op byte, fin bool, payload []byte) {
	t.Helper()
	var hdr []byte
	b0 := op
	if fin {
		b0 |= 0x80
	}
	hdr = append(hdr, b0)
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 0x80|126)
		var e [2]byte
		binary.BigEndian.PutUint16(e[:], uint16(n))
		hdr = append(hdr, e[:]...)
	default:
		hdr = append(hdr, 0x80|127)
		var e [8]byte
		binary.BigEndian.PutUint64(e[:], uint64(n))
		hdr = append(hdr, e[:]...)
	}
	var key [4]byte
	_, _ = rand.Read(key[:])
	hdr = append(hdr, key[:]...)
	masked := make([]byte, n)
	for i := range payload {
		masked[i] = payload[i] ^ key[i%4]
	}
	if _, err := c.conn.Write(hdr); err != nil {
		t.Fatal(err)
	}
	if n > 0 {
		if _, err := c.conn.Write(masked); err != nil {
			t.Fatal(err)
		}
	}
}

func (c *testClient) readFrame(t *testing.T) (byte, []byte) {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var hdr [2]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		t.Fatalf("ヘッダを読めない: %v", err)
	}
	op := hdr[0] & 0x0F
	n := int64(hdr[1] & 0x7F)
	switch n {
	case 126:
		var e [2]byte
		_, _ = io.ReadFull(c.br, e[:])
		n = int64(binary.BigEndian.Uint16(e[:]))
	case 127:
		var e [8]byte
		_, _ = io.ReadFull(c.br, e[:])
		n = int64(binary.BigEndian.Uint64(e[:]))
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		t.Fatalf("本体を読めない: %v", err)
	}
	return op, payload
}

func TestEchoTextAndBinary(t *testing.T) {
	ts := echoServer(t, 0)
	c, resp := dial(t, ts, "")
	defer c.conn.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("101 でない: %d", resp.StatusCode)
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
		t.Errorf("Upgrade ヘッダ: %q", resp.Header.Get("Upgrade"))
	}

	c.writeFrame(t, opText, true, []byte("hello"))
	if op, p := c.readFrame(t); op != opText || string(p) != "hello" {
		t.Fatalf("text エコー: op=%d p=%q", op, p)
	}

	// 16bit 長 (126..65535) と 64bit 長 (>65535) を通す。
	long := bytes.Repeat([]byte("ABCD"), 400) // 1600
	c.writeFrame(t, opBinary, true, long)
	if op, p := c.readFrame(t); op != opBinary || !bytes.Equal(p, long) {
		t.Fatalf("binary エコー: op=%d len=%d", op, len(p))
	}
	huge := bytes.Repeat([]byte("x"), 70000)
	c.writeFrame(t, opBinary, true, huge)
	if op, p := c.readFrame(t); op != opBinary || !bytes.Equal(p, huge) {
		t.Fatalf("長い binary エコー: op=%d len=%d", op, len(p))
	}
}

func TestFragmentedMessage(t *testing.T) {
	ts := echoServer(t, 0)
	c, _ := dial(t, ts, "")
	defer c.conn.Close()

	c.writeFrame(t, opText, false, []byte("he"))
	c.writeFrame(t, opContinuation, false, []byte("ll"))
	c.writeFrame(t, opContinuation, true, []byte("o"))
	if op, p := c.readFrame(t); op != opText || string(p) != "hello" {
		t.Fatalf("分割の結合: op=%d p=%q", op, p)
	}
}

func TestPingGetsPong(t *testing.T) {
	ts := echoServer(t, 0)
	c, _ := dial(t, ts, "")
	defer c.conn.Close()
	c.writeFrame(t, opPing, true, []byte("ping-data"))
	if op, p := c.readFrame(t); op != opPong || string(p) != "ping-data" {
		t.Fatalf("pong: op=%d p=%q", op, p)
	}
}

func TestOriginMismatch(t *testing.T) {
	ts := echoServer(t, 0)
	c, resp := dial(t, ts, "http://evil.example")
	defer c.conn.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("Origin 不一致は 403 のはず: %d", resp.StatusCode)
	}
}

func TestReadLimit(t *testing.T) {
	ts := echoServer(t, 4)
	c, _ := dial(t, ts, "")
	defer c.conn.Close()
	c.writeFrame(t, opText, true, []byte("hello"))
	// 上限超過でサーバーは 1009 で閉じる。
	op, p := c.readFrame(t)
	if op != opClose {
		t.Fatalf("close フレームのはず: op=%d", op)
	}
	if len(p) >= 2 && int(binary.BigEndian.Uint16(p[:2])) != 1009 {
		t.Errorf("close code: %d", binary.BigEndian.Uint16(p[:2]))
	}
}

func TestCloseHandshake(t *testing.T) {
	ts := echoServer(t, 0)
	c, _ := dial(t, ts, "")
	defer c.conn.Close()
	c.writeFrame(t, opClose, true, []byte{0x03, 0xe8}) // 1000
	if op, _ := c.readFrame(t); op != opClose {
		t.Fatalf("close 応答のはず: op=%d", op)
	}
}

func TestUnmaskedFrameRejected(t *testing.T) {
	ts := echoServer(t, 0)
	c, _ := dial(t, ts, "")
	defer c.conn.Close()
	// マスク無しのフレームを送る (プロトコル違反)。
	_, _ = c.conn.Write([]byte{0x81, 0x01, 'a'})
	// サーバーは接続を切る。読み取りは失敗するか close が来る。
	_ = c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	op, err := readOp(c.br)
	if err == nil && op != opClose {
		t.Fatalf("切断されるはず: op=%d", op)
	}
}

func readOp(br *bufio.Reader) (byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return 0, err
	}
	return hdr[0] & 0x0F, nil
}

func TestHeaderHasToken(t *testing.T) {
	if !headerHasToken("keep-alive, Upgrade", "upgrade") {
		t.Error("Upgrade を見つけられない")
	}
	if headerHasToken("keep-alive", "upgrade") {
		t.Error("誤検知")
	}
}
