# quagent

プロジェクトごとに使い捨ての qemu VM を立て、外向き通信を **host 側で** 制限した
うえでコーディングエージェントを動かすためのツール。

## 隔離の仕組み

qemu は unprivileged な user/mount/net namespace の中で動く。qemu が外へ張る
socket はその netns の nftables を通り、許可リスト以外への新規接続は拒否される。
ルールは VM の外側にあるので、guest の root からは見えず改変もできない。
すべて非 root で動く。

```
host ── vsock ──────────────────────────────────────► guest (quagent __guest)
host ◄─ vsock (run ごとのポート、この VM の CID だけ) ◄─ guest 127.0.0.1:7070
                                                       (quagent.host: LLM / MCP)
        qemu ── tap0 ──► slirp4netns ──► host (uplink)
        └ 子 netns: nftables (許可した IP への TCP 80/443 以外を reject) + 自前 DNS
                     + Web の透明プロキシ (SNI/Host を許可名と照合。任意で TLS 終端して内容ガード)
```

host から VM の操作 (コマンド実行・端末・repo の受け渡し・PR 用の git fetch) は
vsock で行う。網を通らないので nft にも DNS にも関わらず、ssh も使わない。VM 内では
quagent 自身が作業ユーザーの権限で vsock を待ち受ける (run ごとに cloud-init の
seed で持ち込むので、quagent を更新してもイメージの焼き直しは要らない。接続は host
からのものだけ受ける)。
なお vsock では VM から host の vsock の待ち受けにも接続できる (ふつうの host
には無いが、vsock で待ち受けるサービスを動かしているなら VM から届く)。
sshd は既定で止める (systemd-ssh-generator が作る vsock / unix ソケットの sshd も
含めて mask し、止まっていなければ起動をやめる)。`quagent run --ssh` のときだけ、
使い捨ての鍵で `127.0.0.1` から人が入れるようにする (接続コマンドは承認コンソールに出る)。

## VM の中

- ベースイメージ: OS ごとのレシピから焼く (下記)。どれも rootless docker と
  opencode 入りで、rootful の docker デーモンは動かさない。
- ユーザー `agent` (sudo なし)。作業ディレクトリは `/work` で、ここに対象 repo を
  履歴ごと取り込み、host と同じブランチを checkout する。渡すのは checkout 中の
  ブランチと origin の remote-tracking だけで、ほかのローカルブランチやタグは渡さない。
  ブランチの先頭は既定では upstream (fetch 済みのもの) で、未 push のコミットは渡らない
  (upstream が無ければ起動しない)。ローカルの先頭で渡すなら `--local-head` (TUI の
  オプション)。未コミットの変更はどちらでも渡らない。
- VM は毎回ベースイメージの overlay から起動し、終了時に破棄する。強制終了で
  残った作業ディレクトリは、次に起動したときに掃除する。
- VM には CPU の仮想化支援 (svm / vmx) を見せないので、VM の中では KVM を使えない。
  VM の中で VM を動かすとき (quagent 自体の開発など) は `--nested-virt` (TUI のオプション)。
- `--mount-tmp` (TUI のオプション) で、host の `<repo>/.tmp` と VM の `/work/.tmp`
  を受け渡す (レポートなどのテキストを置く想定)。VM の `/work/.tmp` は専用の小さな
  ディスク (64 MiB、noexec) で、host のディレクトリは直接見せない。起動時に host の
  `.tmp` の通常ファイル (合計 32 MiB まで) を VM へコピーし、終了時に VM の中身を
  host の `.tmp` へ回収する。回収するのは通常ファイル (0644) とディレクトリだけで、
  リンク・特殊ファイル・実行権限は host に作らない。VM で消したファイルは host
  では消さない。回収できなければディスクのイメージをログに残す。`.tmp` に git で
  管理しているファイルがあれば受け渡さない。

## 使い方

