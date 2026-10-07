# quagent

プロジェクトごとに使い捨ての QEMU VM を起動し、外向き通信を**ホスト側で**厳格に制限した環境で各種コーディングエージェントを安全に動かすためのツールです。

- **隔離の主体は VM とホスト:** QEMU は非特権（unprivileged）な user/mount/net 名前空間内で動作し、許可リストに登録されていない接続先への新規アウトバウンド通信は VM の外側（nftables）ですべて遮断されます。ゲスト OS 内の root 権限からもファイアウォールルールを参照・改変することはできません。ホスト側の全処理は非 root（非特権）ユーザーで動作します。
- **ホストと VM 間の通信は vsock を利用:** ホストからの VM 操作（コマンド実行、仮想端末、リポジトリの同期、PR 作成用の git fetch など）はすべて vsock 経由で行われます。ネットワークを経由しないため、sshd は既定で停止されます。
- **VM 内部における多層防御:** VM 内で実行されるコマンドに対しても、エージェント自身の権限では解除できない seccomp / Landlock による制限を適用します（危険なシステムコールへのアクセス経路を減らすための追加の防御層です）。
- **機密情報を VM 内に保持しない設計:** LLM の API キー、コミット署名鍵、GitHub トークンなどの機密情報はホスト側にのみ保持し、ホスト側のプロキシがリクエストに付与します。

仕組みの詳細や脅威モデルについては [docs/design.md](docs/design.md)、開発やコーディング指針については [docs/development.md](docs/development.md) を参照してください。本 README では基本的な使い方を説明します。

## 使い方

```sh
make build                     # bin/quagent (VM 内にも持ち込むため静的リンクでビルド)
make install                   # ~/.local/bin/quagent にインストール (PREFIX で変更可)
cd <repo> && quagent           # TUI: 起動設定 (対象リポジトリ・OS・CPU・メモリ) とベースイメージの管理
```

TUI を使わずに CLI から直接操作することも可能です:

```sh
quagent image recipes          # 利用可能なレシピ (OS) の一覧表示
quagent image build [--refresh|--incremental] arch   # ベースイメージのビルド / 差分更新
                               #   --refresh: クラウドイメージを再取得してクリーンビルド
                               #   --incremental: 前回のイメージをもとに差分更新 (カーネルは更新がある場合のみ再構築)
quagent image ls / rm IMAGE    # ビルド済みイメージの一覧表示・削除
quagent guard check "本文"      # ローカル LLM によるリクエスト内容検査のテスト実行 (後述)
quagent run --image arch       # VM を起動 (--ssh: ホストからの SSH 接続を許可、--mount-tmp: .tmp をホスト・ゲスト間で同期)
```

ベースイメージのビルドは起動画面からではなく、管理画面（ベースイメージの管理）で行います。初回起動時など利用可能なイメージがまだ存在しない場合は、起動画面ではなく管理画面が開き、イメージのビルドへと誘導されます。起動画面で選択した OS のイメージが存在しない場合も同様に管理画面へ案内されます。

ビルド開始前には、ビルド用 VM の CPU コア数とメモリ容量を指定できます（前回指定した値が既定値となります。カーネルを再構築するレシピではコア数が多いほど高速に完了します）。前回のイメージが存在する場合は「差分更新」または「焼き直し（再ビルド）」を選択できます。管理画面では各 OS のイメージ作成・更新のほか、イメージの個別削除（各 OS の最新イメージも削除可能）や、古いイメージを残さない一括クリーンアップ（prune）が行えます。

`quagent run` を実行すると tmux セッションが作成され、上部ペインで VM 内のエージェントが、下部ペインでアクセスの承認コンソールが開きます。利用するエージェントは `--agent` オプション（TUI でも選択可能）で指定します。`opencode`（既定、`--auto` 付与）、`claude`（Claude Code、`--dangerously-skip-permissions` 付与）、`agy`（Antigravity CLI、`--dangerously-skip-permissions` 付与）に対応しています。VM による強固な隔離環境下で動作するため、エージェント側でのプロンプト確認はスキップして自律実行されます。

