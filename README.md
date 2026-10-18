# quagent

quagent は、外向き通信をホスト側で厳格に遮断した使い捨て QEMU VM 上で、コーディングエージェント（opencode、Claude Code、agy、Codex）を自律かつ安全に動かすための実行環境です。

> **このガイドの読み方:**
> - **対象読者:** Linux 環境で各種コーディングエージェントを安全に動かしたい開発者（Linux 基本コマンドと tmux の操作を前提とします）。
> - **通読部 (「主な特徴」〜「基本操作」):** 初回セットアップからエージェントの起動・終了までは順にお読みください。
> - **参照部 (「各種機能と設定」以降):** LLM 設定、外部通信申請、DLP などの各機能は、必要に応じて参照してください。
> - **疑問や不具合:** 不明な点や改善要望は [GitHub Issues](https://github.com/) へお寄せください。

---

## 主な特徴: ホスト主体の完全隔離と安全な自律実行

- **ホスト側での厳格な通信遮断:** 未許可のアウトバウンド通信は VM の外側ですべて遮断されます。ゲスト OS 内の root 権限からもルールを参照・改変できません。
- **機密情報を VM に渡さない:** API キーや Git 署名鍵はホスト側のプロキシが保持し、VM 内には一時認証トークンのみを渡します。
- **完全使い捨ての VM 環境:** セッションごとにスナップショットから起動し、終了時に破棄されます。
- **安全な PR 作成:** VM 内で作成されたコミットをホスト側が検証し、正規の鍵で再署名して GitHub へ push します。

アーキテクチャや脅威モデルの詳細は [docs/design.md](docs/design.md)、開発やコーディング指針については [docs/development.md](docs/development.md) を参照してください。

---

## 動作環境: Linux ホスト上に KVM・非特権名前空間・vsock が必要

- **仮想化・名前空間:** `qemu-system-x86_64` (KVM), `qemu-img`, `xorriso`, `slirp4netns`, `unshare` / `nsenter` / `prlimit` (util-linux), `nft`, 非特権ユーザー名前空間, vsock (`/dev/vhost-vsock`)
- **ツール・CLI:** `git`, `gh`, `tmux`
- **イメージビルド時:** `gpg` / `gpgv` (全レシピで署名検証), OVMF (Gentoo 等の UEFI レシピ用)

---

## インストール: make build で静的バイナリを作成して配置する

```sh
make build                     # bin/quagent (VM 内にも持ち込むため CGO_ENABLED=0 の静的リンクでビルド)
make install                   # ~/.local/bin/quagent にインストール (PREFIX で変更可)
```

---

## 基本操作: リポジトリ直下で quagent を実行し TUI から起動する

### 起動 (TUI / CLI)

対象リポジトリのディレクトリで `quagent` を実行すると TUI が起動します。

```sh
cd <repo> && quagent           # TUI: 起動設定とベースイメージ管理
```

CLI から直接操作することも可能です。

```sh
quagent image recipes          # 利用可能なレシピ (OS) の一覧表示
quagent image build arch       # ベースイメージのビルド (--refresh / --incremental 対応)
quagent image ls / rm IMAGE    # ビルド済みイメージの一覧表示・削除
quagent cache ls / clean       # キャッシュの一覧表示・全削除
quagent run --image arch       # VM を起動 (--mount-tmp: .tmp をホスト・ゲスト間で同期)
quagent hub                    # Web 遠隔操作モードを起動 (Tailscale 越しにブラウザから操作)

```

- **イメージの管理:** 初回起動時など利用可能なイメージが存在しない場合は、管理画面（ベースイメージの管理）へ案内されます。CPU コア数やメモリ容量の指定、差分更新、不要イメージの一括削除（prune）が可能です。
- **対応エージェント:** `quagent run` では `--agent` オプション（TUI でも選択可）でエージェントを指定します。
  - `opencode` (既定、`--auto` 付与)
  - `claude` (Claude Code、`--dangerously-skip-permissions` 付与)
  - `agy` (Antigravity CLI、`--dangerously-skip-permissions` 付与)
  - `codex` (Codex CLI、`--dangerously-bypass-approvals-and-sandbox` 付与)
  ※ VM による強固な隔離環境下で動作するため、エージェント側の実行前プロンプト確認はスキップして自律実行されます。

### 画面構成: 上部でエージェントが自律動作し、下部承認コンソールで人間が判定する

`quagent run` を実行すると tmux セッションが作成されます。

- **上部ペイン:** VM 内のエージェントが自律動作します。終了時は対話シェルに戻り、`↑` + Enter で即座にエージェントを再起動できます。
- **下部ペイン:** 承認コンソールが開き、接続先申請や動的緩和、PR 作成などの承認・対話を行います。

### セッション終了と再起動: シェル終了で TUI に戻り、VM 保持のままエージェントを選び直せる

エージェントペインのシェルを終了するか、承認コンソールで `quit` を入力すると tmux セッションが閉じ、ホスト側の TUI に戻ります。

- **エージェントを選び直して再起動:** VM を破棄せず、エージェントや設定のみを差し替えて即座に再起動します。
- **終了:** VM を破棄します。終了時のみ、該当 VM のホスト側ログを残すか選択できます。

`Ctrl-b d` でデタッチしたときは、`quagent run` を動かしている端末がそのまま案内を出します。**Enter** で同じ端末から再アタッチでき、**`q` + Enter** で終了します（デタッチ中も VM は保持されます）。戻るためだけに別の SSH 接続を張る必要はありません。別の端末から戻る場合は `tmux attach -t quagent-<...>` を使います。

---

## 各種機能と設定: 必要に応じて設定ファイルで調整する

### 1. LLM 接続: API キーを VM に渡さずプロキシ経由で利用する

API キー等の認証情報は VM 内には配置しません。ホスト側の認証プロキシが正規の認証情報を付与して上流 API へリクエストを転送します。

設定ファイル: `~/.config/quagent/config.json`

#### opencode の設定例
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
※ OpenCode の API キーは Zen（Free モデルを含む全モデル）と Go（サブスクリプション）で共通です。`opencode-go` または `opencode` のどちらか一方を設定すると、もう片方のエンドポイントも同じ認証情報で自動的にプロキシへ登録されます。これにより、VM 内の OpenCode から Go モデルと Zen の Free モデル（`opencode/big-pickle` 等）をシームレスに切り替えて利用できます。

#### Claude Code の設定例
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
※ サブスクリプション利用時は、ホスト側で `claude setup-token` により長期トークンを作成して機密情報に指定します。モデルを指定する場合は `"claude": { "subscription": "max", "model": "claude-sonnet-5.5" }` のように追記します。

#### agy (Antigravity CLI) の設定例
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
    "agy": {
      "subscription": true,
      "refresh_token_command": ["pass", "show", "secret-service/Default/password_for_antigravity_on_gemini__gl2m"]
    }
  }
  ```
  ※ ホスト側の OAuth ログイン情報から一時トークンを生成してプロキシが付与します（長期トークンは VM に渡りません）。`refresh_token_command` のほか、環境変数（`refresh_token_env`）やファイルパス（`refresh_token_file`）も指定可能です。詳細は [docs/design.md](docs/design.md#llm-api-の認証プロキシ-一時トークンで中継し本物の-api-キーを-vm-に渡さない) を参照してください。

#### Codex (Codex CLI) の設定例
- **サブスクリプションで利用 (ChatGPT Plus / Pro 等、ホスト側でログイン済み):**
  ```json
  {
    "codex": {
      "subscription": true
    }
  }
  ```
  ※ ホスト側の OAuth ログイン情報（`~/.codex/auth.json`）から一時トークンを生成してプロキシが付与します（長期トークンは VM に渡りません）。`refresh_token_command` のほか、環境変数（`refresh_token_env`）やファイルパス（`refresh_token_file`）も指定可能です。モデルを指定する場合は `"model": "gpt-5.2-codex-medium"` などを追記します。
- **API キーで利用:**
  ```json
  {
    "providers": {
      "openai": {
        "upstream": "https://api.openai.com/v1",
        "secret_command": ["pass", "show", "openai/api-key"]
      }
    },
    "codex": { "model": "gpt-5.2-codex-medium" }
  }
  ```

- **機密情報の取得方法:** `secret_command`（コマンド実行）、`secret_env`（環境変数）、`secret_file`（ファイルパス）から選択できます。

---

### 2. 外部通信の申請・承認 (MCP): コンソール上で対話的に許可する

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

- **Docker および言語パッケージの Pull-through キャッシュ:**
  - VM 内の rootless Docker および主要言語パッケージマネージャー（Go, Cargo, npm, pip）による取得は、vsock 経由でホスト側のローカルキャッシュ（`127.0.0.1:51337`）へ自動転送されます。
  - **キャッシュヒット時:** 完全オフラインかつ承認不要で即座に高速返却されます。
  - **依存関係事前解決とバッチ集約承認 (連打防止):** キャッシュミス時に外部からダウンロードする際は、プロキシが上流メタデータから依存関係ツリーを事前に走査・解決し、必要な全パッケージ一覧をまとめて承認コンソールで一括確認（`[y]` 承認して取得 / `[n]` 拒否）を求めます。何十個もの依存パッケージも 1 回の承認でまとめて取得可能です。
  - **エコシステム対応:** Docker Hub (`docker.io`), `ghcr.io`, Go (`proxy.golang.org`), Cargo (`crates.io`), npm (`registry.npmjs.org`), PyPI (`pypi.org`) に標準対応。
  - **保存先:** ホスト側の `~/.cache/quagent/proxy/` 配下に統一されています（Docker は `docker/blobs/`, `docker/manifests/`、言語パッケージは `go/`, `cargo/`, `npm/`, `pypi/`）。保存先と容量上限は `~/.config/quagent/config.json` の `cache`（`dir`, `max_size_gib`）で変更できます。
  - **管理:** `quagent cache ls` で各エコシステムのキャッシュ状況の確認、`quagent cache clean [ecosystem]` または TUI から削除が可能です。

---


### 3. データ持ち出し防止 (Content Guard / DLP): 秘密鍵やトークンの漏洩を自動遮断する

quagent は VM から外部への意図しない機密情報漏洩や不正な通信チャネルの確立を多層で防止します。

- **Content Guard / DLP (コンテンツ検査エンジン):**
  Web 透過プロキシを通過する HTTP リクエスト（URL、ヘッダ、ボディ）を走査し、機密情報のパターンを検知した場合は即座に HTTP 403 で遮断します。
  - **検知対象:** 秘密鍵（RSA/EC/OpenSSH）、各種認証情報（AWS, GitHub PAT, Google, Slack, OpenAI, JWT 等）
  - **設定:** 無効化する場合は `config.json` で `"content_guard": { "enabled": false }` を指定します。
- **DNS トンネリング検知:** 許可ドメイン配下であっても、不正な FQDN 長、異常なエントロピー、深層サブドメイン、TXT/ANY クエリ等の兆候を検知して即座に遮断（REFUSED 応答）します。
- **宛先別動的緩和 (MCP):** エージェントが特定 API 呼び出し等で追加ヘッダやボディ送信を必要とする場合、MCP の `request_header_relaxation` を通じて動的緩和を申請できます。不要になった緩和は `release_header_relaxation` で解放できます。

---

### 4. HTTP リクエストの絞り込み: 不要ヘッダの除去とメソッド制限 (任意)

`header_policy` を有効にすると、VM から外部への HTTP リクエストから不要なヘッダ（Cookie, Referer, 独自ヘッダ等）を除去し、User-Agent を固定します。リクエストボディの送信遮断や HTTP メソッドの制限も可能です。

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

### 5. 外部ツール連携 (OpenAPI): 仕様から MCP ツールを自動生成する

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

### 6. クリップボード共有 (OSC 52): VM からの書き込みを対話確認する

VM 内のエージェントが端末経由でクリップボードへ書き込もうとすると (OSC 52)、ホスト側で捕捉して承認コンソール上で確認を求めます（`[y] コピー / [n] 拒否`）。クリップボードの読み出し要求は常に拒否されます。

反映方法は `config.json` の `clipboard` で設定します:
- `"clipboard": { "method": "tmux" }` (既定)
- `"clipboard": { "method": "osc52" }` (端末へ OSC 52 を再送)
- `"clipboard": { "method": "command", "command": ["wl-copy"] }`

---

### 7. PR 作成と再署名: 一時鍵のコミットをホスト側の正規鍵で push する

エージェントは `/work` の作業ブランチでコミットを作成し、MCP の `create_pull_request` ツールを通じて PR 作成を依頼します。

- **安全なコミット再署名:** VM 内では一時的な使い捨て鍵でコミットが作成され、ホスト側がこれを取り込んで正規の鍵で再署名した上で push します（GitHub トークンや秘密鍵は VM に渡りません）。
- **PR 作成の承認:** 既定で承認制（`--pr-approval`）となっており、push 前に承認コンソール上でブランチ名・マージ先・タイトル・本文の確認を求めます。

---

### 8. 共通スキルの配置: プロジェクトやエージェントを問わずスキルを共有する

プロジェクトや使用エージェントを問わず共通して利用したいスキル（調査手順やコーディング規約等）は、ホスト側のスキルディレクトリに配置することで、VM 起動時に自動的にすべての対応エージェントへ反映されます。

- **配置場所 (ホスト側):** `~/.config/quagent/skills/<スキル名>/SKILL.md` (既定)
- **ディレクトリ変更 (任意):** `config.json` の `"skills_dir": "/path/to/skills"` で任意の場所を指定可能
- **対応エージェント:** `opencode`、`claude`、`agy` のすべてに自動でシンボリックリンクが展開されます。セッション中のエージェント再起動・切り替え時にもそのまま利用可能です。
- **転送と安全設計:** 合計 16 MiB を上限として通常ファイルとディレクトリのみを vsock 経由で安全にコピーします（VM 側からホストのファイルは変更されません）。

---

## Web 遠隔操作 (hub): Tailscale 越しにスマホからセッションを操作する

`quagent hub` を起動しておくと、登録したセッションの端末（エージェント TUI）と承認パネルを、ブラウザから操作できます。スマートフォンからの利用を想定し、Tailscale 越しのアクセスを前提とします。

```sh
quagent hub                     # 別の端末/プロセスで常駐させる
quagent run --hub               # `--hub` を付けた run (または TUI の「hub に登録する」) が hub に登録される
```

- **アクセス URL:** 登録時に `http://<Tailscale の IP>:8787/s/<シークレット>` が表示されます。**この URL を知っている人だけ**がアクセスでき、初回に開くとシークレットは Cookie に交換されて URL は無効になります（以降は再発行が必要）。
- **URL の再発行:** 承認コンソール (TUI) で `r` を押すと、新しいワンタイム URL が表示されます。接続済みの端末はそのまま使え、新しい端末を追加できます。
- **端末と承認:** ブラウザの端末 (xterm.js) でエージェントを操作でき、承認パネルで接続先申請や PR 作成などの判定を行えます。承認パネルは TUI と同じ「承認待ち / ログ / 履歴」のタブを持ちます。端末側の承認コンソールと並立して使えます。ブラウザ資産は quagent に同梱され、外部 CDN に依存しません。
- **配色 (ブラウザ UI):** 端末と承認パネルは既定で OS のライト / ダーク設定に追従します。ヘッダ右のボタンで「システムに合わせる → ライト → ダーク」を順に切り替えられ、選んだ設定はブラウザに保存されます。ライトは白磁色を基調にした配色です。
- **配色 (エージェント TUI):** エージェントは端末に背景色を問い合わせ (OSC 11)、その答えに合わせて自分の配色を変えます。quagent はこの問い合わせに直接答えるので、tmux に実端末が付いているかどうかで配色が揺れません (実端末をデタッチしても黒く転びません)。既定では `quagent run` を実行した端末の背景色から自動判定し、`--term-theme light|dark` で固定できます。判定できなかったときは、hub に接続中のブラウザの配色 (OS 追従またはヘッダのボタンで選んだもの) を書き込んだファイルを読んで答えます。ブラウザからの配色は接続時と切り替え時に送られます。
- **Discord 通知:** セッションの開始と承認ダイアログを Discord Webhook へ通知します。通知のリンクをタップするとスマホのブラウザで承認できます。
- **待ち受け:** 既定では Tailscale の IP にだけバインドします（`0.0.0.0` には公開しません）。HTTPS を使いたい場合は `tailscale serve` を併用します。

