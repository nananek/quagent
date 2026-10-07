// Package config はユーザー設定 (~/.config/quagent/config.json) を読む。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nananek/quagent/internal/paths"
	"github.com/nananek/quagent/internal/sandbox"
)

// Config はユーザー設定全体。
type Config struct {
	// Providers は認証プロキシ経由で guest に使わせる LLM API。キーは provider ID
	// (opencode の provider ID と揃える)。
	Providers map[string]Provider `json:"providers"`
	// Opencode は VM 内の opencode の設定。
	Opencode Opencode `json:"opencode"`
	// Claude は VM 内の Claude Code の設定。
	Claude Claude `json:"claude"`
	// Agy は VM 内の agy (Antigravity CLI) の設定。
	Agy Agy `json:"agy"`
	// PR は PR 作成の設定。
	PR PR `json:"pr"`
	// Clipboard は VM が OSC 52 で書き込もうとした中身 (承認したもの) の入れ方。
	Clipboard Clipboard `json:"clipboard"`
	// Guard はローカル LLM による HTTP リクエストの内容点検の設定。
	Guard Guard `json:"guard"`
	// Sandbox は VM の中のコマンドにかける seccomp / Landlock の設定。
	// 未指定なら既定 (compat で有効)。
	Sandbox *sandbox.Policy `json:"sandbox,omitempty"`
}

// Guard はローカル LLM による HTTP リクエストの内容点検の設定。ネットワークの
// 許可制だけでは、許可したドメインへ秘密や個人情報を持ち出す要求を見抜けないので、
// LLM 認証プロキシを通るリクエストを近くのローカル LLM に点検させる。
type Guard struct {
	// Enabled が true のときだけ点検する (既定 false)。
	Enabled bool `json:"enabled"`
	// Backend は "openai" (既定。llama.cpp の llama-server など OpenAI 互換) か
	// "ollama"。
	Backend string `json:"backend,omitempty"`
	// Endpoint はローカル LLM の URL。既定 "http://127.0.0.1:8080" (llama.cpp)。
	Endpoint string `json:"endpoint,omitempty"`
	// Model は使うモデル。既定 "qwen2.5-3b-instruct" (6GB の VRAM 向け。
	// llama.cpp は起動時の --alias と合わせる)。
	Model string `json:"model,omitempty"`
	// TimeoutSeconds は 1 リクエストの点検の上限。既定 30。
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// MaxBytes は LLM に見せる本文の塊 1 つの先頭バイト数。既定 8192、上限 32768。
	// NumCtx (文脈長) に収まらない大きさは切り下げる。
	MaxBytes int `json:"max_bytes,omitempty"`
	// MaxChunks は 1 リクエストの本文を何個の塊に分けて点検するか。既定 8、上限 64。
	// 塊は少し重ねてあり、境目にまたがる短い秘密もどれかの塊に丸ごと入る。
	MaxChunks int `json:"max_chunks,omitempty"`
	// NumCtx はローカル LLM の文脈長 (トークン)。既定 8192。llama.cpp では起動時の
	// num_ctx と合わせる (リクエストごとには変えられない)。Ollama にはこの値を渡す。
	// MaxBytes がこの文脈に収まるよう切り下げられる。
	NumCtx int `json:"num_ctx,omitempty"`
	// Concurrency は同時に点検する件数。GPU 1 枚なら 1 (既定)。llama.cpp を並列
	// (--parallel) で動かすときはその数まで上げられる。
	Concurrency int `json:"concurrency,omitempty"`
	// Mode は LLM が evidence を引用して deny したときの扱い。"ask" (既定。承認コンソールが通すか止めるか決める)、
	// "deny" (確認せず止める)、"advisory" (ログに残して通す) のどれか。
	Mode string `json:"mode,omitempty"`
	// OnError は点検できなかったときの扱い。"ask" (既定)、"deny"、"allow" のどれか。
	OnError string `json:"on_error,omitempty"`
	// InspectHTTPS は、許可した行き先への HTTPS を host 側で TLS 終端し、平文に
	// なったリクエストも同じコンテンツガードにかける。使い捨ての CA を作って guest の
	// 信頼ストアに入れるので、証明書を固定 (pinning) するクライアントとは相性が悪い。
	// Enabled が true のときだけ使える。
	InspectHTTPS bool `json:"inspect_https,omitempty"`
	// PassthroughHTTPS は TLS 終端せず素通しする行き先のパターン。証明書を固定
	// (pinning) するクライアントの行き先をここに挙げる。SNI/Host が許可名に一致する
	// ことの確認だけは続けるので、点検はできないが素通しにはならない。"example.com"
	// (完全一致) か "*.example.com" (サブドメイン)。InspectHTTPS が true のときだけ
	// 意味がある。
	PassthroughHTTPS []string `json:"passthrough_https,omitempty"`
}