エージェントの起動処理は VM 内の `/entrypoint.sh` にまとめられており、エージェントが終了すると対話型シェルに戻ります。`~/.bashrc` の設定により `/entrypoint.sh` がコマンド履歴の先頭に登録されているため、`↑` キーを押して Enter を押すだけで即座にエージェントを再起動できます。エージェントペインのシェルを終了するか、承認コンソールで `quit` を入力すると tmux セッションが閉じ、ホスト側の端末に戻って次の操作を TUI で選択できます（エージェントを選び直して同じ VM のまま再起動するか、終了するか。終了を選んだときだけ、該当 VM のホスト側ログを残すかどうかも指定できます。Ctrl-C 等で中断した場合は「終了・ログを残す」となります）。再起動では VM を破棄せず、`/entrypoint.sh` と該当エージェントの設定だけを差し替えます（agy のサブスクリプションは agy で起動した VM でのみ選べます）。終了を選ぶと VM は破棄されます。なお、tmux セッションからデタッチした場合は、セッションが維持されている限り VM はバックグラウンドで動作し続けます。

## 接続先の申請 (MCP)

VM からの外部宛先へのアウトバウンド通信は既定で一切遮断されています。エージェントが外部通信を必要とする場合は、MCP エンドポイント (`http://quagent.host:7070/mcp`) の `request_network_access` ツールを通じて「申請理由」と「ドメイン一覧」をまとめて申請し、ユーザーが承認コンソール上で一括して可否を判断します。

| 入力 | 意味 |
| --- | --- |
| `1` | 今回のみ許可 (5分間、新規接続を許可) |
| `2` | このセッション中のみ常に許可 (以降の確認をスキップ) |
| `3` | 今後常に許可 (全プロジェクト共通。npm や PyPI などの汎用パッケージリポジトリ等を想定) |
| `d` | 拒否 |
| `q` | 質問・指示を返す (エージェント側で回答を申請理由に反映して再申請させる) |

10分間応答がない場合はタイムアウトとして自動拒否され、その旨がエージェントに通知されます。エージェントは不要になったアクセス許可を `release_network_access` で明示的に解放できます。アクセス許可は「新規接続の開始を認めるか」の判定であるため、期限切れや解放によって既に確立済みの既存接続が切断されることはありません。「今後常に許可」したドメインは `~/.local/share/quagent/always-allow.json` に保存され、`quagent always ls` や `quagent always rm DOMAIN...`（または TUI）から確認・削除できます。

アクセス許可は DNS レベルで判定されます。許可されたドメインであっても、DNS 応答の解決先 IP がプライベート LAN、ループバック、リンクローカル（クラウド事業者のメタデータエンドポイントなど）、CGNAT（Tailscale 等）などの内部向けアドレスである場合は通信を遮断します。また、許可された IP 宛ての TCP 80/443 通信は透過プロキシを経由し、実際の接続時に提示されたホスト名（TLS の SNI、HTTP の Host ヘッダー）が許可済みドメインと一致しない場合は即座に切断します。詳細な判定ロジックは [docs/design.md](docs/design.md) を参照してください。

## クリップボード (OSC 52)

VM 内のエージェントが端末経由でクリップボードへ書き込もうとすると (OSC 52)、ホスト側でエージェントペインの出力を監視・捕捉し、承認コンソール上で確認を求めます（`[y] コピーする / [n] 拒否`、内容の先頭プレビューとサイズを表示）。ホスト側の tmux やターミナルエミュレータへ直接届くことはありません。クリップボードの読み出し要求（ホスト側のクリップボード内容を VM 側へ送信させようとする要求）は常に拒否されます。

承認待ちは最大 1 件のみ保持され、待機中に発生した新たな要求は破棄されます。また、連続要求の間隔制限（3秒以上）、最大サイズ制限（64 KiB）、タイムアウト（2分間無応答で自動拒否）が設けられています。

承認されたクリップボードデータのホスト側への反映方法は、`config.json` の `clipboard` で設定します:

```json
"clipboard": { "method": "tmux" }                         // 既定。tmux load-buffer -w を実行
"clipboard": { "method": "osc52" }                        // 起動元端末に OSC 52 シーケンスを再送 (Kitty など)
"clipboard": { "method": "command", "command": ["wl-copy"] }
```

## LLM の設定

API キー等の認証情報は VM 内には配置しません。VM 内の opencode は `http://quagent.host:7070/llm/<provider>` を baseURL として利用し、ホスト側の認証プロキシが正規の認証情報を付与した上で本来の上流 API へリクエストを転送します。VM からは外部の API ドメインへ直接通信することはできません（既定のアウトバウンド通信は遮断されています）。

`~/.config/quagent/config.json`:

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

Claude Code を利用する場合は `providers` に `anthropic` を設定します。サブスクリプション（Pro/Max）で利用する場合は、ホスト側で `claude setup-token` を実行して長期（1年）トークンを作成し、それを機密情報として指定した上で `claude.subscription` にプラン（`pro` / `max` / `team` / `enterprise`）を指定します:

```json
"providers": {
  "anthropic": {
    "upstream": "https://api.anthropic.com",
    "secret_command": ["pass", "show", "anthropic/claude-oauth-token"]
  }
},
"claude": { "subscription": "max" }
```

agy (Antigravity CLI) を利用する場合は `providers` に `gemini`（Gemini API）を設定します:

```json
"providers": {
  "gemini": {
    "upstream": "https://generativelanguage.googleapis.com",
    "header": "x-goog-api-key", "prefix": "",
    "secret_command": ["pass", "show", "gemini/api-key"]
  }
},
"agy": { "model": "gemini-3.8-flash-high" }
```

サブスクリプション（ホスト側で agy にログイン済み）で利用する場合は、API キーの代わりに `"agy": { "subscription": true }` を指定します。ホスト側の OAuth ログイン情報から一時トークンを再生成してプロキシが付与するため、VM 内の agy はサブスクリプション枠で動作します（この場合 `providers` の `gemini` 設定は不要です）。VM に渡されるのは起動時に生成された有効期限 1 時間の一時トークンのみであり、長期の refresh_token がホストから流出することはありません。この一時トークンはプロキシへの接続認証トークンとしても使用されます。なお、起動直後のユーザー情報確認とプロフィール画像の取得はゲストから直接アクセスされるため、その宛先（`www.googleapis.com` および画像ホスト）のみ一時的に egress を開放します。開放は `--agent agy` の指定時のみ行われ、初回推論が完了すると直ちに遮断されます（以降は通常の申請・承認フローに戻ります）。

`claude.model` で VM 内の Claude Code、`agy.model` で VM 内の agy の既定モデル、`claude.theme` でカラーテーマ、`agy.color_scheme` でカラースキームを指定できます（既定ではホスト側の各エージェントの設定を引き継ぎます）。provider ID は opencode の provider ID と一致させます。機密情報の取得方法は `secret_env`（環境変数名）、`secret_file`（ファイルパス）、`secret_command`（コマンド実行）から選択できます。`opencode.model` は VM 内の opencode の既定モデルであり、`providers` に設定した provider のモデルを指定します。プロキシが転送対象とする操作（推論およびモデル一覧取得）や `allow` リストの指定方法については [docs/design.md](docs/design.md) を参照してください。

## ツールサーバー (OpenAPI)

Open WebUI のツールサーバーなど、OpenAPI 仕様で公開された外部サーバーを、VM 内のエージェントから MCP ツールとして利用できます。ホスト側で OpenAPI 仕様 (JSON) を取得して各 operation を MCP ツールへと自動変換し、既存の MCP エンドポイント (`http://quagent.host:7070/mcp`) に登録します。ツール呼び出しはホストがツールサーバーへプロキシ転送するため、VM からツールサーバーへ直接通信する必要はなく（プライベート LAN 上のサーバーも利用可能）、API キーなどの機密情報が VM 内に配置されることもありません。

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

- 各サーバーの設定キー（`my-tools` など）がツール名のプレフィックスとなり、ツールは `<キー>__<operationId>` という形式で公開されます。
- 認証が不要なサーバーの場合は `secret_env` / `secret_file` / `secret_command` をすべて省略します（認証なしで登録されます）。認証が必要な場合は `providers` と同様に設定し、`header` / `prefix` で認証ヘッダーのカスタマイズも可能です（既定: `Authorization: Bearer <機密情報>`）。機密情報は仕様の取得およびツール呼び出しの転送時にホスト側で付与されます。
- OpenAPI 仕様は `url` に `openapi_path`（既定: `/openapi.json`）を連結した URL から取得します。Open WebUI のツールサーバー等でパスが異なる場合は適宜指定してください（OpenAPI 3.x の JSON 形式に対応、YAML は非対応）。
- パス・クエリ・ヘッダーの各パラメータは同名のツール引数となり、リクエストボディは `body` 引数として渡されます（JSON、または `application/x-www-form-urlencoded` 形式に対応）。
- 仕様の取得は VM 起動時に 1 回のみ行われます。取得できなかったサーバーは警告ログを出力してスキップされ、VM の起動処理自体は継続します。
- ツール呼び出しのリクエストもコンテンツガード（有効時）の検査対象となります。レスポンス本文は最大 1 MiB まで取得され、画像等のテキスト以外のレスポンスはメタデータ（Content-Type とサイズ）のみを返します。

## コンテンツガード (任意)

