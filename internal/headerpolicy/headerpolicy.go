// Package headerpolicy は VM から外へ出る HTTP リクエストのヘッダを絞る。
//
// 許可した行き先でも、ヘッダ (User-Agent に入れたメールアドレス、Referer、Cookie、
// 独自の X-* など) は持ち出しの経路になる。そこで、既知の必要なヘッダだけを残し、
// User-Agent は固定値に置き換える。行き先ごとの緩和 (許可ヘッダの追加や User-Agent
// の差し替え) は設定で与える。将来、緩和をエージェントに申請させる場合も、
// 判定が行き先名の関数 (Policy.Rules) になっているので、呼び出し側が Policy を
// 差し替えるだけで足りる。
package headerpolicy

import (
	"net/http"
	"strings"
)

// DefaultUserAgent は User-Agent の既定の固定値。
const DefaultUserAgent = "quagent"

// 既定で転送を許すヘッダ。HTTP の基本操作 (取得、ダウンロード、レジューム、
// リクエストボディの送信) に最低限要るものだけに絞る。任意の文字列を載せて情報持ち出しの
// 経路になりうる Authorization、Cache-Control、If-Match/If-None-Match、
// 拡張ヘッダ (Grpc-*)、素通しトンネル化を招く WebSocket (Upgrade / Sec-WebSocket-*)
// などは既定で落とす。これらが必要な宛先は設定 (hosts / allow) で個別に緩和する。
var defaultAllow = []string{
	"Accept", "Accept-Encoding",
	"Content-Length", "Content-Type",
	"Range",
	"If-Modified-Since",
}

// HostRule は行き先ごとの緩和。
type HostRule struct {
	// Allow はその行き先でだけ追加で転送するヘッダ名 (末尾 "*" は前方一致)。
	Allow []string `json:"allow,omitempty"`
	// UserAgent が空でなければ、その行き先での固定値を差し替える。
	UserAgent string `json:"user_agent,omitempty"`
	// KeepUserAgent が true なら User-Agent を固定せずエージェントの値を通す。
	KeepUserAgent bool `json:"keep_user_agent,omitempty"`
}

// Policy はヘッダを絞る設定。ゼロ値は無効 (何も変えない)。
type Policy struct {
	// Enabled が true のときだけ働く。
	Enabled bool `json:"enabled"`
	// UserAgent は固定する User-Agent。空なら DefaultUserAgent。
	UserAgent string `json:"user_agent,omitempty"`
	// Allow は全行き先で追加で転送するヘッダ名。
	Allow []string `json:"allow,omitempty"`
	// Passthrough は TLS を終端せず素通しする行き先 ("example.com" は完全一致、
	// "*.example.com" はサブドメイン)。証明書を固定 (pinning) するクライアント向けで、
	// SNI/Host が許可名に一致することの確認だけは続ける。素通しの宛先は暗号化されて
	// いるのでヘッダを絞れない。
	Passthrough []string `json:"passthrough_https,omitempty"`
	// Hosts は行き先ごとの緩和。キーは "example.com" (完全一致) か
	// "*.example.com" (サブドメイン)。
	Hosts map[string]HostRule `json:"hosts,omitempty"`
}

// FixedUserAgent は固定する User-Agent (行き先ごとの差し替えを除く) を返す。
func (p *Policy) FixedUserAgent() string {
	if p.UserAgent == "" {
		return DefaultUserAgent
	}
	return p.UserAgent
}

// Rules は 1 つの行き先に適用する、解決済みの規則。
type Rules struct {
	exact         map[string]bool
	prefixes      []string
	userAgent     string
	keepUserAgent bool
}

// Rules は host に適用する規則を返す (行き先ごとの緩和は足し合わせる)。
func (p *Policy) Rules(host string) Rules {
	r := Rules{exact: map[string]bool{}, userAgent: p.FixedUserAgent()}
	r.add(defaultAllow)
	r.add(p.Allow)
	for pattern, hr := range p.Hosts {
		if !matchHost(pattern, host) {
			continue
		}
		r.add(hr.Allow)
		if hr.UserAgent != "" {
			r.userAgent = hr.UserAgent
		}
		if hr.KeepUserAgent {
			r.keepUserAgent = true
		}
	}
	return r
}

func (r *Rules) add(names []string) {
	for _, n := range names {
		n = strings.TrimSpace(n)
		if prefix, ok := strings.CutSuffix(n, "*"); ok {
			r.prefixes = append(r.prefixes, http.CanonicalHeaderKey(prefix))
			continue
		}
		r.exact[http.CanonicalHeaderKey(n)] = true
	}
}

// Apply は h を規則どおりに絞る (h を直接書き換える)。許可に無いヘッダ、または
// 値が構文規則に合致しないヘッダは落とし、User-Agent は固定値にする (KeepUserAgent
// の行き先では元の値のまま)。落としたヘッダ名を返す。User-Agent の置き換えは落とした
// ものに数えない。
func (r Rules) Apply(h http.Header) (dropped []string) {
	ua := h.Values("User-Agent")
	for k, vs := range h {
		if k == "User-Agent" {
			continue
		}
		canon := http.CanonicalHeaderKey(k)
		if !r.allowed(k) || !validateHeaderValues(canon, vs) {
			dropped = append(dropped, k)
			delete(h, k)
		}
	}
	h.Del("User-Agent")
	switch {
	case r.keepUserAgent && len(ua) > 0 && validateGenericHeaderValue(ua[0]):
		h["User-Agent"] = ua
	case r.keepUserAgent:
		// エージェントが付けていないなら付けない。Go の転送は未設定だと自前の既定値
		// を補うので、空の値で抑える。
		h["User-Agent"] = []string{""}
	default:
		h.Set("User-Agent", r.userAgent)
	}
	return dropped
}

func (r Rules) allowed(name string) bool {
	name = http.CanonicalHeaderKey(name)
	if r.exact[name] {
		return true
	}
	for _, p := range r.prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// matchHost は "example.com" (完全一致) か "*.example.com" (サブドメイン) を照合する。
func matchHost(pattern, host string) bool {
	pattern = strings.ToLower(pattern)
	host = strings.ToLower(host)
	if rest, ok := strings.CutPrefix(pattern, "*."); ok {
		return strings.HasSuffix(host, "."+rest)
	}
	return pattern == host
}
