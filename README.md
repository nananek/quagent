# quagent

プロジェクトごとに使い捨ての qemu VM を立て、外向き通信を **host 側で** 制限した
うえでコーディングエージェントを動かすツール。

- **閉じ込めの本体は VM と host。** qemu は unprivileged な user/mount/net namespace の
  中で動き、許可リスト以外への新規接続は VM の外側 (nftables) で拒否される。guest の
  root からもルールは見えず、改変もできない。すべて非 root で動く。
- **操作は vsock で。** host から VM の操作 (コマンド実行・端末・repo の受け渡し・PR 用
  の git fetch) は vsock で行う。ssh は既定で止める。
- **VM の中にも一枚。** VM の中のコマンドに、本人には外せない seccomp / Landlock を
  かける (危険な syscall の入口を減らす追加の一枚)。
- **秘密は VM に入れない。** API キー・署名鍵・gh のトークンは host に残し、host の
  プロキシが付ける。

仕組みの詳細と脅威モデルは [docs/design.md](docs/design.md)、開発とコーディング指針は
[docs/development.md](docs/development.md)。この README は使い方だけを書く。

## 使い方

```sh
make build                     # bin/quagent (VM に持ち込むので静的リンク)
make install                   # ~/.local/bin/quagent に入れる (PREFIX で変更可)
cd <repo> && quagent           # TUI: 起動設定 (repo・OS・CPU・メモリ) とベースイメージの管理
```

TUI を使わずに直接操作することもできる:

```sh
quagent image recipes          # 使えるレシピ (OS) の一覧
quagent image build [--refresh|--incremental] arch   # ベースイメージを焼く / 差分更新する
                               #   --refresh: クラウドイメージも取り直す
                               #   --incremental: 前回のイメージから更新 (カーネルは更新があるときだけ)
quagent image ls / rm IMAGE    # 焼いたイメージの一覧・削除
quagent guard check "本文"      # ローカル LLM による内容点検を 1 件試す (下記)
quagent run --image arch       # VM を起動 (--ssh: 人が ssh で入れる、--mount-tmp: .tmp を受け渡す)
```

焼き込みは起動画面では行わない。まだ 1 つも焼いていない初回は、メニューではなく
管理画面 (ベースイメージの管理) を開いて焼き込みへ誘導する。起動画面で選んだ OS の
イメージが無いときも、そこへ案内する。焼き込みの前に、焼き込み VM の CPU とメモリを
指定できる (前回の値が既定。カーネルを作り直すレシピはコアが多いほど速い)。前回の
イメージがあるときは「差分更新」と「焼き直し」を選べる。管理画面
からは OS ごとの焼き込み・更新のほか、イメージの個別削除 (各 OS の最新も消せる) と、
古いものを残さない prune ができる。

`quagent run` は tmux セッションを作り、上のペインで VM 内のエージェントを、
下のペインで承認コンソールを開く。エージェントは `--agent` (TUI でも選べる) で
`opencode` (既定、`--auto`)、`claude` (Claude Code、
`--dangerously-skip-permissions`)、`agy` (Antigravity CLI、
`--dangerously-skip-permissions`) を選ぶ。VM という檻の中では確認なしで動かす。
エージェントの起動は VM の `/entrypoint.sh` にまとめてあり、終了するとシェルに落ちる。
`~/.bashrc` にあらかじめ仕込んだ仕掛けで `/entrypoint.sh` が履歴の先頭に入るので、
`↑` を押して Enter するだけで素早く再起動できる。
エージェントのペインを終了するか、承認コンソールで `quit` すると VM を破棄する。
デタッチしてもセッションが続くあいだ VM は動き続ける。

## 接続先の申請 (MCP)

VM からの外向き通信は既定でゼロ。エージェントは MCP (`http://quagent.host:7070/mcp`)
の `request_network_access` で「理由 + ドメイン群」をまとめて申請し、承認
コンソールで一括して判断する。