接続先ドメインの許可制だけでは、許可済みドメインへの通信に機密情報が意図せず（または悪意を持って）紛れ込むケース（例: User-Agent ヘッダーに機密情報を埋め込むなど）を防止できません。任意で、ホスト側で平文として参照可能なリクエスト内容をローカル LLM に検査させ、機密情報と判断される具体的な根拠（`evidence`）が抽出された場合のみ、承認コンソールへ転送して人間の判断を仰ぐことができます。判定の仕組みや限界については [docs/design.md](docs/design.md) を参照してください。

```json
"guard": {
  "enabled": true,
  "backend": "openai",
  "endpoint": "http://127.0.0.1:8080",
  "model": "qwen2.5-3b-instruct",
  "timeout_seconds": 30,
  "max_bytes": 8192,
  "max_chunks": 8,
  "num_ctx": 8192,
  "concurrency": 1,
  "mode": "ask",
  "on_error": "ask",
  "inspect_https": false
}
```

| フィールド | 意味 |
| --- | --- |
| `enabled` | コンテンツ検査を有効にするか（既定: false） |
| `backend` | `openai`（既定。llama.cpp などの OpenAI 互換サーバー）または `ollama` |
| `endpoint` | ローカル LLM の URL（既定: `http://127.0.0.1:8080`、llama.cpp 想定） |
| `model` | 使用するモデル名（既定: `qwen2.5-3b-instruct`。llama.cpp では起動時の `--alias` と一致させる） |
| `timeout_seconds` | LLM 呼び出し 1 回（1 チャンク）あたりのタイムアウト秒数（既定: 30。リクエスト全体の分割検査は最大 10 分） |
| `max_bytes` | LLM に渡す本文 1 チャンクあたりのバイト数（既定: 8192、上限: 32768） |
| `max_chunks` | 本文の最大分割チャンク数（既定: 8、上限: 64） |
| `num_ctx` | ローカル LLM のコンテキスト長（トークン数。既定: 8192、範囲: 2048〜131072） |
| `concurrency` | 同時に実行する検査数（GPU 1 枚運用の場合は既定値の 1 を推奨） |
| `mode` | `evidence` が検出され deny と判定された場合の動作。`ask`（既定。承認コンソールで確認）/ `deny` / `advisory` |
| `on_error` | 検査に失敗（エラー）した場合の動作。`ask`（既定。承認コンソールで確認）/ `deny` / `allow` |
| `inspect_https` | 外部宛先への HTTPS 通信（および平文 HTTP）も TLS 終端して内容を検査する（既定: false） |
| `passthrough_https` | TLS 終端を行わず透過させるホスト名一覧（証明書ピニングを行うクライアント向け） |

検査に使用するローカル LLM（llama.cpp など）のセットアップ方法、パラメータのチューニング指針、制限事項については [docs/design.md](docs/design.md) を参照してください。`quagent guard check "本文"` コマンドで単体の検査動作をテストできます。

## PR の作成と署名

エージェントは `/work` の保護されていない作業ブランチにコミットを作成し、MCP の `create_pull_request` ツールを通じてプルリクエストの作成を依頼します。GitHub の認証トークンやコミット署名鍵は VM 内には配置されません。VM 内で作成されたコミットは一時的な使い捨て鍵で署名され、ホスト側がこれを取り込んで正規の鍵で再署名した上で push します。仕組みの詳細は [docs/design.md](docs/design.md) を参照してください。

`quagent run --pr-approval`（TUI では「PR を承認制にする」）を指定すると、ホスト側で push する前に、承認コンソール上でブランチ名・マージ先ベースブランチ・タイトル・本文を提示して `y` / `n` の確認を求めます。`n` が入力された場合や 10 分間応答がない場合は、push および PR 作成は中止されます。

## ホストに必要な環境

`qemu-system-x86_64` (KVM)、`qemu-img`、`xorriso`、`slirp4netns`、`unshare`/`nsenter`/`prlimit` (util-linux)、`nft`、`git`、`gh`、`tmux`。

また、非特権ユーザー名前空間（unprivileged user namespace）が有効化されており、vsock（`/dev/vhost-vsock`、カーネルモジュール `vhost_vsock`）が利用可能である必要があります。

`--ssh` を利用する場合は `ssh` / `ssh-keygen` も必要です。署名検証付きのレシピ（Arch）でイメージをビルドする場合は `gpg` / `gpgv` も必要です。UEFI レシピ（Gentoo）をビルド・実行する場合は OVMF（`edk2-ovmf`、`ovmf` など。統合イメージ `OVMF.fd` / `OVMF.4m.fd`）が必要です。

## ライセンス

MIT
