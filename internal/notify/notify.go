// Package notify はホスト側から外部へ通知を送る (Discord Webhook)。
//
// 通知は承認処理を止めてはいけないため、送信は非同期のキューで行い、失敗や
// 満杯は呼び出し側に波及させない。本文は VM 由来の文字列を含むので、送る前に
// 制御文字・Bidi を無害化する。
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nananek/quagent/internal/console"
)

const (
	requestTimeout = 5 * time.Second
	sendInterval   = time.Second
	queueSize      = 64
	maxFieldRunes  = 900
	maxRespBytes   = 4 << 10
)

// Message は 1 件の通知。
type Message struct {
	Title string // 見出し
	Body  string // 本文
	URL   string // 詳細へのリンク (Web UI の URL など)。空なら省略。
}

// Notifier は Discord Webhook へ非同期に通知する。nil は無効を表す。
type Notifier struct {
	webhook  string
	hc       *http.Client
	ch       chan Message
	interval time.Duration
	once     sync.Once
}

// New は Webhook の URL から Notifier を作る。url が空なら nil (無効) を返す。
func New(url string) *Notifier {
	if strings.TrimSpace(url) == "" {
		return nil
	}
	n := &Notifier{
		webhook:  strings.TrimSpace(url),
		hc:       &http.Client{Timeout: requestTimeout},
		ch:       make(chan Message, queueSize),
		interval: sendInterval,
	}
	go n.run()
	return n
}

// Enabled は通知が設定されているかを返す。
func (n *Notifier) Enabled() bool { return n != nil }

// Send は通知をキューに積む。満杯なら捨てる (承認処理を止めないため)。
func (n *Notifier) Send(m Message) {
	if n == nil {
		return
	}
	select {
	case n.ch <- m:
	default:
	}
}

// Close は送信ループを止める。
func (n *Notifier) Close() {
	if n == nil {
		return
	}
	n.once.Do(func() { close(n.ch) })
}

func (n *Notifier) run() {
	for m := range n.ch {
		_ = n.post(m)
		// 連投して Discord 側のレート制限に当たらないよう間隔を空ける。
		if n.interval > 0 {
			time.Sleep(n.interval)
		}
	}
}

type discordPayload struct {
	Username string         `json:"username,omitempty"`
	Embeds   []discordEmbed `json:"embeds,omitempty"`
}

type discordEmbed struct {
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	URL         string `json:"url,omitempty"`
}

func (n *Notifier) post(m Message) error {
	payload := discordPayload{
		Username: "quagent",
		Embeds: []discordEmbed{{
			Title:       truncateRunes(console.Sanitize(m.Title), maxFieldRunes),
			Description: truncateRunes(console.Sanitize(m.Body), maxFieldRunes),
			URL:         m.URL,
		}},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.webhook, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxRespBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook が %s を返した", resp.Status)
	}
	return nil
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
