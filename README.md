# quagent

プロジェクトごとに使い捨ての QEMU VM を起動し、外向き通信を**ホスト側で**厳格に制限した環境で各種コーディングエージェントを安全に動かすためのツールです。

- **隔離の主体はホスト側:** QEMU は非特権な user/mount/net 名前空間内で動作し、許可リストに登録されていない接続先への新規アウトバウンド通信は VM の外側（nftables）ですべて遮断されます。ゲスト OS 内の root 権限からもルールを参照・改変できません。
- **ホスト・VM 間は vsock で通信:** コマンド実行、仮想端末、リポジトリ同期、PR 作成等はすべて vsock 経由で行われます。通常のネットワークを経由しないため、sshd は既定で停止されます。
- **VM 内部における多層防御:** VM 内で実行されるコマンドに対しても、seccomp / Landlock による追加の制限を適用し、プロセス間権限分離や設定ファイルの不可視化を行います。
- **機密情報を VM 内に保持しない:** LLM の API キー、コミット署名鍵、GitHub トークンなどの機密情報はホスト側にのみ保持し、ホスト側のプロキシや取り込み処理が付与・再署名します。

アーキテクチャや脅威モデルの詳細は [docs/design.md](docs/design.md)、開発やコーディング指針については [docs/development.md](docs/development.md) を参照してください。

---

## 必要な環境 (ホスト)

- **仮想化・名前空間:** `qemu-system-x86_64` (KVM), `qemu-img`, `xorriso`, `slirp4netns`, `unshare` / `nsenter` / `prlimit` (util-linux), `nft`, 非特権ユーザー名前空間, vsock (`/dev/vhost-vsock`)
- **ツール・CLI:** `git`, `gh`, `tmux`
- **イメージビルド時:** `gpg` / `gpgv` (全レシピで署名検証), OVMF (Gentoo 等の UEFI レシピ用)

---

## インストールとビルド

```sh
make build                     # bin/quagent (VM 内にも持ち込むため静的リンクでビルド)
make install                   # ~/.local/bin/quagent にインストール (PREFIX で変更可)
```

---

## 基本的な使い方

### 起動 (TUI / CLI)

リポジトリディレクトリで `quagent` を実行すると TUI が起動します:

```sh
cd <repo> && quagent           # TUI: 起動設定とベースイメージ管理
```

CLI から直接操作することも可能です:

```sh
quagent image recipes          # 利用可能なレシピ (OS) の一覧表示
quagent image build arch       # ベースイメージのビルド (--refresh / --incremental 対応)
quagent image ls / rm IMAGE    # ビルド済みイメージの一覧表示・削除
quagent run --image arch       # VM を起動 (--mount-tmp: .tmp をホスト・ゲスト間で同期)
```

- **イメージの管理:** 初回起動時など利用可能なイメージが存在しない場合は、管理画面（ベースイメージの管理）へ案内されます。CPU コア数やメモリ容量の指定、差分更新、不要イメージの一括クリーンアップ（prune）が可能です。
- **対応エージェント:** `quagent run` では `--agent` オプション（TUI でも選択可）でエージェントを指定します。
  - `opencode` (既定、`--auto` 付与)
  - `claude` (Claude Code、`--dangerously-skip-permissions` 付与)
  - `agy` (Antigravity CLI、`--dangerously-skip-permissions` 付与)
  ※ VM による強固な隔離環境下で動作するため、エージェント側でのプロンプト確認はスキップして自律実行されます。

### 画面構成と操作フロー

`quagent run` を実行すると tmux セッションが作成されます:

- **上部ペイン:** VM 内のエージェントが自律動作します。終了時は対話シェルに戻り、`↑` + Enter で即座にエージェントを再起動できます。
- **下部ペイン:** 承認コンソールが開き、接続先申請や動的緩和、PR 作成などの承認・対話を行います。

### セッション終了と再起動

エージェントペインのシェルを終了するか、承認コンソールで `quit` を入力すると tmux セッションが閉じ、ホスト側の TUI に戻ります:

- **エージェントを選び直して再起動:** VM を破棄せず、エージェントや設定のみを差し替えて即座に再起動します。
- **終了:** VM を破棄します。終了時のみ、該当 VM のホスト側ログを残すか選択できます。

---

## 各種機能と設定

### 1. LLM の設定

API キー等の認証情報は VM 内には配置しません。ホスト側の認証プロキシが正規の認証情報を付与して上流 API へリクエストを転送します。

`~/.config/quagent/config.json`:

#### opencode の設定
```json
{
  "providers": {
    "opencode-go": {
      "upstream": "https://opencode.ai/zen/go/v1",
      "secret_command": ["pass", "show", "opencode/go"]
    }
  },
  "opencode": { "model": "opencode-go/deepseek-v4.1-flash" }
}
```

#### Claude Code の設定
```json
{
  "providers": {
    "anthropic": {
      "upstream": "https://api.anthropic.com",
      "secret_command": ["pass", "show", "anthropic/claude-oauth-token"]
    }
  },
  "claude": { "subscription": "max" }
}
```
※ サブスクリプション利用時は、ホスト側で `claude setup-token` により長期トークンを作成して機密情報に指定します。

#### agy (Antigravity CLI) の設定
- **API キーで利用:**
  ```json
  {
    "providers": {
      "gemini": {
        "upstream": "https://generativelanguage.googleapis.com",
        "header": "x-goog-api-key", "prefix": "",
        "secret_command": ["pass", "show", "gemini/api-key"]
      }
    },
    "agy": { "model": "gemini-3.8-flash-high" }
  }
  ```
- **サブスクリプションで利用 (ホスト側でログイン済み):**
  ```json
  {
    "agy": { "subscription": true }
  }
  ```
  ※ ホスト側の OAuth ログイン情報から一時トークンを生成してプロキシが付与します（長期トークンは VM に渡りません）。
  ※ 長期の `refresh_token` は既定でホストのトークンファイル (`~/.gemini/antigravity-cli/antigravity-oauth-token`) から読みます。agy がトークンを secret-service (キーリング) に書く環境や、`pass` 等で別管理している場合は、以下で取り出し方を指定します (出力はトークンファイルと同じ JSON でも素の `refresh_token` でも可):
  ```json
  {
    "agy": {
      "subscription": true,
      "refresh_token_command": ["pass", "show", "secret-service/Default/password_for_antigravity_on_gemini__gl2m"]
    }
  }
  ```
  `refresh_token_env` (環境変数名)、`refresh_token_file` (ファイルパス) も使えます。secret-service を直接読む場合は `["secret-tool", "lookup", ...]` を `refresh_token_command` に指定します。

- **機密情報の取得方法:** `secret_command`（コマンド実行）、`secret_env`（環境変数）、`secret_file`（ファイルパス）から選択できます。

---

### 2. 接続先の申請と承認 (MCP)

VM からの新規アウトバウンド通信は既定ですべて遮断されています。エージェントが外部通信を必要とする場合、MCP ツール（`request_network_access`）を通じて申請を行い、ユーザーが承認コンソールで判定します。

| 入力 | 動作 |
| --- | --- |
| `1` | 今回のみ許可 (5分間、新規接続を許可) |
| `2` | このセッション中のみ常に許可 (以降の確認をスキップ) |
| `3` | 今後常に許可 (全プロジェクト共通。汎用パッケージリポジトリ等を想定) |
| `d` | 拒否 |
| `q` | 質問・指示を返す (エージェント側で理由に反映して再申請させる) |

- 10分間無応答の場合は自動拒否されます。不要になった許可は `release_network_access` で明示的に解放できます。
- 「今後常に許可」したドメインは `~/.local/share/quagent/always-allow.json` に保存され、`quagent always ls` / `quagent always rm` から確認・削除できます。
- 許可ドメインであっても、プライベート IP や内部向けアドレスへの通信、および SNI/Host ヘッダーが一致しない通信は即座に切断されます。

---

### 3. データ持ち出し防止 (Content Guard / DLP) と動的緩和

quagent は VM から外部への意図しない機密情報漏洩や不正な通信チャネルの確立を多層で防止します:

