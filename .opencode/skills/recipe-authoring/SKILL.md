---
name: quagent のベースイメージレシピ
description: quagent のベースイメージレシピ (internal/image/recipes/<名前>/ の recipe.json と user-data.yaml) を新しく足す・直すときに使う。OS ごとの焼き込み手順、クラウドイメージの取得と検証、レシピが満たすべき約束をまとめてある。
---

# quagent のベースイメージレシピ

quagent は OS ごとに「ベースイメージ」を焼く。手順はレシピ
(`internal/image/recipes/<名前>/`) として独立している。ここには、レシピを足す・
直すときに満たすべき約束と、押さえておくべき点を書く。使い方は
`README.md`、内部の仕組みは `docs/design.md` を参照。

## 置き場と読み込み

- 組み込み: `internal/image/recipes/<名前>/`
- 利用者: `~/.config/quagent/images/<名前>/` (同名なら組み込みより優先される)
- 1 つのレシピは `recipe.json` (取得方法) と `user-data.yaml` (焼き込みの
  cloud-init) からなる。
- `recipe.json` の `description` は起動メニューに出す短い名前 (OS 名と版くらい)、
  `details` はイメージ管理の画面に出す中身の説明にする (何をどこから入れるか)。

## recipe.json

| フィールド | 意味 |
| --- | --- |
| `description` | メニューに出す短い名前 (OS 名と版くらい) |
| `details` | 管理画面に出す中身の説明 |
| `cloud_image_url` | 出発点のクラウドイメージ (必須) |
| `latest_url` | 「今のファイル名」を載せた小さなテキスト (版ごとにファイル名が変わる配布元向け) |
| `checksum_url` | 配布元のチェックサム一覧 (必須。sha256 か sha512 の `<16進> <ファイル名>` の行) |
| `signature_url` / `signing_key` | イメージの分離署名 (OpenPGP) と、レシピのディレクトリに置く公開鍵のファイル名。両方そろって指定する |
| `firmware` | 焼く VM の起動ファームウェア。`bios` (既定・SeaBIOS) か `uefi` (OVMF) |
| `build_timeout_minutes` | 焼き込み VM の上限 (分)。既定 45。カーネルを作り直すレシピ向け |

## URL の $FILE 置換 (版ごとにファイル名が変わる配布元)

Gentoo のクラウドイメージはファイル名にタイムスタンプが入り URL を固定できない。
そこで `latest_url` に「今のファイル名を載せた小さなテキスト」を指定し、
`cloud_image_url` / `checksum_url` / `signature_url` の中の `$FILE` をその名前で
置き換える。取得のたびに最新のファイル名を解決するので、配布元が更新されても
URL は古くならない。

## 取得の検証

- 取得したクラウドイメージは配布元のチェックサム (`checksum_url`、必須) と照合する。
- 署名があれば (`signature_url` と、レシピのディレクトリに置いた公開鍵
  `signing_key`) `gpgv` でその鍵だけを使って検証する (利用者の鍵束は使わない)。
  - arch は arch-boxes の署名鍵 (arch-boxes の README に載っている鍵)。
  - gentoo は Release Engineering の署名鍵 (`gentoo-release.asc`。weekly key の
    署名 subkey) でイメージの分離署名を検証し、チェックサムは配布元の
    clearsigned な `.sha256` を使う。
  - debian は配布元が署名を出していないので、cloud.debian.org から TLS で取った
    チェックサムとの照合だけ。
- 同梱の鍵の期限は GitHub Actions (`signing-keys`) が毎週確かめ、60 日以内に
  切れるなら issue を立てる (`.github/scripts/check-signing-keys.sh`)。

## firmware

`firmware` は焼く VM の起動ファームウェア。既定は `bios` (SeaBIOS)。配布イメージが
UEFI 専用のとき (Gentoo のクラウドイメージなど) は `uefi` にして OVMF で起動する
(ホストに OVMF が要る)。焼いたイメージには起動方法を付帯情報 (`base-*.json`) と
して残すので、run はレシピを後から変えてもイメージに合った方法で起動する。

## レシピの約束

user-data は `{{.User}}` (作業ユーザー名) と `{{.Marker}}` (成功のマーカー) を
quagent が埋めてから cloud-init に渡す。次を満たすこと。