| 入力 | 意味 |
| --- | --- |
| `1` | 今回は許可 (5 分間、新規接続を許す) |
| `2` | このセッションでは確認しない |
| `3` | 以後確認しない (全プロジェクト共通。npm や PyPI のような汎用のものに限る想定) |
| `d` | 拒否 |
| `q` | 質問を返す (エージェントは答えを理由に書いて再申請する) |

10 分応答がなければ時間切れとして拒否し、時間切れであることをエージェントに伝える。
エージェントは `release_network_access` で用済みの許可を自分で放棄できる。
許可は「新規接続を始めてよいか」の判断なので、期限切れや放棄で確立済みの接続は
切れない。「以後確認しない」は `~/.local/share/quagent/always-allow.json` に保存され、
`quagent always ls` / `quagent always rm DOMAIN...` (TUI でも可) で確認・取り消しできる。

許可は DNS で判定する。許可したドメインでも、応答の IP が LAN・loopback・link-local
(クラウドのメタデータ)・CGNAT (Tailscale) などの内部向けなら通さない。許可した IP への
TCP 80/443 は透明プロキシを通し、接続先が実際に言ってきた名前 (TLS の SNI、HTTP の
Host) が許可名と一致しなければ切る。判定の詳細は [docs/design.md](docs/design.md) を参照。

## クリップボード (OSC 52)

VM 内のエージェントが端末経由でクリップボードに書き込もうとすると (OSC 52)、
エージェントのペインの出力から host 側で抜き取り、承認コンソールで確認する
(`[y] コピーする / [n] 拒否`、中身の先頭と大きさを表示)。tmux や端末には直接
届かない。読み出し要求 (クリップボードの中身を VM に送らせるもの) は常に拒否する。
確認待ちは 1 件までで、その間の要求は捨てる。確認の間隔は 3 秒以上、大きさは
64KiB まで、2 分応答がなければ拒否。

承認したものの入れ方は `config.json` の `clipboard` で選ぶ:

```json
"clipboard": { "method": "tmux" }                         // 既定。tmux load-buffer -w
"clipboard": { "method": "osc52" }                        // 起動した端末に OSC 52 を送り直す (Kitty など)
"clipboard": { "method": "command", "command": ["wl-copy"] }
```

## LLM の設定

API キーは VM に入れない。VM 内の opencode は `http://quagent.host:7070/llm/<provider>`
を baseURL として使い、host 側のプロキシが本物の鍵を付けて本来の API へ転送する。
VM からは API のドメインにも直接出られない (既定の外向き通信はゼロ)。

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

Claude Code を使うときは `providers` に `anthropic` を入れる。サブスクリプション
(Pro/Max) で使うときは、host で `claude setup-token` を実行して長期 (1 年) のトークンを
作り、それを秘密にして `claude.subscription` にプラン (`pro` / `max` / `team` /
`enterprise`) を書く:

```json
"providers": {
  "anthropic": {
    "upstream": "https://api.anthropic.com",
    "secret_command": ["pass", "show", "anthropic/claude-oauth-token"]
  }
},
"claude": { "subscription": "max" }
```

agy (Antigravity CLI) を使うときは `providers` に `gemini` (Gemini API) を入れる:

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

サブスクリプション (host で agy にログイン済み) で使うときは、API キーの代わりに
`"agy": { "subscription": true }` と書く。host の OAuth ログインから短命トークンを
作り直してプロキシが付け、VM 内の agy はサブスク枠で動く。`providers` の `gemini`
は要らない。VM に入るのは起動時に作った 1 時間ものだけで、長期の refresh_token は
host から出さない。その 1 時間ものは窓口の追加の合言葉にもなっている。起動直後の
ユーザー情報確認は guest から直接行くので、その宛先 (`www.googleapis.com`) だけ
egress も開ける:

`claude.model` で VM 内の Claude Code、`agy.model` で VM 内の agy の既定モデル、
`claude.theme` でカラーテーマを指定できる (既定では host の Claude Code の設定を引き継ぐ)。
provider ID は opencode の provider ID と揃える。秘密の取り出し方は `secret_env` (環境変数名)・
`secret_file` (パス)・`secret_command` (コマンド) のいずれか。`opencode.model` は VM 内
opencode の既定モデルで、`providers` に挙げた provider のものを指定する。プロキシが転送する操作
(推論とモデル一覧) と `allow` の書き方は [docs/design.md](docs/design.md) を参照。

## コンテンツガード (任意)

許可したドメインへ秘密を持ち出す要求は許可制では防げない (例: User-Agent に
メールアドレスを紛れ込ませる)。任意で、host が平文で見られるリクエストの中身を手元の
ローカル LLM に点検させ、機密だと思う具体的な値 (`evidence`) を引用できたときだけ
承認コンソールに回す。判定の仕組みと限界は [docs/design.md](docs/design.md) を参照。

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
| `enabled` | 点検するか (既定 false) |
| `backend` | `openai` (既定。llama.cpp など OpenAI 互換) か `ollama` |
| `endpoint` | ローカル LLM の URL。既定 `http://127.0.0.1:8080` (llama.cpp) |
| `model` | 使うモデル。既定 `qwen2.5-3b-instruct` (llama.cpp は起動時の `--alias` と合わせる) |
| `timeout_seconds` | LLM 1 回 (塊 1 つ) の点検の上限。既定 30。分割点検全体は最大 10 分 |
| `max_bytes` | LLM に見せる本文の塊 1 つのバイト数。既定 8192、上限 32768 |
| `max_chunks` | 本文を分ける塊の数。既定 8、上限 64 |
| `num_ctx` | ローカル LLM の文脈長 (トークン)。既定 8192、範囲 2048〜131072 |
| `concurrency` | 同時に点検する件数。GPU 1 枚なら 1 (既定) |
| `mode` | `evidence` を引用した deny のとき。`ask` (既定) / `deny` / `advisory` |
| `on_error` | 点検できなかったとき。`ask` (既定) / `deny` / `allow` |
| `inspect_https` | 外向き HTTPS (と平文 HTTP) も TLS 終端して点検する。既定 false |
| `passthrough_https` | TLS 終端せず素通しする行き先。証明書を固定するクライアント向け |

点検に使うローカル LLM (llama.cpp など) の立て方、強さの調整、効かないところは
[docs/design.md](docs/design.md) を参照。`quagent guard check "本文"` で 1 件試せる。

## PR の作成と署名

エージェントは `/work` の保護されていないブランチにコミットし、MCP の
`create_pull_request` で PR 化を依頼する。gh のトークンも署名鍵も VM には入らない。
VM 内のコミットは使い捨て鍵で署名され、host が取り込んで署名し直してから push する。
仕組みの詳細は [docs/design.md](docs/design.md) を参照。

`quagent run --pr-approval` (TUI では「PR を承認制にする」) を付けると、push する前に
承認コンソールでブランチ・向き先・タイトル・本文を見せて `y` / `n` を求める。`n` または
10 分応答が無ければ push も PR の作成もしない。

## host に必要なもの

`qemu-system-x86_64` (KVM)、`qemu-img`、`xorriso`、`slirp4netns`、`unshare`/`nsenter`/
`prlimit` (util-linux)、`nft`、`git`、`gh`、`tmux`。unprivileged user namespace が有効で、
vsock (`/dev/vhost-vsock`、カーネルモジュール `vhost_vsock`) が使えること。
`--ssh` を使うなら `ssh` / `ssh-keygen` も。署名付きのレシピ (arch) でイメージを焼くなら
`gpg` / `gpgv` も。UEFI のレシピ (gentoo) を焼く・動かすなら OVMF (`edk2-ovmf`、`ovmf`
など。統合イメージ `OVMF.fd` / `OVMF.4m.fd`) も。

## ライセンス

MIT