- **Content Guard / DLP (コンテンツ検査エンジン):** Web 透過プロキシを通過する HTTP リクエスト（URL、ヘッダ、ボディ）を走査し、秘密鍵（RSA/EC/OpenSSH）やクラウド・API プロバイダの認証情報（AWS, GitHub PAT, Google, Slack, OpenAI, JWT 等）の漏洩を検知して即座に HTTP 403 で遮断します（`config.json` の `"content_guard": { "enabled": false }` で無効化可能）。
- **DNS トンネリング検知:** 許可ドメイン配下であっても、不正な FQDN 長、異常なエントロピー、深層サブドメイン、TXT/ANY クエリ等のトンネリング兆候を検知して即座に遮断（REFUSED 応答）します。
- **宛先別動的緩和 (MCP):** エージェントが特定 API 呼び出し等で追加ヘッダやボディ送信を必要とする場合、MCP の `request_header_relaxation` を通じて動的緩和を申請できます。承認コンソール上でユーザーが対話的に判断し、不要になった緩和は `release_header_relaxation` で解放できます。

---

### 4. ヘッダ・本文の制限 (任意)

`header_policy` を有効にすると、VM から外部への HTTP リクエストの不要なヘッダ（Cookie, Referer, 独自ヘッダ等）を除去し、User-Agent を固定します。リクエストボディの送信遮断や HTTP メソッドの制限も可能です。

```json
"header_policy": {
  "enabled": true,
  "user_agent": "quagent",
  "deny_request_body": true,
  "allowed_methods": ["GET", "HEAD"],
  "hosts": {
    "*.example.com": { "allow": ["X-Example-*"] },
    "registry.example.org": { "allow_request_body": true, "allowed_methods": ["POST", "PUT"] }
  }
}
```

---

### 5. ツールサーバー (OpenAPI)

OpenAPI 仕様で公開された外部サーバー（Open WebUI のツールサーバー等）を、VM 内のエージェントから MCP ツールとして利用できます。ホスト側が仕様を取得して MCP ツールに自動変換し、呼び出しをプロキシします。

```json
"tool_servers": {
  "my-tools": {
    "url": "http://192.168.1.10:8000",
    "secret_env": "MY_TOOLS_KEY"
  },
  "public-tools": {
    "url": "https://tools.example.com",
    "openapi_path": "/weather/openapi.json"
  }
}
```

---

### 6. クリップボード (OSC 52)

VM 内のエージェントが端末経由でクリップボードへ書き込もうとすると (OSC 52)、ホスト側で捕捉して承認コンソール上で確認を求めます（`[y] コピー / [n] 拒否`）。クリップボードの読み出し要求は常に拒否されます。

反映方法は `config.json` の `clipboard` で設定します:
- `"clipboard": { "method": "tmux" }` (既定)
- `"clipboard": { "method": "osc52" }` (端末へ OSC 52 を再送)
- `"clipboard": { "method": "command", "command": ["wl-copy"] }`

---

### 7. PR の作成と署名

エージェントは `/work` の保護されていない作業ブランチでコミットを作成し、MCP の `create_pull_request` ツールを通じて PR 作成を依頼します。

- **安全なコミット再署名:** VM 内では一時的な使い捨て鍵でコミットが作成され、ホスト側がこれを取り込んで正規の鍵で再署名した上で push します（GitHub トークンや秘密鍵は VM に渡りません）。
- **PR 作成の承認:** 既定で承認制（`--pr-approval`）となっており、push 前に承認コンソール上でブランチ名・マージ先・タイトル・本文の確認を求めます。

---

## 関連ドキュメントとスキル

- [docs/design.md](docs/design.md) — 内部アーキテクチャ、隔離の仕組み、多層防御一覧表、脅威モデル、詳細仕様
- [docs/development.md](docs/development.md) — 開発ガイドライン、ビルド・テスト手順、コーディング指針、PR 規約
- [.opencode/skills/recipe-authoring/](.opencode/skills/recipe-authoring/SKILL.md) — ベースイメージレシピの作成・修正規約
- [.opencode/skills/document-authoring/](.opencode/skills/document-authoring/SKILL.md) — ドキュメント構成・作成規約

---

## ライセンス

MIT
