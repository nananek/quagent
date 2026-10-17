package hub

import (
	"encoding/json"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/nananek/quagent/internal/console"
)

var errNotConnected = errors.New("console.sock に接続していない")

// consoleBridge はセッションの console.sock へ UI クライアントとして繋ぎ、切断したら
// 繋ぎ直しながらメッセージを中継する。
type consoleBridge struct {
	sock  string
	onMsg func(console.Msg)

	mu   sync.Mutex
	enc  *json.Encoder
	conn net.Conn

	done      chan struct{}
	closeOnce sync.Once
}

// newConsoleBridge は中継を開始する。onMsg は受信ごとに呼ばれる (別 goroutine)。
func newConsoleBridge(sock string, onMsg func(console.Msg)) *consoleBridge {
	b := &consoleBridge{sock: sock, onMsg: onMsg, done: make(chan struct{})}
	go b.run()
	return b
}

func (b *consoleBridge) stopped() bool {
	select {
	case <-b.done:
		return true
	default:
		return false
	}
}

func (b *consoleBridge) run() {
	for !b.stopped() {
		c, err := net.DialTimeout("unix", b.sock, 5*time.Second)
		if err != nil {
			b.wait(2 * time.Second)
			continue
		}
		enc := json.NewEncoder(c)
		if err := enc.Encode(console.Msg{Type: "ui"}); err != nil {
			c.Close()
			b.wait(2 * time.Second)
			continue
		}
		b.set(c, enc)
		dec := json.NewDecoder(c)
		for {
			var m console.Msg
			if err := dec.Decode(&m); err != nil {
				break
			}
			if b.onMsg != nil {
				b.onMsg(m)
			}
		}
		b.set(nil, nil)
		c.Close()
		b.wait(2 * time.Second)
	}
}

func (b *consoleBridge) set(c net.Conn, enc *json.Encoder) {
	b.mu.Lock()
	b.conn, b.enc = c, enc
	b.mu.Unlock()
}

// Send はブラウザの判断を console.sock へ送る。
func (b *consoleBridge) Send(m console.Msg) error {
	b.mu.Lock()
	enc := b.enc
	b.mu.Unlock()
	if enc == nil {
		return errNotConnected
	}
	return enc.Encode(m)
}

// Close は中継を止める。
func (b *consoleBridge) Close() {
	b.closeOnce.Do(func() { close(b.done) })
	b.mu.Lock()
	if b.conn != nil {
		_ = b.conn.Close()
	}
	b.mu.Unlock()
}

func (b *consoleBridge) wait(d time.Duration) {
	select {
	case <-b.done:
	case <-time.After(d):
	}
}
