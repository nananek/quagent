// Package ws は RFC 6455 の WebSocket をサーバー側だけ最小実装する。
//
// quagent は外部依存を最小限に保ちたいため、hub が必要とする範囲 (HTTP からの
// 昇格、テキスト・バイナリの送受信、ping/pong、close、分割フレーム、マスク解除、
// 読み込み上限) だけを実装する。クライアント側や拡張 (permessage-deflate 等) は
// 扱わない。
package ws

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// MessageType はデータフレームの種類。
type MessageType int

const (
	MessageText   MessageType = 1
	MessageBinary MessageType = 2
)

// フレームのオペコード。
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// クローズコード (よく使うもの)。
const (
	StatusNormalClosure   = 1000
	StatusPolicyViolation = 1008
	StatusInternalError   = 1011
)

// acceptGUID は RFC 6455 の固定文字列。
const acceptGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	defaultReadLimit = 1 << 20
	handshakeTimeout = 10 * time.Second
)

// ErrClosed は接続が閉じているときのエラー。
var ErrClosed = errors.New("websocket: 接続が閉じている")

// Conn は昇格後の WebSocket 接続。
type Conn struct {
	conn net.Conn
	br   *bufio.Reader

	writeMu sync.Mutex

	readLimit int64

	closeOnce sync.Once
	done      chan struct{}
}

// Accept は HTTP リクエストを WebSocket に昇格する。Origin があれば同一ホストで
// あることを確認する (cross-site WebSocket hijacking 対策)。
func Accept(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if r.Method != http.MethodGet {
		http.Error(w, "websocket: GET が必要", http.StatusMethodNotAllowed)
		return nil, errors.New("websocket: メソッドが不正")
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		!headerHasToken(r.Header.Get("Connection"), "upgrade") {
		http.Error(w, "websocket: Upgrade が必要", http.StatusBadRequest)
		return nil, errors.New("websocket: Upgrade ヘッダが無い")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "websocket: version 13 が必要", http.StatusBadRequest)
		return nil, errors.New("websocket: version が不正")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "websocket: key が無い", http.StatusBadRequest)
		return nil, errors.New("websocket: Sec-WebSocket-Key が無い")
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || !strings.EqualFold(hostOf(u.Host), hostOf(r.Host)) {
			http.Error(w, "websocket: origin が不正", http.StatusForbidden)
			return nil, errors.New("websocket: Origin が一致しない")
		}
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "websocket: 昇格できない", http.StatusInternalServerError)
		return nil, errors.New("websocket: Hijacker が無い")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))

	h := sha1.Sum([]byte(key + acceptGUID))
	accept := base64.StdEncoding.EncodeToString(h[:])
	var b strings.Builder
	b.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	b.WriteString("Upgrade: websocket\r\n")
	b.WriteString("Connection: Upgrade\r\n")
	b.WriteString("Sec-WebSocket-Accept: " + accept + "\r\n\r\n")
	if _, err := io.WriteString(conn, b.String()); err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})

	return &Conn{
		conn:      conn,
		br:        rw.Reader,
		readLimit: defaultReadLimit,
		done:      make(chan struct{}),
	}, nil
}

// SetReadLimit は 1 メッセージの最大バイト数を設定する。
func (c *Conn) SetReadLimit(n int64) {
	if n > 0 {
		c.readLimit = n
	}
}

// Read は次のデータメッセージを読む。制御フレーム (ping/pong/close) は内部で処理する。
// ctx が終われば読み取りを打ち切る。
func (c *Conn) Read(ctx context.Context) (MessageType, []byte, error) {
	// ctx の終了でブロック中の読み取りを解く。
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = c.conn.SetReadDeadline(time.Now())
		case <-stop:
		case <-c.done:
		}
	}()

	var (
		msgType MessageType
		msg     []byte
		frag    bool
	)
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			if ctx.Err() != nil {
				return 0, nil, ctx.Err()
			}
			return 0, nil, err
		}
		switch op {
		case opPing:
			if err := c.writeControl(opPong, payload); err != nil {
				return 0, nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			_ = c.writeControl(opClose, nil)
			c.CloseNow()
			return 0, nil, ErrClosed
		case opText, opBinary:
			if frag {
				c.fail(1002)
				return 0, nil, errors.New("websocket: 分割中に新しいデータフレーム")
			}
			msgType = MessageType(op)
			if fin {
				return msgType, payload, nil
			}
			frag, msg = true, payload
		case opContinuation:
			if !frag {
				c.fail(1002)
				return 0, nil, errors.New("websocket: 継続フレームが先行している")
			}
			msg = append(msg, payload...)
			if int64(len(msg)) > c.readLimit {
				c.fail(1009)
				return 0, nil, errors.New("websocket: メッセージが大きすぎる")
			}
			if fin {
				return msgType, msg, nil
			}
		default:
			c.fail(1002)
			return 0, nil, fmt.Errorf("websocket: 未知のオペコード %d", op)
		}
	}
}