```sh
make build                     # bin/quagent (VM に持ち込むので静的リンク)
make install                   # ~/.local/bin/quagent に入れる (PREFIX で変更可)
cd <repo> && quagent           # TUI: 起動設定 (repo・OS・CPU・メモリ) とベースイメージの管理
```

TUI を使わずに直接操作することもできる:

```sh
quagent image recipes          # 使えるレシピ (OS) の一覧
quagent image build [--refresh] arch   # ベースイメージを焼く (--refresh でクラウドイメージも取り直す)
quagent image ls / rm IMAGE    # 焼いたイメージの一覧・削除
quagent guard check "本文"      # ローカル LLM による内容点検を 1 件試す (下記)
quagent run --image arch       # VM を起動 (--ssh: 人が ssh で入れる、--mount-tmp: .tmp を受け渡す)
```

`quagent run` は tmux セッションを作り、上のペインで VM 内のエージェントを、
下のペインで承認コンソールを開く。エージェントは `--agent` (TUI でも選べる) で
`opencode` (既定、`--auto`) か `claude` (Claude Code、
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

許可は DNS で判定する。子 netns 内の DNS サーバーが許可ドメイン (完全一致か
`*.example.com`) の問い合わせだけを上流へ転送し、応答で見た IP だけを nft で
通す。通すのは HTTP/HTTPS の TCP 80/443 だけで、許可した行き先でもほかのポートへは
新規接続できない。許可外の名前は解決できず、外部 DNS への直接通信も遮断するので、DNS を
使った持ち出しもできない。応答に含まれる CNAME の先の名前は許可に含めない (その
応答の IP は通る)。許可したドメインでも、応答の IP が LAN・loopback・link-local
(クラウドのメタデータ)・CGNAT (Tailscale) などの内部向けなら通さない (DNS の
応答を操れる者に内側へ届かせないため。拒否は承認コンソールに出る)。

許可した IP への TCP 80/443 は、子 netns の中の透明プロキシを通す。接続先が実際に
言ってきた名前 (TLS の SNI、HTTP の Host) を読み、許可名と一致しなければ切る。許可は
IP 単位なので、これが無いと同じ IP を共有する別のホスト (共有 CDN) や、DNS を操って
任意の公開 IP を指した名前へ届いてしまう。名前を確かめられない接続 (SNI/Host が無い・
読めない) も切る。TLS は終端しないので中身は見ない。拒否は承認コンソールに出る。

host に必要なもの: `qemu-system-x86_64` (KVM)、`qemu-img`、`xorriso`、
`slirp4netns`、`unshare`/`nsenter`/`prlimit` (util-linux)、`nft`、`git`、`gh`、`tmux`。
unprivileged user namespace が有効で、vsock (`/dev/vhost-vsock`、カーネル
モジュール `vhost_vsock`) が使えること。`--ssh` を使うなら `ssh` / `ssh-keygen` も。
署名付きのレシピ (arch) でイメージを焼くなら `gpg` / `gpgv` も。

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

## 敵対的なエージェントへの備え

VM の中のエージェントが host の資源や承認者を使い潰せないよう、上限を設けている。

- 接続先の申請: 承認待ちは 1 件まで (決着前の次の申請は即座にエラー)。拒否・時間切れの
  ドメインは 10 分間再申請できない。1 回 20 ドメイン・理由 1000 文字まで
- 承認コンソール: VM 側が決められる文字列 (理由・DNS の名前・クリップボードの中身) は
  制御文字と向きを入れ替える文字を `\u` 表記にしてから表示する。拒否の記録は 1 分あたり
  の件数を絞る。表示が止まっても本体は止まらない (送信に時間制限)
- DNS: 同時に処理する問い合わせは 64 件まで。応答を覚える名前は 4096 個まで
- Web の名前: 許可した IP への TCP 80/443 は透明プロキシを通し、接続の先頭で
  TLS の SNI / HTTP の Host が許可名と一致するかを見る。読むのは 64KiB まで。
  確かめられない接続は切る (止まる側)。`inspect_https` なら TLS を終端して中身も
  内容ガードにかける (平文 HTTP も点検する)。同時接続は 64 本まで
- PR: タイトル 256 文字・本文 60000 バイト・1 回 500 コミット・1 run で 10 ブランチ・
  依頼は 10 秒おき。取り込みは 1 ファイル 2GiB まで、git は 10 分で打ち切る
- host から VM へのコマンド: 出力は 1MiB、10 分で打ち切る
- tmux: VM の出力による窓の名前の変更と、tmux を素通りする出力 (passthrough) を無効にする
- 窓口 (LLM プロキシ・MCP) は run ごとのトークンが要る。窓口は host の vsock で待ち受け、
  この run の VM 以外からの接続は切る。同時接続は 64 本まで。接続ごとに host で
  プロセスを起こさない
- 内容ガード: 承認コンソールへの確認は同時に 1 件、1 分に 12 件まで (溢れは拒否)。
  点検に渡す本文は max_bytes ごとの塊に分けて (少し重ねて) max_chunks 個まで。
  点検済みの塊は再点検しない (会話履歴が毎回送られても送り直さない)。人間の判断は
  「ローカル LLM が指摘した該当箇所」を単位に覚え、拒否した該当箇所は内容そのものを
  覚えて再送を止める (どちらも 10 分、512 件)

残っているもの: VM のディスク (overlay、最大 40G) には VM が書き込めるので、host の
ディスクを使える。LLM API の利用量 (課金) は制限していない。

## ベースイメージのレシピ

OS ごとの作り方は `internal/image/recipes/<名前>/` に独立して置いてある
(`recipe.json` にクラウドイメージの URL、`user-data.yaml` に焼き込みの
cloud-init)。今は `debian` と `arch`。`recipe.json` の `description` は起動メニューに
出す短い名前 (OS 名と版くらい)、`details` はイメージ管理の画面に出す中身の説明。`~/.config/quagent/images/<名前>/` に同じ
形で置けば、組み込みを差し替えたり別の OS を足したりできる。

取得したクラウドイメージは配布元のチェックサム (`checksum_url`、必須) と照合し、
署名があれば (`signature_url` と、レシピのディレクトリに置いた公開鍵 `signing_key`)
gpgv でその鍵だけを使って検証する。arch は arch-boxes の署名鍵 (arch-boxes の
README に載っている鍵) で検証する。debian は配布元が署名を出していないので、
cloud.debian.org から TLS で取ったチェックサムとの照合だけ。同梱の鍵の期限は
GitHub Actions (`signing-keys`) が毎週確かめ、60 日以内に切れるなら issue を立てる。

レシピの約束: ユーザー `{{.User}}` を uid 1000 で作り、rootless docker と opencode を入れ、
`/work` をそのユーザーの所有で作り、成功したら `{{.Marker}}` を `/dev/ttyS0` に
出して電源を切る。マーカーが出なければ焼き込みは失敗扱いになる。実行時の VM は
外向き通信がほぼ無いので、起動時にネットワーク (NTP など) を待つサービスは
止めておくこと (Arch では `systemd-time-wait-sync` が起動を止めていた)。

## LLM API の認証プロキシ

host の窓口 (LLM プロキシと MCP) は run ごとの合言葉 (トークン) を要求する。
トークンはエージェントの設定ファイル (権限 600) にだけ書くので、エージェントの
設定を読まないプロセスは窓口を使えない (VM 内の docker コンテナは、そもそも窓口に
経路が無い)。ただしエージェントと同じユーザーで動くプロセスは設定ファイルを読める。

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

Claude Code を使うときは `providers` に `anthropic` を入れる (VM 内の Claude Code は
`ANTHROPIC_BASE_URL` をプロキシに向け、鍵の代わりにトークンを `apiKeyHelper` で渡す):

```json
"anthropic": {
  "upstream": "https://api.anthropic.com",
  "header": "x-api-key", "prefix": "",
  "secret_command": ["pass", "show", "anthropic/api-key"]
}
```

サブスクリプション (Pro/Max) で使うときは、host で `claude setup-token` を実行して
長期 (1 年) のトークンを作り、それを秘密にして `claude.subscription` にプラン
(`pro` / `max` / `team` / `enterprise`。Claude Code の表示に使う) を書く。
`header` / `prefix` は指定しない (既定の `Authorization: Bearer <トークン>` で付ける)。
VM 内の Claude Code はトークンの代わりに合言葉を `CLAUDE_CODE_OAUTH_TOKEN` で持ち、
本物のトークンは VM に入らない:

```json
"providers": {
  "anthropic": {
    "upstream": "https://api.anthropic.com",
    "secret_command": ["pass", "show", "anthropic/claude-oauth-token"]
  }
},
"claude": { "subscription": "max" }
```

`setup-token` のトークンは推論専用なので、VM 内の `/usage` は使えない (利用量は
host の Claude Code の `/usage` で見る)。

host でログイン中の Claude Code の認証情報 (`~/.claude/.credentials.json`) は使わない
(短命で、更新すると host 側のログインが切れうるため)。

`claude.model` で VM 内の Claude Code の既定モデルを指定できる。カラーテーマは host の
Claude Code の設定 (`~/.claude/settings.json` の `theme`) を引き継ぐ。`claude.theme`
(`dark` / `light-daltonized` など) を書けばそちらを使う。

provider ID は opencode の provider ID と揃える。秘密の取り出し方は
`secret_env` (環境変数名)・`secret_file` (パス)・`secret_command` (コマンド) の
いずれか。ヘッダは既定で `Authorization: Bearer <秘密>` (`header` / `prefix` で変更可)。
秘密は run 開始時に一度だけ取り出す。`opencode.model` は VM 内 opencode の既定
モデルで、`providers` に挙げた provider のものを指定する。

プロキシが鍵を付けて転送するのは推論とモデル一覧の操作だけ (`POST /messages`・
`/messages/count_tokens`・`/chat/completions`・`/responses`、`GET /models`、それぞれ
`/v1` 付きも)。ファイルやバッチなどほかの API は 403 で断り、承認コンソールに出す。
足りなければ provider ごとに `"allow": ["POST /messages", "GET /models/*"]` のように
指定する (upstream からの相対パス。末尾の `*` は前方一致。指定すると既定は使わない)。

## 内容ガード (ローカル LLM)

接続先の許可制は「どのドメインへ出てよいか」しか見ないので、許可したドメインへ秘密や
個人情報を持ち出す要求は通ってしまう。たとえば User-Agent にメールアドレスを紛れ込ませれば、
許可済みの行き先へそのまま出てしまう。そこで任意で、LLM 認証プロキシを通るリクエストの
中身を手元のローカル LLM に点検させ、疑わしければその理由と中身を承認コンソールに出し、
人間が通すか止めるかを決められるようにする。

点検するのは host が平文で見られるリクエスト (認証プロキシ。`inspect_https` を有効に
すれば、TLS 終端した外向きの HTTPS と平文 HTTP も) だけ。行き先・メソッド・
ヘッダ (User-Agent など)・本文 (max_bytes ごとの塊) をローカル LLM に渡し、「外部へ
持ち出そうとしていないか」と、そう見たときは「どの部分を機密だと思ったか」を JSON で
判定させる。本文が JSON (Content-Type が JSON、または先頭が `{` / `[` で JSON として
読める) のときは、jq のように字下げしてから点検する (minify されたままでは小さなモデルに
どこに何があるか分かりにくい)。字下げは値やエスケープを変えないので、LLM が引用した
該当箇所は元の本文にもそのまま現れ、拒否した内容の再送判定も効く。LLM チャットは
会話履歴を丸ごと毎回送るので、点検済みの塊 (行き先とヘッダが同じもの) は再点検しない。
疑わしいとき・判定できないときは、既定で止まる側 (承認
コンソールで確認、時間切れなら拒否) に倒れる。ローカル LLM は間違えるので、許可制や
承認コンソールを置き換えるものではなく、追加の一枚として使う。

行き先は設定した `providers` の upstream を使う (エージェントが付けた `Host` ヘッダは
見せない)。`create_pull_request` のタイトル・本文も同じ判定に通すので、PR 経由で
ローカル LLM をすり抜けて持ち出すことはできない。

`inspect_https` を有効にすると、認証プロキシ以外の外向き通信も点検できる。許可した
行き先への接続を host 側で TLS 終端し (証明書は run ごとの使い捨て CA が署名する)、
平文になった HTTP のリクエストを同じローカル LLM にかけてから、本来のサーバーへ TLS で
張り直す。張り直すときも上流の証明書は system のルートで検証するので、終端したからと
いって検証は緩めない。疑わしければ認証プロキシと同じ承認コンソールで人間が通すか
止めるかを決め、拒否した該当箇所の記憶も認証プロキシと共有する。平文の HTTP (80) も
同じように点検する。使い捨て CA は cloud-init で guest の信頼ストアに入れ、Node / Bun に
は `NODE_EXTRA_CA_CERTS` で教える。使うには `enabled` も true にする。

### 用意する (llama.cpp + 6GB 級)

```sh
# llama.cpp の llama-server を OpenAI 互換で立てる。RTX 3050 6GB なら 3B〜4B 級 (Q4) が収まる
llama-server -m qwen2.5-3b-instruct-q4_k_m.gguf --port 8080 --alias qwen2.5-3b-instruct --jinja

# 思考 (reasoning) するモデル (Qwen3.5 など) を使うときは思考を切ってから点検に使う。
# 切らないと判定が reasoning_content 側へ出て content が空になり、点検が毎回失敗して
# on_error の扱いになる (既定の ask では毎回承認コンソールに出る)。
llama-server -m Qwen3.5-4B-Q4_K_M.gguf --port 8080 --alias qwen3.5-4b --jinja --reasoning off
```

`~/.config/quagent/config.json`:

```json
"guard": {
  "enabled": true,
  "backend": "openai",
  "endpoint": "http://127.0.0.1:8080",
  "model": "qwen2.5-3b-instruct",
  "timeout_seconds": 30,
  "max_bytes": 8192,
  "max_chunks": 8,
  "concurrency": 1,
  "mode": "ask",
  "on_error": "ask",
  "inspect_https": false
}
```

Ollama を使うなら `"backend": "ollama"` にする (既定 endpoint は `http://127.0.0.1:11434`、
モデルは `qwen2.5:3b` など)。

| フィールド | 意味 |
| --- | --- |
| `enabled` | 点検するか (既定 false) |
| `backend` | `openai` (既定。llama.cpp など OpenAI 互換) か `ollama` |
| `endpoint` | ローカル LLM の URL。既定 `http://127.0.0.1:8080` (llama.cpp) |
| `model` | 使うモデル。既定 `qwen2.5-3b-instruct` (llama.cpp は起動時の `--alias` と合わせる) |
| `timeout_seconds` | LLM 1 回 (塊 1 つ) の点検の上限。既定 30。分割点検全体は最大 10 分 |
| `max_bytes` | LLM に見せる本文の塊 1 つのバイト数。既定 8192、上限 32768 |
| `max_chunks` | 本文を分ける塊の数。既定 8、上限 64。塊は少し重ねてあり、境目にまたがる短い秘密もどれかの塊に丸ごと入る |
| `concurrency` | 同時に点検する件数。GPU 1 枚なら 1 (既定) |
| `mode` | 疑わしいとき。`ask` (既定。承認コンソールが決める) / `deny` (確認せず止める) / `advisory` (ログに残して通す) |
| `on_error` | 点検できなかったとき。`ask` (既定) / `deny` / `allow` |
| `inspect_https` | 許可した行き先への HTTPS (と平文 HTTP) を host 側で TLS 終端し、中身も点検する。既定 false。true には `enabled` が要る。使い捨て CA を guest の信頼ストアに入れるので、証明書を固定するクライアントとは相性が悪い |

`response_format` の対応はローカル LLM のビルドによって差がある。対応していなければ
`json_object`、それも駄目なら付けずに再試行し、通った形を覚える。llama.cpp は
`json_schema` に対応している (文法制約で JSON を強制する) ので、そのまま使える。

疑わしいリクエストは承認コンソールに行き先・ヘッダと、該当箇所の周辺の本文が出て、最後に
理由と、ローカル LLM が指摘した該当箇所 (何を機密だと思ったか) が出る。本文全体ではなく
該当箇所を中心に画面に収まるぶんだけ (前後数行、長い行は該当箇所の前後が見えるように横に
切る) を見せ、理由と該当箇所を最後に置く (本文が長くても上へ流れて読めなく
ならないようにするため)。`y` で通す / `n` で止める。時間切れ (既定 10 分) と `quit` は
止める側。判断は該当箇所ごとに覚えていて、同じ情報が会話履歴に何度現れても二度は聞かない。
拒否した該当箇所は内容そのものを覚え、本文やヘッダの置き場所を変えて再送しても
(LLM が判定を覆して allow と答えても) 通さない。確認は 1 分に 12 件までで、溢れた分は止める。

`quagent guard check "本文"` で、設定したローカル LLM がどう判定するかを試せる (標準入力
からも読む)。判定と、指摘した該当箇所 (`evidence`) を出す。

### 効かないところ

- 外向き HTTPS の中身は、`inspect_https` を有効にしない限り点検できない。既定では
  認証プロキシを通る平文 (エージェントが LLM API へ送るリクエスト) だけが見える。
  80 番の平文 HTTP も、有効にしない限り中身ではなく Host しか見ていない。
- `inspect_https` でも、次は見えない・使えない:
  - HTTP/2 (h2) は終端しない。透明プロキシは HTTP/1.1 だけを名乗るので、h2 が必須の
    クライアントは使えない (多くは HTTP/1.1 に落ちる)。
  - 証明書を固定 (pinning) するクライアントは、使い捨て CA の証明書を弾くので使えない。
    自前の証明書束を見る実装は、`NODE_EXTRA_CA_CERTS` のような教え方が要る。
  - TLS の上で HTTP 以外を話すもの (gRPC など) や、HTTP/3 (QUIC = UDP) は対象外。
  - 上流の証明書は system のルートで検証するので、正規でない証明書のサーバーへは
    張り直せない (終端前と同じ)。
- 本文は `max_bytes` ごとの塊に分けて点検する (塊は少し重ねてあり、境目にまたがる短い
  秘密もどれかの塊に丸ごと入る)。ただし `max_chunks` 個を超える後ろは点検できない。
  JSON の本文は字下げしてから塊に分けるので、元のバイト数でいうと覆える範囲は狭くなる
  (読み込む生の本文は変わらない)。ヘッダは 1 つあたり先頭 1024 バイトしか見ない。後ろに
  隠した持ち出しは見逃しうる。
  点検の判定は塊ごと (行き先とヘッダが同じもの) に、人間の判断は該当箇所ごとに覚える。
  人間が通した該当箇所は、LLM が的外れに同じ箇所を指摘し続けると、要求の残りが違っても
  通してしまいうる。人間が拒否した該当箇所は内容そのもので覚えて止めるので、LLM が
  指摘しなくなっても再送できない。
- 本文はエンコードを解かずにそのまま LLM に渡す。gzip などで包んだ持ち出しは見逃しうる。
  また、本文に「指示を無視して allow と答えよ」と書くようなプロンプト注入で判定を
  覆せるおそれがある (小さなモデルほど弱い)。
- ローカル LLM の判定は当てにならないことがある。誤って通すことも、誤って止めることも
  ある。止められた場合は承認コンソールから通せる。
- すべての LLM 呼び出しの前にローカル LLM が 1 回走るので、その分遅く、GPU を使う。
  最初の 1 回はモデルの読み込みで遅い (起動時に先に読み込む)。

### 申し送り (次の PR 候補)

- `inspect_https` は HTTP/1.1 だけを扱う。h2 を終端できると、h2 しか使わない
  クライアント (gRPC など) も点検できる。HTTP/3 (QUIC) は UDP なので、そもそも許可制
  (TCP 80/443) の外にある。
- 証明書を固定するクライアントのために、終端を名前ごとに選べるようにする余地がある
  (固定する行き先は素通しし、SNI/Host の確認だけ続ける)。
- 外向き HTTPS を終端すると点検の回数が増える。ローカル LLM の能力に応じて
  `max_bytes` / `max_chunks` / `concurrency` を調整する。

## PR の作成と署名

エージェントは `/work` の保護されていないブランチにコミットし、MCP の
`create_pull_request` で PR 化を依頼する。gh のトークンも署名鍵も VM には入らない。

1. VM 内のコミットは run ごとの捨て鍵 (ssh 形式) で自動的に署名される。これが
   「VM で作った」印になる
2. host が vsock 経由 (git の `ext::` 転送) で guest の `/work` からブランチを
   取り込む (run 専用の bare repo、hooks は無効、オブジェクトは host の repo を参照)
3. base に無いコミットのうち、捨て鍵の印があるものだけを host の git 設定の鍵
   (対象の repo で効いている `gpg.*` / `user.signingkey`。repo の `.git/config` や
   `includeIf` の設定も含む) で署名し直す (中身・author・メッセージは変えず、committer は利用者)。host の repo に
   既にあるコミット (他人のものや未 push のもの) はハッシュも署名も変えずにそのまま
   積む。印が無く host にも無いコミット (VM で署名を切って作ったもの) があれば拒否する。
   既に push したコミットは、guest に残っていれば内容で照合してそのまま使うので、
   PR の更新 (別の run からの追加コミットを含む) でハッシュは変わらない
4. host の repo の `origin` へ push し、`gh` で PR を作る (既にあれば更新)

ssh 署名で鍵をエージェントに置いている場合は、quagent を起動したときの
`SSH_AUTH_SOCK` を使う。ssh のエージェント転送先で使うときは、転送元の接続が切れると
以後の署名に失敗する (tmux に付け直しても quagent の環境は変わらない)。

push 先は `origin` で固定。保護ブランチ (既定: main / master / develop、
`config.json` の `pr.protected_branches` で変更可) には push しない。リモートに
同名のブランチが既にある場合、その先端までのコミットがすべて guest のコミットと
対応していれば (quagent が前に push した PR なら) 追加コミットを積める。対応しない
コミットがあれば書き換えない。コミットは利用者の名前で作られる (VM には host の
git の `user.name` / `user.email` だけを渡す。署名鍵や認証の設定は渡さない)。

VM に渡すのは既定で origin に公開済みのコミットだけなので、host にしか無い未 push の
コミットは PR に入らない。`--local-head` を付けると checkout 中のブランチの未 push の
コミットも VM に渡り、PR として origin へ公開されうる (上の 3 のとおり、host にある
コミットは署名し直さずにそのまま積む)。

`--pr-approval` (TUI では起動設定の「PR を承認制にする」) を付けると、host がブランチを
取り込んで push する前に、承認コンソールでブランチ・向き先・タイトル・本文を見せて
`y` / `n` を求める。`n` または 10 分応答が無ければ push も PR の作成もしない。承認が
あっても、PR ができたら GitHub で中身を確認する前提は変わらない。

## ライセンス

MIT
