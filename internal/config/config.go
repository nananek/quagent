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

	"github.com/nananek/quagent/internal/headerpolicy"
	"github.com/nananek/quagent/internal/paths"
	"github.com/nananek/quagent/internal/sandbox"
)

// Config はユーザー設定全体。
type Config struct {
	// Providers は認証プロキシ経由で guest に使わせる LLM API。キーは provider ID
	// (opencode の provider ID と揃える)。
	Providers map[string]Provider `json:"providers"`
	// ToolServers は OpenAPI で公開された外部のツールサーバー (Open WebUI のツール
	// サーバーなど)。host が OpenAPI 仕様を読んで MCP のツールに変換し、guest の
	// エージェントに使わせる。キーはツール名の前置きになる名前。
	ToolServers map[string]ToolServer `json:"tool_servers,omitempty"`
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
	// HeaderPolicy は VM から外へ出るリクエストのヘッダや本文を絞り、User-Agent を固定する
	// 設定 (LLM 認証プロキシは対象外。プロバイダごとに必要なヘッダがあるため)。
	// 有効にすると HTTPS を終端して平文に戻す (使い捨て CA を使う)。未指定なら何もしない。
	HeaderPolicy *headerpolicy.Policy `json:"header_policy,omitempty"`
	// ContentGuard は秘密情報・認証情報の漏洩防止 (DLP) 検査の設定。
	// 未指定なら既定で有効。
	ContentGuard *ContentGuardPolicy `json:"content_guard,omitempty"`
	// Sandbox は VM の中のコマンドにかける seccomp / Landlock の設定。
	// 未指定なら既定 (compat で有効)。
	Sandbox *sandbox.Policy `json:"sandbox,omitempty"`
	// QemuSandbox はホスト側 QEMU プロセスにかけるサンドボックスの設定。
	// 未指定なら既定で有効。
	QemuSandbox *QemuSandboxPolicy `json:"qemu_sandbox,omitempty"`
	// Resources はホスト側の計算資源 (ストレージ、CPU、メモリ、I/O) の保護設定。
	Resources *ResourcePolicy `json:"resources,omitempty"`
}

// ContentGuardPolicy は秘密情報・認証情報の漏洩防止 (DLP) 検査の設定。
type ContentGuardPolicy struct {
	// Enabled を false にすると Content Guard / DLP 検査を無効化する (既定: true)。
	Enabled *bool `json:"enabled,omitempty"`
}

// IsEnabled は Content Guard が有効かどうかを返す (既定: true)。
func (p *ContentGuardPolicy) IsEnabled() bool {
	if p == nil || p.Enabled == nil {
		return true
	}
	return *p.Enabled
}

// QemuSandboxPolicy はホスト側 QEMU プロセスのサンドボックス設定。
type QemuSandboxPolicy struct {
	// Enabled を false にするとホスト側 QEMU サンドボックスを無効化する (既定: true)。
	Enabled *bool `json:"enabled,omitempty"`
}

// ResourcePolicy はホスト側の計算資源 (ストレージ、CPU、メモリ、I/O) の保護設定。
type ResourcePolicy struct {
	// Enabled を false にするとホスト側リソース保護を無効化する (既定: true)。
	Enabled *bool `json:"enabled,omitempty"`
	// MinFreeDiskGiB は VM 起動に必要なホスト作業ディレクトリの最小空き容量 (GiB, 既定: 10)。
	// これを下回る場合は起動を拒否する。
	MinFreeDiskGiB int `json:"min_free_disk_gib,omitempty"`
	// HostSafetyFreeGiB は実行中のホスト安全下限空き容量 (GiB, 既定: 2)。
	// 実行中にホストの空き容量がこれを下回った場合は即時安全停止する。
	HostSafetyFreeGiB int `json:"host_safety_free_gib,omitempty"`
	// DiskWarnPercent はゲスト仮想ディスク (40 GiB) に対する警告使用率 (既定: 80%)。
	DiskWarnPercent int `json:"disk_warn_percent,omitempty"`
	// DiskStopPercent はゲスト仮想ディスクに対する安全停止使用率 (既定: 95%)。
	DiskStopPercent int `json:"disk_stop_percent,omitempty"`
	// CPUQuotaPercent はホスト側 CPU 占有上限率 (例: 200 = 2コア分, 0 = 無制限/自動, 既定: 0)。
	CPUQuotaPercent int `json:"cpu_quota_percent,omitempty"`
	// MemoryOverheadMiB は QEMU のメモリ割当 (-m) に対するホスト側許容マージン (MiB, 既定: 512)。
	MemoryOverheadMiB int `json:"memory_overhead_mib,omitempty"`
	// IOReadBPS はディスク読込上限 (バイト/秒, 0 = 無制限, 既定: 0)。
	IOReadBPS int64 `json:"io_read_bps,omitempty"`
	// IOWriteBPS はディスク書込上限 (バイト/秒, 0 = 無制限, 既定: 0)。
	IOWriteBPS int64 `json:"io_write_bps,omitempty"`
	// Nice は QEMU プロセスのスケジューリング優先度 (既定: 10)。
	Nice int `json:"nice,omitempty"`
	// OOMScoreAdj は OOM 発生時の優先身代わりスコア (既定: 500)。
	OOMScoreAdj int `json:"oom_score_adj,omitempty"`
	// MaxLogSizeMiB は console.log の最大保持サイズ (MiB, 既定: 50)。
	MaxLogSizeMiB int `json:"max_log_size_mib,omitempty"`
}