設定ファイル: `~/.config/quagent/config.json`

```json
{
  "hub": {
    "enabled": true,
    "listen": "tailscale",
    "port": 8787,
    "notify": {
      "discord_webhook_command": ["pass", "show", "quagent/discord-webhook"],
      "on_session_start": true,
      "on_approval": true
    }
  }
}
```

- `listen` は `"tailscale"`（既定）または `"127.0.0.1:8787"` のような明示アドレスを指定します。`base_url` を指定すると、通知や URL 生成に使う基点を上書きできます。
- Webhook の URL は `discord_webhook_command` のほか `discord_webhook_env` / `discord_webhook_file` でも指定できます（設定ファイルに生の URL を書かずに済みます）。

### 常駐させる (systemd user unit)

hub は run と同じユーザーで動かす必要があります（`hub.sock` は 0600、tmux サーバーはユーザーごとに 1 つ）。`systemd --user` ならこの条件を満たします。サンプルは [contrib/systemd/quagent-hub.service](contrib/systemd/quagent-hub.service) です。

```sh
make install            # ~/.local/bin/quagent を配置
make install-hub-unit   # ~/.config/systemd/user/quagent-hub.service を配置
systemctl --user daemon-reload
systemctl --user enable --now quagent-hub
loginctl enable-linger "$USER"   # ログインしていない間も動かす場合
```

