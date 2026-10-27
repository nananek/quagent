package console

import (
	"encoding/json"
	"errors"
	"net"
	"sync"
	"time"
)

// Conn は console.sock へ UI として繋いだクライアント。quagent ctl のような
// 外部プロセスが承認の流れを読んだり、判断を 1 つだけ送ったりするのに使う。
//
// 再接続は持たない (TUI と要件が違う)。呼び出し側が Close して Dial し直す。
type Conn struct {
	c   net.Conn
	enc *json.Encoder
	dec *json.Decoder

	events chan Msg
	done   chan struct{}

	sendMu    sync.Mutex
	closeOnce sync.Once
}

// ErrConnClosed は閉じた Conn に送ろうとしたときのエラー。
var ErrConnClosed = errors.New("console: 接続は閉じている")

// Dial は sock に接続して UI として名乗る ({"type":"ui"})。
// 本体は名乗りを受けると保留中の申請をすべて送り返すので、後から繋いでも
// 取りこぼさない。
func Dial(sock string) (*Conn, error) {
	c, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return nil, err
	}
	enc := json.NewEncoder(c)
	_ = c.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := enc.Encode(Msg{Type: "ui"}); err != nil {
		_ = c.Close()
		return nil, err
	}
	_ = c.SetWriteDeadline(time.Time{})

	conn := &Conn{
		c:      c,
		enc:    enc,
		dec:    json.NewDecoder(c),
		events: make(chan Msg, 64),
		done:   make(chan struct{}),
	}
	go conn.read()
	return conn, nil
}

// Events は受信したメッセージを流す。接続が切れると閉じる。
// バッファ (64) が溢れたら読み手が取るまでブロックする (承認を落とさない)。
func (c *Conn) Events() <-chan Msg { return c.events }

// Send は判断を 1 つ送る (write deadline 2 秒)。
func (c *Conn) Send(m Msg) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	select {
	case <-c.done:
		return ErrConnClosed
	default:
	}
	_ = c.c.SetWriteDeadline(time.Now().Add(writeTimeout))
	return c.enc.Encode(m)
}

// Close は接続を閉じる。Events の channel は受信ループが閉じる。
func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.done)
		err = c.c.Close()
	})
	return err
}

func (c *Conn) read() {
	defer close(c.events)
	for {
		var m Msg
		if err := c.dec.Decode(&m); err != nil {
			return
		}
		select {
		case c.events <- m:
		case <-c.done:
			return
		}
	}
}
