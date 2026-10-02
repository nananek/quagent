// Package guest は host と VM の間の操作経路 (vsock)。
//
// VM 内では quagent 自身が `__guest` として vsock で待ち受け、host からの
// コマンド実行 (端末つきも可) を受ける。ネットワークを通らないので nft にも
// DNS にも関わらず、ssh も要らない。
//
// 接続ごとに、最初に JSON の Header を 1 行送り、その後は枠 (種別 1 バイト +
// 長さ 4 バイト + 中身) で標準入出力・端末サイズ・終了コードをやり取りする。
package guest

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"
)

// Port は VM 内で待ち受ける vsock のポート。
const Port = 7000

// Header は接続の最初に送るコマンドの指定。
type Header struct {
	Argv []string `json:"argv"`
	Dir  string   `json:"dir,omitempty"`
	TTY  bool     `json:"tty,omitempty"`
	Term string   `json:"term,omitempty"`
	Rows uint16   `json:"rows,omitempty"`
	Cols uint16   `json:"cols,omitempty"`
}

// 枠の種別
const (
	fStdin    byte = iota // host -> guest
	fStdinEOF             // host -> guest
	fResize               // host -> guest (rows, cols)
	fStdout               // guest -> host
	fStderr               // guest -> host
	fExit                 // guest -> host (終了コード)
	fError                // guest -> host (起動できなかった等)
)

const maxFrame = 1 << 20

type frameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (fw *frameWriter) write(t byte, p []byte) error {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	var hdr [5]byte
	hdr[0] = t
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(p)))
	if _, err := fw.w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := fw.w.Write(p)
	return err
}

// writeChunks は大きなデータを枠の上限に収まるよう分けて送る。
func (fw *frameWriter) writeChunks(t byte, p []byte) error {
	for len(p) > 0 {
		n := min(len(p), maxFrame)
		if err := fw.write(t, p[:n]); err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

func readFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrame {
		return 0, nil, fmt.Errorf("枠が大きすぎる: %d", n)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return 0, nil, err
	}
	return hdr[0], p, nil
}

// frameStream は枠の種別 t でデータを送る io.Writer。
type frameStream struct {
	fw *frameWriter
	t  byte
}

func (s frameStream) Write(p []byte) (int, error) {
	if err := s.fw.writeChunks(s.t, p); err != nil {
		return 0, err
	}
	return len(p), nil
}