// Write はメッセージを 1 フレームで送る。
func (c *Conn) Write(ctx context.Context, t MessageType, data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	select {
	case <-c.done:
		return ErrClosed
	default:
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = c.conn.SetWriteDeadline(dl)
	} else {
		_ = c.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	}
	defer c.conn.SetWriteDeadline(time.Time{})
	return c.writeFrame(opcodeOf(t), true, data)
}

// Close は close フレームを送ってから接続を閉じる。
func (c *Conn) Close(code int, reason string) error {
	var payload []byte
	if code != 0 {
		payload = make([]byte, 2+len(reason))
		binary.BigEndian.PutUint16(payload, uint16(code))
		copy(payload[2:], reason)
	}
	c.writeMu.Lock()
	err := c.writeFrame(opClose, true, payload)
	c.writeMu.Unlock()
	c.CloseNow()
	return err
}

// CloseNow は close フレームを送らずに接続を閉じる。
func (c *Conn) CloseNow() {
	c.closeOnce.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}

func (c *Conn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		return false, 0, nil, err
	}
	fin = hdr[0]&0x80 != 0
	if hdr[0]&0x70 != 0 {
		return false, 0, nil, errors.New("websocket: RSV ビットが立っている")
	}
	op = hdr[0] & 0x0F

	masked := hdr[1]&0x80 != 0
	length := int64(hdr[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(ext[:]))
		if length < 0 {
			return false, 0, nil, errors.New("websocket: 長さが不正")
		}
	}
	if isControl(op) {
		if !fin || length > 125 {
			return false, 0, nil, errors.New("websocket: 制御フレームが不正")
		}
	} else if length > c.readLimit {
		c.fail(1009)
		return false, 0, nil, errors.New("websocket: フレームが大きすぎる")
	}

	// クライアントからのフレームはマスクが必須 (RFC 6455)。
	if !masked {
		return false, 0, nil, errors.New("websocket: マスクされていないフレーム")
	}
	var key [4]byte
	if _, err := io.ReadFull(c.br, key[:]); err != nil {
		return false, 0, nil, err
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return false, 0, nil, err
	}
	for i := range payload {
		payload[i] ^= key[i%4]
	}
	return fin, op, payload, nil
}

func (c *Conn) writeFrame(op byte, fin bool, payload []byte) error {
	var hdr [10]byte
	hdr[0] = op
	if fin {
		hdr[0] |= 0x80
	}
	n := 2
	switch {
	case len(payload) < 126:
		hdr[1] = byte(len(payload))
	case len(payload) <= 0xFFFF:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(len(payload)))
		n = 4
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(len(payload)))
		n = 10
	}
	if _, err := c.conn.Write(hdr[:n]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := c.conn.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

func (c *Conn) writeControl(op byte, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.writeFrame(op, true, payload)
}

// fail は指定のクローズコードで閉じる。
func (c *Conn) fail(code int) {
	c.writeMu.Lock()
	_ = c.writeFrame(opClose, true, []byte{byte(code >> 8), byte(code)})
	c.writeMu.Unlock()
	c.CloseNow()
}

func opcodeOf(t MessageType) byte {
	if t == MessageText {
		return opText
	}
	return opBinary
}

func isControl(op byte) bool { return op >= 0x8 }

func headerHasToken(v, token string) bool {
	for _, part := range strings.Split(v, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

func hostOf(h string) string {
	if h == "" {
		return ""
	}
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.Contains(h[i:], "]") {
		return h[:i]
	}
	return h
}