- Tailscale が上がる前に起動すると待ち受けに失敗しますが、`Restart=on-failure` で再試行します。
- `tailscale` と `tmux` に PATH が通っている必要があります。通っていない場合は unit の `Environment=PATH=...` を有効にしてください。

---

## 困ったときは (トラブルシューティング)

| 症状 | 主な原因 | 対処法 |
| :--- | :--- | :--- |
| `quagent run` で仮想化エラーが出る | ホストで KVM が無効、またはアクセス権限不足 | `/dev/kvm` のパーミッションを確認してください。VM 内でさらに実行する場合は `--nested-virt` を指定します。 |
| agy 起動時に認証エラーで終了する | ホスト側の agy OAuth ログインが未完了 | ホスト側で `agy models` を実行して再ログインを完了させてから起動してください。 |
| 外部リクエストが HTTP 403 で遮断される | Content Guard がリクエスト内の認証情報を検知 | 送信内容が正当な場合は、承認コンソールまたは MCP で動的緩和を申請してください。 |
| 外部接続の申請がタイムアウトした | 10分間承認コンソールで応答しなかった | 拒否後 10 分間は再申請できません。承認コンソールで内容を確認して再実行してください。 |

---

## 関連ドキュメントとスキル

- [docs/design.md](docs/design.md) — 内部アーキテクチャ、隔離の仕組み、多層防御一覧表、脅威モデル、詳細仕様
- [docs/development.md](docs/development.md) — 開発ガイドライン、ビルド・テスト手順、コーディング指針、更新ワークフロー
- [.opencode/skills/recipe-authoring/](.opencode/skills/recipe-authoring/SKILL.md) — ベースイメージレシピの作成・修正規約
- [.opencode/skills/document-authoring/](.opencode/skills/document-authoring/SKILL.md) — ドキュメント構成・作成規約

---

## 次の一歩

まずは `make build` でバイナリを作成し、`quagent` を実行してベースイメージのビルドと対話セッションを試してください。

## ライセンス

MIT