- ユーザー `{{.User}}` を uid 1000 で作る (sudo は付けない)。
- rootless docker と opencode を入れ、`git` も入れる (`git` は repo の取り込みと
  PR の fetch が使う)。rootful の docker デーモンは動かさない。
- `/work` をそのユーザーの所有で作る。
- 成功したら `{{.Marker}}` を `/dev/ttyS0` に出して電源を切る。マーカーが出なければ
  焼き込みは失敗扱いになる。
- 実行時の VM は外向き通信がほぼ無いので、起動時にネットワーク (NTP など) を
  待つサービスは止めておく (Arch では `systemd-time-wait-sync` が起動を止めていた)。
- 焼き込みに使ったキャッシュ (Portage の作業場・distfiles・binhost など) や、
  実行時に使わない焼き込み専用の依存をイメージに残さない。
- Gentoo の cloud-init は `packages:` で emerge を扱わない。焼き込みは自前の
  スクリプト (`write_files` + `runcmd`) で行う。

## 差分更新 (--incremental)

`--incremental` は前回焼いたイメージを出発点にして、イメージで使うパッケージ
(slirp4netns・fuse-overlayfs など) を更新する。**ベースシステム全体 (@world) は
更新しない**。配布イメージの base はリリースエンジニアリングが焼いた古い stage で、
更新すると Rust/clang/LLVM のような大きなビルドを呼ぶことがあるため。
base ごと更新したくなったら `--refresh` でクラウドイメージから焼き直す。

カーネルは新しい版が入ったときだけ作り直す (`emerge --update` が何もしなければ
数秒で終わる)。**fragment を変えても差分更新ではカーネルを建て直さない**ので、
硬化設定を変えたら `--refresh` で焼き直す。

## Gentoo の硬化カーネル

Gentoo のレシピは、公式の cloud image (`di-amd64-cloudinit`) を出発点に、
hardened プロファイル (`no-multilib/systemd`) に切り替え、配布カーネル
(`sys-kernel/gentoo-kernel`) を `USE=hardened` と config fragment
(`/etc/kernel/config.d/*.config`) で硬化し直し、起動オプション (GRUB) にも硬化を
入れる。配布イメージは UEFI 専用なので `firmware` を `uefi` にする。

- config fragment は `/etc/kernel/config.d/` に置く。`50-` は rootless docker 用、
  `90-quagent-hardening.config` は硬化用。
- カーネルの版でシンボルが改名・廃止される (`PAGE_TABLE_ISOLATION` は 6.8 で
  `MITIGATION_PAGE_TABLE_ISOLATION` になった等)。`merge_config` は警告するだけで
  無視するため、焼き込みの最後に `verify_config` が fragment の各設定行を、今
  ビルドした版の config (`/usr/src/linux-<ver>/.config`、無ければ
  `/usr/src/linux/.config`) と突き合わせ、ずれがあれば焼き込みを失敗させる。
  これは焼くカーネルの設定が意図どおりかを見るだけで、起動そのものは確かめない
  (最初の起動は run で行う)。fragment を直すときは、ここが通る (実在するシンボルと
  値にする) ようにする。
- ジョブは `recipe.json` の `build_timeout_minutes` (既定 45、gentoo は 240) で
  打ち切られる。カーネルの作り直しはコアが多いほど速い。

## gentoo の焼き込みスクリプトの要点

- 焼き込みの出力はファイルにいったん書き、別プロセスの `tail` で serial へ流す。
  シリアル getty が ttyS0 を初期化し直すと、直接流していた出力が消えて失敗しても
  原因が残らないため、getty は焼き込みの前に止める。失敗時はログの末尾を `ttyS0`
  へ直接流してから電源を切る。
- 焼き込みのあいだだけ `kernel.yama.ptrace_scope` を 1 にする。git 2.52 以降の
  Rust 部品 (cargo) は `PTRACE_TRACEME` を使い、硬化設定の `ptrace_scope=2` は
  これを拒否してビルドが落ちる。sysctl.d の設定自体は触らないので、起動し直した
  イメージでは 2 に戻る。
- クラウドイメージには Portage ツリーが入っていない (`make.profile` は
  `/var/db/repos/gentoo` を指しているだけ)。`emerge-webrsync` でツリーを取ってから
  プロファイルを切り替える。
- rootless docker の部品 (slirp4netns・fuse-overlayfs) は `~amd64` なので鍵を
  自動で足す (`--autounmask-continue`)。
