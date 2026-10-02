package netns

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"
)

// Grant は 1 つの接続許可。Pattern は "example.com" (完全一致) か
// "*.example.com" (サブドメイン)。Expires は unix 秒で、0 は期限なし。
type Grant struct {
	Pattern string `json:"pattern"`
	Expires int64  `json:"expires,omitempty"`
}

func (g Grant) active(now time.Time) bool {
	return g.Expires == 0 || now.Unix() < g.Expires
}

// 親 -> ランチャ (stdin、1 行 1 JSON)
type control struct {
	Seq    int     `json:"seq"`
	Grants []Grant `json:"grants"`
}

// Event はランチャ -> 親 (stdout、1 行 1 JSON)。
type Event struct {
	// Applied は control の Seq。その許可が nft に反映済みであることを示す。
	Applied int `json:"applied,omitempty"`
	// Denied は許可外として名前解決を拒否したドメイン。
	Denied string `json:"denied,omitempty"`
}

// egress は許可の状態と nft の allow set を同期させる。
//
// 許可されたドメインの DNS 応答で見た IP だけを allow set に入れる (内部向けの IP は
// 除く。internalIP を参照)。CNAME の先の名前は許可しない (応答に含まれる A レコードは
// 問い合わせた名前の分として扱うので、先の名前を別に引く必要はない)。set は
// 許可・応答・期限切れのたびに作り直す。許可は「新規接続を始めてよいか」の
// 判断なので、set から外れても確立済みの接続は切れない (ct established)。
type egress struct {
	nft    func(script string) error
	events *eventWriter

	mu      sync.Mutex
	grants  []Grant
	seen    map[string]map[string]bool // ドメイン -> 応答で見た IP
	applied string

	// 拒否のログ・通知の量を抑える (ランダムな名前の連打でログを膨らませない)
	logWindow  time.Time
	logCount   int
	logDropped int
	seenFull   bool
}

const (
	// maxSeen は応答を覚えておく名前の上限 (ワイルドカード許可で無限に増やされないように)。
	maxSeen = 4096
	// maxDeniedLogs は 1 分あたりに記録・通知する拒否の件数。
	maxDeniedLogs = 30
)

func newEgress(nft func(string) error, events *eventWriter, initial []Grant) *egress {
	return &egress{nft: nft, events: events, grants: initial,
		seen: map[string]map[string]bool{}}
}

// internalNets は通常の外向き接続には要らない宛先 (LAN・loopback・link-local
// (クラウドのメタデータを含む)・CGNAT (Tailscale を含む)・予約・マルチキャスト)。
var internalNets = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// internalIP は ip が internalNets に入るかを返す。許可したドメインの応答でも
// これらは allow set に入れない (応答を操れる者が DNS だけで内側へ届かせないように)。
func internalIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	a = a.Unmap()
	for _, p := range internalNets {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// matchLocked は name が有効な許可に当たるかを返す。
func (e *egress) matchLocked(name string, now time.Time) bool {
	for _, g := range e.grants {
		if g.active(now) && Matches(g.Pattern, name) {
			return true
		}
	}
	return false
}

func (e *egress) allowed(name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.matchLocked(name, time.Now())
}

func (e *egress) denied(name string) {
	e.mu.Lock()
	now := time.Now()
	if now.Sub(e.logWindow) >= time.Minute {
		if e.logDropped > 0 {
			log.Printf("dns: 許可外 ほか %d 件 (多すぎるので省略)", e.logDropped)
		}
		e.logWindow, e.logCount, e.logDropped = now, 0, 0
	}
	e.logCount++
	if e.logCount > maxDeniedLogs {
		e.logDropped++
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()
	log.Printf("dns: 許可外 %s", name)
	e.events.send(Event{Denied: name})
}

// onAnswer は DNS 応答を guest に返す前に呼ばれ、IP を set に反映し終えてから戻る。
func (e *egress) onAnswer(name string, ips []net.IP) {
	var blocked []string
	defer func() {
		for _, b := range blocked {
			e.denied(b)
		}
	}()
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.seen[name]
	if m == nil {
		if len(e.seen) >= maxSeen {
			if !e.seenFull {
				log.Printf("dns: 応答を覚える名前が上限 (%d) に達した。以後の新しい名前には接続できない", maxSeen)
				e.seenFull = true
			}
			return
		}
		m = map[string]bool{}
		e.seen[name] = m
	}
	for _, ip := range ips {
		if internalIP(ip) {
			log.Printf("dns: %s の応答の内部向け IP %s は通さない", name, ip)
			blocked = append(blocked, fmt.Sprintf("%s -> %s (内部向けの IP は通さない)", name, ip))
			continue
		}
		m[ip.String()] = true
	}
	e.syncLocked()
}

func (e *egress) setGrants(gs []Grant) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.grants = gs
	e.syncLocked()
}

// prune は期限切れの許可を set から外す。
func (e *egress) prune() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.syncLocked()
}

func (e *egress) syncLocked() {
	now := time.Now()
	want := map[string]bool{}
	for name, ips := range e.seen {
		if !e.matchLocked(name, now) {
			continue
		}
		for ip := range ips {
			want[ip] = true
		}
	}
	list := make([]string, 0, len(want))
	for ip := range want {
		list = append(list, ip)
	}
	sort.Strings(list)
	key := strings.Join(list, ",")
	if key == e.applied {
		return
	}
	script := "flush set inet quagent allow4\n"
	if len(list) > 0 {
		script += "add element inet quagent allow4 { " + strings.Join(list, ", ") + " }\n"
	}
	if err := e.nft(script); err != nil {
		log.Printf("allow set の更新に失敗: %v", err)
		return
	}
	e.applied = key
	log.Printf("allow set: %d IP", len(list))
}

type eventWriter struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func (w *eventWriter) send(ev Event) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.enc.Encode(ev)
}

func readControl(r io.Reader, fn func(control)) error {
	dec := json.NewDecoder(r)
	for {
		var c control
		if err := dec.Decode(&c); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("control の読み取りに失敗: %w", err)
		}
		fn(c)
	}
}
