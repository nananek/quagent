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
host ◄─ unix socket ◄─ qemu guestfwd ◄──────────────── guest (quagent.host: LLM / MCP)
        qemu ── tap0 ──► slirp4netns ──► host (uplink)
        └ 子 netns: nftables (許可した名前の IP 以外を reject) + 自前 DNS
```

host から VM の操作 (コマンド実行・端末・repo の受け渡し・PR 用の git fetch) は
vsock で行う。網を通らないので nft にも DNS にも関わらず、ssh も使わない。VM 内では
quagent 自身が作業ユーザーの権限で vsock を待ち受ける (run ごとに cloud-init の
seed で持ち込むので、quagent を更新してもイメージの焼き直しは要らない)。
なお vsock では VM から host の vsock の待ち受けにも接続できる (ふつうの host
には無いが、vsock で待ち受けるサービスを動かしているなら VM から届く)。
sshd は既定で止める。`quagent run --ssh` のときだけ、使い捨ての鍵で
`127.0.0.1` から人が入れるようにする (接続コマンドは承認コンソールに出る)。

## VM の中

- ベースイメージ: OS ごとのレシピから焼く (下記)。どれも rootless docker と
  opencode 入りで、rootful の docker デーモンは動かさない。
- ユーザー `agent` (sudo なし)。作業ディレクトリは `/work` で、ここに対象 repo を
  履歴ごと取り込み、host と同じブランチを checkout する (未コミットの変更は渡らない)。
- VM は毎回ベースイメージの overlay から起動し、終了時に破棄する。強制終了で
  残った作業ディレクトリは、次に起動したときに掃除する。
- `--mount-tmp` (TUI のオプション) で、host の `<repo>/.tmp` を VM の
  `/work/.tmp` に 9p で読み書き可能にマウントする。ここだけは VM から host に
  書き込めるので、中身を host で実行するときは気をつける。`.tmp` に git で管理して
  いるファイルがあればマウントしない (VM の checkout が host に書き込むため)。
  qemu は入れ子の userns で動かし、host の利用者を VM の作業ユーザー (uid 1000) に
  読み替えるので、ファイルの持ち主は双方で揃う。

## 使い方

```sh
CGO_ENABLED=0 go build -o bin/quagent ./cmd/quagent   # VM に持ち込むので静的リンクにする
cd <repo> && quagent           # TUI: 起動設定 (repo・OS・CPU・メモリ) とベースイメージの管理
```

TUI を使わずに直接操作することもできる:

```sh
quagent image recipes          # 使えるレシピ (OS) の一覧
quagent image build [--refresh] arch   # ベースイメージを焼く (--refresh でクラウドイメージも取り直す)
quagent image ls / rm IMAGE    # 焼いたイメージの一覧・削除
quagent run --image arch       # VM を起動 (--ssh: 人が ssh で入れる、--mount-tmp: .tmp をマウント)
```

`quagent run` は tmux セッションを作り、上のペインで VM 内のエージェントを、
下のペインで承認コンソールを開く。エージェントは `--agent` (TUI でも選べる) で
`opencode` (既定、`--auto`) か `claude` (Claude Code、
`--dangerously-skip-permissions`) を選ぶ。VM という檻の中では確認なしで動かす。
エージェントのペインを終了するか、承認コンソールで `quit` すると VM を破棄する。
デタッチしてもセッションが続くあいだ VM は動き続ける。

## 接続先の申請 (MCP)

VM からの外向き通信は既定でゼロ。エージェントは MCP (`http://quagent.host/mcp`)
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
通す。許可外の名前は解決できず、外部 DNS への直接通信も遮断するので、DNS を
使った持ち出しもできない。

host に必要なもの: `qemu-system-x86_64` (KVM)、`qemu-img`、`xorriso`、
`slirp4netns`、`unshare`/`nsenter` (util-linux)、`nft`、`socat`、`git`、`gh`、`tmux`。
unprivileged user namespace が有効で、vsock (`/dev/vhost-vsock`、カーネル
モジュール `vhost_vsock`) が使えること。`--ssh` を使うなら `ssh` / `ssh-keygen` も。

## ベースイメージのレシピ

OS ごとの作り方は `internal/image/recipes/<名前>/` に独立して置いてある
(`recipe.json` にクラウドイメージの URL、`user-data.yaml` に焼き込みの
cloud-init)。今は `debian` と `arch`。`~/.config/quagent/images/<名前>/` に同じ
形で置けば、組み込みを差し替えたり別の OS を足したりできる。

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

API キーは VM に入れない。VM 内の opencode は `http://quagent.host/llm/<provider>`
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

`claude.model` で VM 内の Claude Code の既定モデルを指定できる。

provider ID は opencode の provider ID と揃える。秘密の取り出し方は
`secret_env` (環境変数名)・`secret_file` (パス)・`secret_command` (コマンド) の
いずれか。ヘッダは既定で `Authorization: Bearer <秘密>` (`header` / `prefix` で変更可)。
秘密は run 開始時に一度だけ取り出す。`opencode.model` は VM 内 opencode の既定
モデルで、`providers` に挙げた provider のものを指定する。

## PR の作成と署名

エージェントは `/work` の保護されていないブランチにコミットし、MCP の
`create_pull_request` で PR 化を依頼する。gh のトークンも署名鍵も VM には入らない。

1. VM 内のコミットは run ごとの捨て鍵 (ssh 形式) で自動的に署名される。これが
   「VM で作った」印になる
2. host が vsock 経由 (git の `ext::` 転送) で guest の `/work` からブランチを
   取り込む (run 専用の bare repo、hooks は無効、オブジェクトは host の repo を参照)
3. base に無いコミットのうち、捨て鍵の印があるものだけを host の git 設定の鍵で
   署名し直す (中身・author・メッセージは変えず、committer は利用者)。host の repo に
   既にあるコミット (他人のものや未 push のもの) はハッシュも署名も変えずにそのまま
   積む。印が無く host にも無いコミット (VM で署名を切って作ったもの) があれば拒否する。
   既に push したコミットは書き換えないので、PR の更新でハッシュは変わらない
4. host の repo の `origin` へ push し、`gh` で PR を作る (既にあれば更新)

push 先は `origin` で固定。保護ブランチ (既定: main / master / develop、
`config.json` の `pr.protected_branches` で変更可) には push しない。リモートに
同名のブランチが既にあれば上書きしない。コミットは利用者の名前で作られる (VM には
host の git の `user.name` / `user.email` だけを渡す。署名鍵や認証の設定は渡さない)。

## 現状

今後: PR 作成の承認制 (任意)。Claude Code はサブスクリプションのログインには
未対応 (API キーのみ)。

## ライセンス

MIT
