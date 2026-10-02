package console

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"time"
	"unicode/utf8"

	"github.com/nananek/quagent/internal/access"
)

// VM が OSC 52 でクリップボードに書き込もうとしたときの確認。連打で承認者を
// 埋め尽くされないよう、確認待ちは 1 件まで・間隔を空ける・時間切れで拒否する。
const (
	clipInterval = 3 * time.Second
	clipTimeout  = 2 * time.Minute
	previewRunes = 400
)

type clipReq struct {
	id      int
	data    []byte
	created time.Time
	timer   *time.Timer
}

type clipState struct {
	next    int
	pending *clipReq
	last    time.Time // 前に確認を出した時刻
	dropped int       // 確認を出さずに捨てた要求の数 (次の確認かログでまとめて伝える)
	lastLog time.Time // 読み出し要求などのログを前に出した時刻
}

func preview(data []byte) string {
	if !utf8.Valid(data) {
		return fmt.Sprintf("(テキストではないデータ %d バイト)", len(data))
	}
	s := string(data)
	if utf8.RuneCountInString(s) > previewRunes {
		r := []rune(s)
		s = string(r[:previewRunes]) + "…"
	}
	return s
}

func clipMsg(c *clipReq) Msg {
	return Msg{Type: "clip", ID: c.id, Text: preview(c.data), Size: len(c.data),
		Deadline: c.created.Add(clipTimeout).Format("15:04:05")}
}

// onClipboard は __attach から届いた OSC 52 を処理する。
func (s *Server) onClipboard(m Msg) {
	if m.Text != "" { // 読み出し要求・大きすぎる・壊れている。ログは間隔を空けて出す
		s.mu.Lock()
		show := time.Since(s.clip.lastLog) >= clipInterval
		if show {
			s.clip.lastLog = time.Now()
		}
		s.mu.Unlock()
		if show {
			s.Log("クリップボード: " + m.Text)
		}
		return
	}
	if len(m.Data) > 64<<10 {
		return
	}
	s.mu.Lock()
	now := time.Now()
	if s.clip.pending != nil || now.Sub(s.clip.last) < clipInterval {
		s.clip.dropped++
		s.mu.Unlock()
		return
	}
	dropped := s.clip.dropped
	s.clip.dropped = 0
	s.clip.next++
	c := &clipReq{id: s.clip.next, data: m.Data, created: now}
	s.clip.pending, s.clip.last = c, now
	id := c.id
	c.timer = time.AfterFunc(clipTimeout, func() { s.clipSettle(id, false, "時間切れ (拒否扱い)") })
	s.mu.Unlock()
	if dropped > 0 {
		s.Log(fmt.Sprintf("クリップボード: 確認待ちの間などに来た %d 件の書き込み要求を捨てた", dropped))
	}
	s.broadcast(clipMsg(c))
}

func (s *Server) clipDecide(id int, ok bool) {
	if ok {
		s.clipSettle(id, true, "コピーした")
	} else {
		s.clipSettle(id, false, "拒否")
	}
}

// clipSettle はクリップボードの確認を決着させ、許可なら host のクリップボードに入れる。
func (s *Server) clipSettle(id int, ok bool, text string) {
	s.mu.Lock()
	c := s.clip.pending
	if c == nil || c.id != id {
		s.mu.Unlock()
		return
	}
	s.clip.pending = nil
	c.timer.Stop()
	s.mu.Unlock()
	if ok {
		sink := s.Clipboard
		if sink == nil {
			sink = TmuxClipboard
		}
		if err := sink(c.data); err != nil {
			text = "コピーに失敗: " + err.Error()
			ok = false
		}
	}
	status := access.Denied
	if ok {
		status = access.Approved
	}
	s.broadcast(Msg{Type: "clipsettled", ID: id, Status: status, Text: text})
}

// TmuxClipboard は tmux のバッファに入れ、tmux の set-clipboard の設定に従って
// 外側の端末のクリップボードにも送る。
func TmuxClipboard(data []byte) error {
	return CommandClipboard([]string{"tmux", "load-buffer", "-w", "-"})(data)
}

// CommandClipboard は中身を標準入力で渡すコマンド (wl-copy など) で入れる。
// wl-copy は裏に残ってクリップボードを提供し続けるので、出力はつながずに待つ。
func CommandClipboard(argv []string) func([]byte) error {
	return func(data []byte) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Stdin = bytes.NewReader(data)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s: %v", argv[0], err)
		}
		return nil
	}
}

// OSC52Clipboard は OSC 52 を tty (quagent を起動した端末) にそのまま送り直す。
// Kitty など OSC 52 を扱える端末向け。
func OSC52Clipboard(tty string) func([]byte) error {
	return func(data []byte) error {
		f, err := os.OpenFile(tty, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = fmt.Fprintf(f, "\x1b]52;c;%s\x07", base64.StdEncoding.EncodeToString(data))
		return err
	}
}
