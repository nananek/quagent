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
	// AllowRequestBody が true なら、全体の DenyRequestBody が有効でもこの行き先ではボディ送信を許可する。
	AllowRequestBody bool `json:"allow_request_body,omitempty"`
	// AllowedMethods はその行き先で追加で許可する HTTP メソッド (例: ["POST", "PUT"])。
	AllowedMethods []string `json:"allowed_methods,omitempty"`
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
	// DenyRequestBody が true なら、リクエストボディの送信を拒否する (Content-Length > 0 や chunked を遮断)。
	DenyRequestBody bool `json:"deny_request_body,omitempty"`
	// AllowedMethods は許可する HTTP メソッド (例: ["GET", "HEAD"])。指定がある場合、これらに含まれないメソッドは拒否する。
	AllowedMethods []string `json:"allowed_methods,omitempty"`
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
	denyBody      bool
	methods       map[string]bool
}

// Rules は host に適用する規則を返す (行き先ごとの緩和は足し合わせる)。
func (p *Policy) Rules(host string) Rules {
	r := Rules{
		exact:     map[string]bool{},
		userAgent: p.FixedUserAgent(),
		denyBody:  p.DenyRequestBody,
		methods:   map[string]bool{},
	}
	r.add(defaultAllow)
	r.add(p.Allow)
	for _, m := range p.AllowedMethods {
		m = strings.ToUpper(strings.TrimSpace(m))
		if m != "" {
			r.methods[m] = true
		}
	}
	for pattern, hr := range p.Hosts {
		if !matchHost(pattern, host) {
			continue
		}
		r.ApplyHostRule(hr)
	}
	return r
}

// ApplyHostRule は単一の HostRule を Rules に適用 (マージ) する。
func (r *Rules) ApplyHostRule(hr HostRule) {
	r.add(hr.Allow)
	if hr.UserAgent != "" {
		r.userAgent = hr.UserAgent
	}
	if hr.KeepUserAgent {
		r.keepUserAgent = true
	}
	if hr.AllowRequestBody {
		r.denyBody = false
	}
	for _, m := range hr.AllowedMethods {
		m = strings.ToUpper(strings.TrimSpace(m))
		if m != "" {
			r.methods[m] = true
		}
	}
}

// RulesWithDynamic は静的ポリシーに動的緩和ルール (dynamicHosts) を足し合わせた解決済み規則を返す。
func (p *Policy) RulesWithDynamic(host string, dynamicHosts map[string]HostRule) Rules {
	var r Rules
	if p != nil {
		r = p.Rules(host)
	} else {
		r = (&Policy{Enabled: true}).Rules(host)
	}
	for pattern, hr := range dynamicHosts {
		if MatchHost(pattern, host) {
			r.ApplyHostRule(hr)
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

// MatchHost は "example.com" (完全一致) か "*.example.com" (サブドメイン) を照合する。
func MatchHost(pattern, host string) bool {
	pattern = strings.ToLower(pattern)
	host = strings.ToLower(host)
	if rest, ok := strings.CutPrefix(pattern, "*."); ok {
		return strings.HasSuffix(host, "."+rest)
	}
	return pattern == host
}

func matchHost(pattern, host string) bool {
	return MatchHost(pattern, host)
}

// DenyRequestBody はリクエストボディを拒否すべきかを返す。
func (r Rules) DenyRequestBody() bool {
	return r.denyBody
}

// MethodAllowed はメソッド m が許可されているかを返す。
// 許可リストが未設定 (空) の場合はすべてのメソッドを許可する。
func (r Rules) MethodAllowed(m string) bool {
	if len(r.methods) == 0 {
		return true
	}
	return r.methods[strings.ToUpper(strings.TrimSpace(m))]
}

// CheckRequest はリクエストのメソッドとボディを点検する。
// 規則に反している場合は HTTP ステータスコード (405, 400 など) とブロック理由を返す。
// 通してよければ 0, "" を返す。
func (r Rules) CheckRequest(req *http.Request) (statusCode int, reason string) {
	if req == nil {
		return 0, ""
	}
	if !r.MethodAllowed(req.Method) {
		return http.StatusMethodNotAllowed, "メソッド不許可"
	}
	if r.denyBody && HasRequestBody(req) {
		return http.StatusBadRequest, "リクエストボディ不許可"
	}
	return 0, ""
}

// HasRequestBody はリクエストに本文 (ボディ) が含まれているかを判定する。
// Content-Length > 0、chunked 転送、または未定長 (-1) の本文ストリームがある場合は true を返す。
func HasRequestBody(req *http.Request) bool {
	if req == nil {
		return false
	}
	if req.ContentLength > 0 {
		return true
	}
	if req.ContentLength < 0 {
		return true
	}
	for _, te := range req.TransferEncoding {
		if strings.EqualFold(strings.TrimSpace(te), "chunked") {
			return true
		}
	}
	if req.Header != nil {
		for _, v := range req.Header["Transfer-Encoding"] {
			if strings.Contains(strings.ToLower(v), "chunked") {
				return true
			}
		}
	}
	return false
}
