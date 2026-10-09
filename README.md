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
quagent run --image arch       # VM を起動 (--ssh: ホストからの SSH 接続を許可、--mount-tmp: .tmp をホスト・ゲスト間で同期)
```

ベースイメージのビルドは起動画面からではなく、管理画面（ベースイメージの管理）で行います。初回起動時など利用可能なイメージがまだ存在しない場合は、起動画面ではなく管理画面が開き、イメージのビルドへと誘導されます。起動画面で選択した OS のイメージが存在しない場合も同様に管理画面へ案内されます。

ビルド開始前には、ビルド用 VM の CPU コア数とメモリ容量を指定できます（前回指定した値が既定値となります。カーネルを再構築するレシピではコア数が多いほど高速に完了します）。前回のイメージが存在する場合は「差分更新」または「焼き直し（再ビルド）」を選択できます。管理画面では各 OS のイメージ作成・更新のほか、イメージの個別削除（各 OS の最新イメージも削除可能）や、古いイメージを残さない一括クリーンアップ（prune）が行えます。

`quagent run` を実行すると tmux セッションが作成され、上部ペインで VM 内のエージェントが、下部ペインでアクセスの承認コンソールが開きます。利用するエージェントは `--agent` オプション（TUI でも選択可能）で指定します。`opencode`（既定、`--auto` 付与）、`claude`（Claude Code、`--dangerously-skip-permissions` 付与）、`agy`（Antigravity CLI、`--dangerously-skip-permissions` 付与）に対応しています。VM による強固な隔離環境下で動作するため、エージェント側でのプロンプト確認はスキップして自律実行されます。

エージェントの起動処理は VM 内の `/entrypoint.sh` にまとめられており、エージェントが終了すると対話型シェルに戻ります。`~/.bashrc` の設定により `/entrypoint.sh` がコマンド履歴の先頭に登録されているため、`↑` キーを押して Enter を押すだけで即座にエージェントを再起動できます。エージェントペインのシェルを終了するか、承認コンソールで `quit` を入力すると tmux セッションが閉じ、ホスト側の端末に戻って次の操作を TUI で選択できます（エージェントを選び直して同じ VM のまま再起動するか、終了するか。終了を選んだときだけ、該当 VM のホスト側ログを残すかどうかも指定できます。Ctrl-C 等で中断した場合は「終了・ログを残す」となります）。再起動では VM を破棄せず、`/entrypoint.sh` と該当エージェントの設定だけを差し替えます（agy のサブスクリプションへの切り替えにも対応）。終了を選ぶと VM は破棄されます。なお、tmux セッションからデタッチした場合は、セッションが維持されている限り VM はバックグラウンドで動作し続けます。

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

サブスクリプション（ホスト側で agy にログイン済み）で利用する場合は、API キーの代わりに `"agy": { "subscription": true }` を指定します。ホスト側の OAuth ログイン情報から一時トークンを再生成してプロキシが付与するため、VM 内の agy はサブスクリプション枠で動作します（この場合 `providers` の `gemini` 設定は不要です）。VM に渡されるのは起動時に生成された有効期限 1 時間の一時トークンのみであり、長期の refresh_token がホストから流出することはありません。この一時トークンはプロキシへの接続認証トークンとしても使用されます。なお、起動直後のユーザー情報確認とプロフィール画像の取得はゲストから直接アクセスされるため、その宛先（`www.googleapis.com` および画像ホスト）のみ一時的に egress を開放します。開放は agy の起動時（初回指定またはセッション終了後の再起動時）に行われ、初回推論が完了すると直ちに遮断されます（以降は通常の申請・承認フローに戻ります）。header_policy が有効な場合も、この宛先だけは TLS 終端せず素通しします（終端すると Authorization が落とされてユーザー情報確認が 401 になり agy が起動できないため）。サブスクで agy を起動する前（再起動での選択時も）は、ホスト側で `agy models` による起動確認を行い、通ればすぐ終了してから VM の起動・切り替えに進みます。確認に失敗したら、初回は VM を作らず終了し、再起動時は VM を残したまま選び直しに戻ります。

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
- レスポンス本文は最大 1 MiB まで取得され、画像等のテキスト以外のレスポンスはメタデータ（Content-Type とサイズ）のみを返します。

## ヘッダ・本文の制限 (任意)

`~/.config/quagent/config.json` の `header_policy` を有効にすると、VM から外へ出る HTTP リクエストの余計なヘッダ（Referer、Cookie、独自の `X-*` など）を落とし、User-Agent を固定します（LLM 認証プロキシは対象外）。また、`deny_request_body` によるリクエストボディの送信遮断や `allowed_methods` による HTTP メソッドの制限も行えます。HTTPS のヘッダや本文を検査・制限するには TLS の終端が必要なため、使い捨て CA が VM に信頼されます。宛先ごとの緩和（許可ヘッダの追加、ボディ送信の許可、メソッドの緩和）も設定できます。詳細は [docs/design.md](docs/design.md) を参照してください。

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

## PR の作成と署名

エージェントは `/work` の保護されていない作業ブランチにコミットを作成し、MCP の `create_pull_request` ツールを通じてプルリクエストの作成を依頼します。GitHub の認証トークンやコミット署名鍵は VM 内には配置されません。VM 内で作成されたコミットは一時的な使い捨て鍵で署名され、ホスト側がこれを取り込んで正規の鍵で再署名した上で push します。仕組みの詳細は [docs/design.md](docs/design.md) を参照してください。

PR 作成は既定で承認制です（`--pr-approval`、TUI では「PR を承認制にする」が既定有効）。ホスト側で push する前に、承認コンソール上でブランチ名・マージ先ベースブランチ・タイトル・本文を提示して `y` / `n` の確認を求めます。`n` が入力された場合や 10 分間応答がない場合は、push および PR 作成は中止されます（自動 push に戻す場合は `--pr-approval=false` を指定）。

## 多層の防御

quagent は「隔離の主体はホスト側に置く」という原則のもと、境界制御と内部制限を組み合わせた多層防御を採用しています。仕組みの詳細は [docs/design.md](docs/design.md) を参照してください。

| 防御層 | 主な硬化策 |
| --- | --- |
| **第1層: ホスト & VM 境界** | QEMU (KVM) による完全隔離、Nested KVM 無効化、非特権 user/mount/net/pid/ipc/uts 名前空間による分離、QEMU プロセスのサンドボックス (QEMU ネイティブ seccomp、事前適用 cBPF seccomp、Landlock による書き込み制限、ホームディレクトリのホワイトリスト化、コアダンプ無効化)、ホスト・ゲスト間通信の vsock 単一化 (ネットワーク/sshd 排除)、ホスト側 vsock リスナーの接続元 CID 検証・接続上限 (64本) および拒否監査ログ (`host.log`) 記録、共有ファイルシステム不使用 (`git bundle` 複製、独立した noexec `.tmp` ディスク) |
| **第2層: ゲストOS & サンドボックス** | 非特権 `agent` ユーザー (sudo なし)、rootless Docker、カーネル硬化 (Gentoo レシピ等: モジュール署名強制、Lockdown、32bit/レガシー廃止)、VM 内コマンドの seccomp (危険システムコールの拒否、`AF_VSOCK` ソケット作成の遮断によるホスト側任意ポート接続防止、32bit ABI 偽装の強制終了) と Landlock (ファイル書込パス制限)、ゲスト側 vsock リスナーの Host (CID 2) 以外接続拒否と接続上限 (64本) |
| **第3層: ネットワーク & Egress 遮断** | ホスト側 nftables による Default Deny、IPv6 遮断、自前 DNS によるホワイトリスト判定と内部向け IP (プライベート/メタデータ/CGNAT) 宛ての遮断、Web 透過プロキシによる SNI / Host 検証 (TCP 80/443 のみ)、任意の TLS 終端 (`header_policy`) によるヘッダ・本文制限 |
| **第4層: 機密情報の非保持** | LLM API キー・GitHub トークン・GPG 鍵を VM に一切置かない、ホスト側 LLM 認証プロキシ (推論/モデル一覧エンドポイントのみ転送)、OpenAPI ツールサーバーのホスト側プロキシ、使い捨て鍵とホスト正規鍵による二重署名 Git PR |
| **第5層: コントロール & 承認** | 承認コンソールによる対話確認 (ネットワーク申請、既定有効な PR 作成承認、OSC 52 クリップボード書込)、不正な Git オブジェクトを検出する `transfer.fsckObjects` による取り込み検査、制御文字・Bidi 文字のエスケープによる画面偽装防止、各種リソース上限 (申請数、接続数、PR サイズ)、改変不能なホスト監査ログ (`host.log`) |
| **第6層: ビルド & サプライチェーン** | 静的リンクバイナリ (`CGO_ENABLED=0`)、ベースイメージの GPG 署名検証 (Arch, Gentoo)、Dependabot 更新の 7 日間遅延適用 |

### 今後の課題

現状のアーキテクチャにおいて、さらなる堅牢化や改善の余地がある主な課題です:

- **ホスト側リソース保護:** QEMU プロセスに対するホスト側 cgroups（CPU / メモリ上限）の設定や、qcow2 overlay ディスクのホスト容量消費上限（Disk Quota）が未設定です（※LLM API の課金クォータは API キー払い出し側で管理するためスコープ外）。
- **ゲスト内におけるプロセス間権限分離:** VM 内では AI エージェントと、エージェントが実行するテスト・ビルドコマンドが同一の UID（`agent`）で動作するため、リポジトリ内のコードからプロキシ認証トークンを読み取れる余地があります。エージェント本体とサブプロセスの UID 分離や、rootless Docker コンテナへのサンドボックス継承が望まれます。
- **データ持ち出し防止（Content Guard）の標準化:** 現在のところ `header_policy`（TLS 終端・ヘッダ/ボディ制限）は任意機能（既定無効）であり、許可済みドメイン宛ての平文/TLS 通信は素通しされます。許可ドメイン配下での DNS トンネリング検知や、コンテンツ検査の標準化が課題です。

## ホストに必要な環境

`qemu-system-x86_64` (KVM)、`qemu-img`、`xorriso`、`slirp4netns`、`unshare`/`nsenter`/`prlimit` (util-linux)、`nft`、`git`、`gh`、`tmux`。

また、非特権ユーザー名前空間（unprivileged user namespace）が有効化されており、vsock（`/dev/vhost-vsock`、カーネルモジュール `vhost_vsock`）が利用可能である必要があります。

`--ssh` を利用する場合は `ssh` / `ssh-keygen` も必要です。イメージをビルドする場合は `gpg` / `gpgv` も必要です（すべての組み込みレシピで署名検証を行います）。UEFI レシピ（Gentoo）をビルド・実行する場合は OVMF（`edk2-ovmf`、`ovmf` など。統合イメージ `OVMF.fd` / `OVMF.4m.fd`）が必要です。

## ライセンス

MIT
