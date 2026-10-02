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

## 現状

最小構成のみ。暫定で opencode の API ドメインへの 443 だけを固定で許可している。
今後: MCP による接続先申請と承認 UI (tmux)、host 側での認証付与プロキシ、
PR の作成と署名、起動時 TUI。