// Clipboard はクリップボードへの入れ方。
type Clipboard struct {
	// Method は "tmux" (既定。tmux load-buffer -w)、"osc52" (quagent を起動した端末に
	// OSC 52 を送り直す。Kitty など)、"command" (Command に標準入力で渡す) のどれか。
	Method  string   `json:"method,omitempty"`
	Command []string `json:"command,omitempty"`
}

// Claude は VM 内の Claude Code の設定。
type Claude struct {
	// Model は既定のモデル (Claude Code の model 設定。空なら Claude Code の既定)。
	Model string `json:"model,omitempty"`
	// Theme はカラーテーマ (Claude Code の theme 設定)。空なら host の Claude Code の
	// 設定 (~/.claude/settings.json) のものを使う。
	Theme string `json:"theme,omitempty"`
	// Subscription はサブスクリプションのプラン ("pro" / "max" / "team" / "enterprise")。
	// 指定すると providers の anthropic の秘密は `claude setup-token` で作ったトークンに
	// し、プロキシが Authorization: Bearer で付ける (header は既定のまま)。空なら API キー。
	Subscription string `json:"subscription,omitempty"`
}

// PR は PR 作成の設定。
type PR struct {
	// ProtectedBranches はエージェントに push させないブランチ。
	// 未指定なら main / master / develop。
	ProtectedBranches []string `json:"protected_branches,omitempty"`
}

// Agy は VM 内の agy (Antigravity CLI) の設定。
type Agy struct {
	// Model は既定のモデル (agy の --model 設定。空なら agy の既定)。
	Model string `json:"model,omitempty"`
	// Subscription が true なら、host の agy のサブスクリプション (OAuth ログイン)
	// を認証プロキシ経由で使う。providers の gemini (Gemini API キー) は要らない。
	// false なら Gemini API キーで使う (providers に gemini が要る)。
	Subscription bool `json:"subscription,omitempty"`
}

// Opencode は VM 内の opencode の設定。
type Opencode struct {
	// Model は既定のモデル ("<provider>/<model>")。providers に挙げた provider の
	// モデルを指定する (それ以外はプロキシを通らないので使えない)。
	Model string `json:"model,omitempty"`
}

// Provider は 1 つの LLM API の転送設定。
type Provider struct {
	// Upstream は本来の API の base URL (例: https://opencode.ai/zen/go/v1)。
	Upstream string `json:"upstream"`
	// Header は秘密を載せるヘッダ名 (既定 Authorization)。
	Header string `json:"header,omitempty"`
	// Prefix はヘッダ値の前置き (Header が Authorization なら既定 "Bearer ")。
	Prefix *string `json:"prefix,omitempty"`
	// 秘密の取り出し方。どれか 1 つを指定する。
	SecretEnv     string   `json:"secret_env,omitempty"`
	SecretFile    string   `json:"secret_file,omitempty"`
	SecretCommand []string `json:"secret_command,omitempty"`
	// Allow は転送してよい操作 ("POST /messages" のようなメソッドと upstream からの
	// 相対パス。末尾の "*" は前方一致)。空なら推論とモデル一覧だけ (authproxy.DefaultAllow)。
	Allow []string `json:"allow,omitempty"`
}

// Path は設定ファイルのパス。
func Path() string { return paths.ConfigFile() }

// Load は設定を読む。ファイルが無ければ空の設定を返す。
func Load() (*Config, error) {
	b, err := os.ReadFile(Path())
	if errors.Is(err, os.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", Path(), err)
	}
	return &c, nil
}

// HeaderName は秘密を載せるヘッダ名を返す。
func (p Provider) HeaderName() string {
	if p.Header == "" {
		return "Authorization"
	}
	return p.Header
}

// HeaderPrefix はヘッダ値の前置きを返す。
func (p Provider) HeaderPrefix() string {
	if p.Prefix != nil {
		return *p.Prefix
	}
	if strings.EqualFold(p.HeaderName(), "Authorization") {
		return "Bearer "
	}
	return ""
}

// Secret は秘密を取り出す。秘密は host のプロセス内だけで使い、guest には渡さない。
func (p Provider) Secret() (string, error) {
	var raw []byte
	var err error
	switch {
	case p.SecretEnv != "":
		v, ok := os.LookupEnv(p.SecretEnv)
		if !ok {
			return "", fmt.Errorf("環境変数 %s が未設定", p.SecretEnv)
		}
		raw = []byte(v)
	case p.SecretFile != "":
		raw, err = os.ReadFile(expandHome(p.SecretFile))
	case len(p.SecretCommand) > 0:
		cmd := exec.Command(p.SecretCommand[0], p.SecretCommand[1:]...)
		cmd.Stderr = os.Stderr
		raw, err = cmd.Output()
	default:
		return "", fmt.Errorf("secret_env / secret_file / secret_command のどれかが必要")
	}
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "", fmt.Errorf("秘密が空")
	}
	return s, nil
}

func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, rest)
	}
	return p
}
