# quagent

プロジェクトごとに使い捨ての qemu VM を立て、外向き通信を **host 側で** 制限した
うえでコーディングエージェントを動かすためのツール。

## 隔離の仕組み

qemu は unprivileged な user/mount/net namespace の中で動く。qemu が外へ張る
socket はその netns の nftables を通り、許可リスト以外への新規接続は拒否される。
ルールは VM の外側にあるので、guest の root からは見えず改変もできない。
すべて非 root で動く。

```
host ── slirp4netns add_hostfwd ──► 子 netns (qemu hostfwd) ──► guest:22
        qemu ── tap0 ──► slirp4netns ──► host (uplink)
        └ nftables (子 netns): DNS と許可リスト以外を reject
```

## VM の中

- ベースイメージ: Debian 13 + rootless docker + opencode。docker の rootful
  デーモンはマスクしてある。
- ユーザー `agent` (sudo なし)。作業ディレクトリは `/work` で、ここに対象 repo を
  履歴ごと clone する (未コミットの変更は渡らない)。
- VM は毎回ベースイメージの overlay から起動し、終了時に破棄する。

## 使い方

```sh
go build -o bin/quagent ./cmd/quagent
bin/quagent image build        # ベースイメージを焼く (時々やり直して更新する)
cd <repo> && quagent run       # VM を起動して /work に入る。exit で破棄
```

host に必要なもの: `qemu-system-x86_64` (KVM)、`qemu-img`、`xorriso`、
`slirp4netns`、`unshare`/`nsenter` (util-linux)、`nft`、`ssh`、`git`。
unprivileged user namespace が有効であること。

## LLM API の認証プロキシ

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
  }
}
```

provider ID は opencode の provider ID と揃える。秘密の取り出し方は
`secret_env` (環境変数名)・`secret_file` (パス)・`secret_command` (コマンド) の
いずれか。ヘッダは既定で `Authorization: Bearer <秘密>` (`header` / `prefix` で変更可)。
秘密は run 開始時に一度だけ取り出す。

## 現状

今後: MCP による接続先申請と承認 UI (tmux)、PR の作成と署名、起動時 TUI。

## ライセンス

MIT