// DefaultResourcePolicy は既定のリソース保護設定を返す。
func DefaultResourcePolicy() ResourcePolicy {
	tr := true
	return ResourcePolicy{
		Enabled:           &tr,
		MinFreeDiskGiB:    10,
		HostSafetyFreeGiB: 2,
		DiskWarnPercent:   80,
		DiskStopPercent:   95,
		CPUQuotaPercent:   0,
		MemoryOverheadMiB: 512,
		Nice:              10,
		OOMScoreAdj:       500,
		MaxLogSizeMiB:     50,
	}
}

// ResourcePolicyOrDefault は設定が存在すれば既定値を補完して返し、
// 未設定ならデフォルトを返す。
func (c *Config) ResourcePolicyOrDefault() ResourcePolicy {
	def := DefaultResourcePolicy()
	if c == nil || c.Resources == nil {
		return def
	}
	res := *c.Resources
	if res.Enabled == nil {
		res.Enabled = def.Enabled
	}
	if res.MinFreeDiskGiB <= 0 {
		res.MinFreeDiskGiB = def.MinFreeDiskGiB
	}
	if res.HostSafetyFreeGiB <= 0 {
		res.HostSafetyFreeGiB = def.HostSafetyFreeGiB
	}
	if res.DiskWarnPercent <= 0 {
		res.DiskWarnPercent = def.DiskWarnPercent
	}
	if res.DiskStopPercent <= 0 {
		res.DiskStopPercent = def.DiskStopPercent
	}
	if res.MemoryOverheadMiB <= 0 {
		res.MemoryOverheadMiB = def.MemoryOverheadMiB
	}
	if res.Nice <= 0 {
		res.Nice = def.Nice
	}
	if res.OOMScoreAdj <= 0 {
		res.OOMScoreAdj = def.OOMScoreAdj
	}
	if res.MaxLogSizeMiB <= 0 {
		res.MaxLogSizeMiB = def.MaxLogSizeMiB
	}
	return res
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
	// ColorScheme はカラースキーム (agy の colorScheme 設定)。空なら host の agy の
	// 設定 (~/.gemini/antigravity-cli/settings.json) のものを使う。
	ColorScheme string `json:"color_scheme,omitempty"`
	// Subscription が true なら、host の agy のサブスクリプション (OAuth ログイン)
	// を認証プロキシ経由で使う。providers の gemini (Gemini API キー) は要らない。
	// false なら Gemini API キーで使う (providers に gemini が要る)。
	Subscription bool `json:"subscription,omitempty"`
	// Seed はサブスクリプション用に host が run 開始時に作った短命トークン。
	// guest のトークンファイルと窓口の追加の合言葉に使う。設定ファイルには書かない。
	Seed string `json:"-"`
	// Egress はサブスクリプション用に一時的に開ける egress の宛先
	// (ユーザー情報確認とプロフィール画像)。初回の推論が通ったら閉じる。
	Egress []string `json:"-"`
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

// ToolServer は OpenAPI で公開された 1 つの外部ツールサーバーの設定。
type ToolServer struct {
	// URL はサーバーの base URL (例: http://192.168.1.10:8000)。host から到達できれば
	// よい (LAN 内でも可。guest が直接つなぐわけではない)。http / https のどちらも使える。
	URL string `json:"url"`
	// OpenAPIPath は OpenAPI 仕様 (JSON) の URL 内のパス。既定 "/openapi.json"。
	// Open WebUI のツールサーバーでは "/<ツール名>/openapi.json" のようになる。
	OpenAPIPath string `json:"openapi_path,omitempty"`
	// Header は秘密を載せるヘッダ名 (既定 Authorization)。
	Header string `json:"header,omitempty"`
	// Prefix はヘッダ値の前置き (Header が Authorization なら既定 "Bearer ")。
	Prefix *string `json:"prefix,omitempty"`
	// 秘密の取り出し方。認証が要らないサーバーでは 3 つとも省略する (認証なし)。
	SecretEnv     string   `json:"secret_env,omitempty"`
	SecretFile    string   `json:"secret_file,omitempty"`
	SecretCommand []string `json:"secret_command,omitempty"`
}

// provider は秘密まわりの設定を Provider として扱う (取り出し方を共通にするため)。
func (t ToolServer) provider() Provider {
	return Provider{Upstream: t.URL, Header: t.Header, Prefix: t.Prefix,
		SecretEnv: t.SecretEnv, SecretFile: t.SecretFile, SecretCommand: t.SecretCommand}
}

// NeedsAuth は認証ヘッダを付けるかどうか (秘密の取り出し方が 1 つでもあれば true)。
func (t ToolServer) NeedsAuth() bool {
	return t.SecretEnv != "" || t.SecretFile != "" || len(t.SecretCommand) > 0
}

// HeaderName は秘密を載せるヘッダ名を返す。
func (t ToolServer) HeaderName() string { return t.provider().HeaderName() }

// AuthValue は認証ヘッダの値 (前置き込み) を返す。認証なしなら空文字列。
// 秘密は host のプロセス内だけで使い、guest には渡さない。
func (t ToolServer) AuthValue() (string, error) {
	if !t.NeedsAuth() {
		return "", nil
	}
	s, err := t.provider().Secret()
	if err != nil {
		return "", err
	}
	return t.provider().HeaderPrefix() + s, nil
}

func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, rest)
	}
	return p
}
