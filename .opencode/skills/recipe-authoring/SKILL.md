---
name: quagent のベースイメージレシピ
description: quagent のベースイメージレシピ (internal/image/recipes/<名前>/ の recipe.json と user-data.yaml) を新規追加・修正する際に使用します。OS ごとのイメージ構築手順、クラウドイメージの取得と署名検証、レシピが満たすべき規約についてまとめています。
---

# quagent のベースイメージレシピ

quagent は OS ごとに「ベースイメージ」をビルドして利用します。各 OS の環境構築手順はレシピ（`internal/image/recipes/<名前>/`）として独立して定義されています。本書では、レシピを新規追加または修正する際に満たすべき要件と注意点を解説します。基本的な使い方は `README.md`、内部アーキテクチャは `docs/design.md` を参照してください。

## 配置場所と読み込み順序

- 組み込みレシピ: `internal/image/recipes/<名前>/`
- ユーザー定義レシピ: `~/.config/quagent/images/<名前>/`（同名レシピが存在する場合は組み込みより優先されます）
- 1 つのレシピは `recipe.json`（イメージ取得・検証設定）と `user-data.yaml`（イメージ構築用の cloud-init 設定）で構成されます。
- `recipe.json` の `description` には起動メニューに表示する簡潔な名称（OS 名やバージョン等）を、`details` にはイメージ管理画面に表示する詳細説明（パッケージ構成や取得元など）を記述します。

## recipe.json の仕様

| フィールド | 意味 |
| --- | --- |
| `description` | 起動メニューに表示する簡潔な名称（OS 名やバージョン等） |
| `details` | 管理画面に表示する詳細説明 |
| `cloud_image_url` | ベースとなる上流クラウドイメージの URL（必須） |
| `latest_url` | 最新のファイル名が記載されたテキストファイルの URL（リリースごとにファイル名が変動するディストリビューション向け） |
| `checksum_url` | 配布元のチェックサム一覧ファイルの URL（必須。sha256 または sha512 の `<ハッシュ値> <ファイル名>` 形式） |
| `signature_url` / `signing_key` | イメージの分離署名（OpenPGP）の URL と、レシピディレクトリ内に配置する公開鍵ファイル名。両方をセットで指定 |
| `firmware` | ビルド用 VM の起動ファームウェア。`bios`（既定・SeaBIOS）または `uefi`（OVMF） |
| `build_timeout_minutes` | ビルド用 VM のタイムアウト上限（分）。既定値は 45。カーネルを再構築するレシピ等で調整 |

## URL の $FILE 置換 (ファイル名が動的に変わる配布元)

Gentoo などのクラウドイメージはファイル名にビルド日時タイムスタンプが含まれ、URL を静的に固定できません。そのため、`latest_url` に「最新ファイル名が記載されたテキストファイル」の取得元を指定し、`cloud_image_url` / `checksum_url` / `signature_url` 内の `$FILE` をその解決されたファイル名で置換します。ビルドのたびに最新ファイル名を取得するため、配布元の更新によって URL が無効化されるのを防ぐことができます。

## ダウンロードの検証

- 取得したクラウドイメージは、配布元のチェックサム（`checksum_url`、必須）と照合・検証されます。
- 署名が設定されている場合（`signature_url` およびレシピディレクトリ内の公開鍵 `signing_key`）、`gpgv` を用いて指定の公開鍵のみで署名を検証します（ユーザー環境のキーリングは参照しません）。組み込みレシピではすべて署名検証を行います:
  - Arch Linux: arch-boxes の署名鍵（arch-boxes の README に記載の鍵）。
  - Gentoo: Release Engineering の署名鍵（`gentoo-release.asc`。weekly key の署名 subkey）でイメージの分離署名を検証し、チェックサムは配布元のクリア署名付き `.sha256` を使用します。
- レシピに同梱された署名鍵の有効期限は GitHub Actions ワークフロー（`signing-keys`）により毎週自動検証され、有効期限まで 60 日を切った場合は自動で issue が起票されます（`.github/scripts/check-signing-keys.sh`）。

## 起動ファームウェア (firmware)

`firmware` はビルド用 VM の起動ファームウェアを指定します。既定値は `bios`（SeaBIOS）です。配布イメージが UEFI 専用である場合（Gentoo のクラウドイメージ等）は `uefi` を指定し、OVMF 経由で起動します（ホスト側に OVMF パッケージが必要です）。ビルドされたベースイメージには起動方式がメタデータ（`base-*.json`）として記録されるため、後からレシピの定義を変更した場合でも、イメージに適したファームウェアで起動されます。

## レシピが満たすべき要件・規約

`user-data.yaml` は、quagent が `{{.User}}`（作業ユーザー名）および `{{.Marker}}`（成功完了マーカー）を展開した上で cloud-init に渡します。レシピは以下の規約を満たす必要があります:

- ユーザー `{{.User}}` を UID 1000 で作成する（sudo 権限は付与しない）。
- rootless Docker、opencode、`git` をインストールする（`git` はリポジトリの同期および PR 作成時の fetch で使用されます）。root 権限の Docker デーモンは起動しない。
- `/work` ディレクトリを作成し、所有者を当該ユーザーに設定する。
- 正常終了時は `{{.Marker}}` を `/dev/ttyS0` に出力した上でシステムをシャットダウン（電源オフ）する。マーカーが出力されない場合、ビルドは失敗として扱われます。
- 実行時の VM は外部ネットワーク通信が遮断されているため、起動時にネットワーク同期（NTP 等）を待機するシステムサービスは無効化しておく（例: Arch Linux では `systemd-time-wait-sync` が起動をブロックする要因となっていました）。
- ビルド時に使用した一時キャッシュ（Portage の作業ディレクトリ、distfiles、binhost 等）や、実行時に不要なビルド専用の依存パッケージはイメージ内に残さず削除する。
- Gentoo の cloud-init は `packages:` による emerge パッケージ管理に対応していないため、ビルド処理は独自スクリプト（`write_files` + `runcmd`）経由で実行する。

## 差分更新 (--incremental) の仕様

`--incremental` オプションは、前回ビルドしたイメージを出発点として、イメージ内で利用するパッケージ（slirp4netns、fuse-overlayfs 等）を差分更新します。**ベースシステム全体（@world）のアップデートは行いません**。配布クラウドイメージのベース環境は古いステージでビルドされていることが多く、全体を更新すると Rust / Clang / LLVM などの大規模なコンパイルが発生するリスクがあるためです。ベース環境を含めて全体を刷新したい場合は、`--refresh` を指定してクラウドイメージから再ビルドを行ってください。

カーネルは新しいバージョンが提供された場合のみ再構築されます（`emerge --update` で変更がない場合は数秒で完了します）。**config fragment を変更した場合でも差分更新ではカーネルの再構築は行われない**ため、セキュリティ強化設定を変更した場合は `--refresh` で再ビルドしてください。

## Gentoo のセキュリティ強化カーネル

Gentoo レシピは、公式クラウドイメージ（`di-amd64-cloudinit`）をベースとし、hardened プロファイル（`no-multilib/systemd`）へ切り替えた上で、ディストリビューションカーネル（`sys-kernel/gentoo-kernel`）を `USE=hardened` および config fragment（`/etc/kernel/config.d/*.config`）で強化し、GRUB 起動パラメータにもセキュリティ強化設定を追加します。配布イメージが UEFI 専用であるため、`firmware` は `uefi` に設定します。

- config fragment は `/etc/kernel/config.d/` 配下に配置します。`50-` は rootless Docker 用、`zz-quagent-hardening.config` はセキュリティ強化用です。`config.d` 内の設定は辞書順にマージされ後勝ちとなるため、配布元の `dist-amd64-livecd.config` よりも後に適用されるファイル名としています。
- 不要なデバイスドライバは fragment で無効化します（攻撃面の最小化）。例えば `CONFIG_ETHERNET=n` を指定することで、ベンダー固有のイーサネットドライバを一括して無効化できます。`=n` の設定行も `verify_config` により整合性が検証されます。ただし、他のシンボルが `select` により強制的に `y` に戻すケースがあるため注意が必要です（Linux 6.18 では iSCSI / FCoE オフロードの 3 項目が `select ETHERNET` を行い、USB 周辺機器の 18 項目が `select USB` を行います）。これらを無効化する場合は `select` 元の機能も併せて停止します。
- `io_uring` は Linux 6.18 時点ではカーネル CONFIG による無効化ができません（`CONFIG_IO_URING=n` に指定しても `y` に戻ります）。そのため、sysctl の `kernel.io_uring_disabled=2` により無効化します。
- カーネルのバージョンアップに伴い、シンボルの改名や廃止が発生します（例: `PAGE_TABLE_ISOLATION` は Linux 6.8 で `MITIGATION_PAGE_TABLE_ISOLATION` に改名されました）。`merge_config` は未認識のシンボルを警告のみで無視するため、ビルド完了時に `verify_config` が fragment 内の各設定行と実際にビルドされたカーネル設定（`/usr/src/linux-<ver>/.config`、存在しない場合は `/usr/src/linux/.config`）を突き合わせ、差異が検出された場合はビルドを失敗として扱います。これは設定が意図通りに反映されているかを検証するものであり、起動自体の可否は `quagent run` 時に検証されます。fragment を修正する際は、この検証を通過する（実在するシンボルおよび有効な値を指定する）必要があります。
- ビルド処理は `recipe.json` の `build_timeout_minutes`（既定値: 45 分、Gentoo は 240 分）でタイムアウト打ち切りとなります。カーネルのコンパイル処理は CPU コア数が多いほど高速に完了します。

## Gentoo ビルドスクリプトのポイント

- ビルド出力はいったんファイルへ記録し、別プロセスの `tail` 経由でシリアルコンソールへ出力します。シリアル getty が `ttyS0` を再初期化する際に直接出力していたログが消失し、失敗原因が特定できなくなる事故を防ぐため、getty はビルド開始前に停止します。失敗時はログ末尾を `ttyS0` へ直接出力してからシャットダウンします。
- ビルド実行中のみ一時的に `kernel.yama.ptrace_scope` を 1 に設定します。Git 2.52 以降の Rust コンポーネント（cargo）が `PTRACE_TRACEME` を使用するため、セキュリティ強化設定の `ptrace_scope=2` がこれを拒否してビルドが失敗するのを防止するためです。`sysctl.d` の設定自体は変更しないため、ビルド完了後のイメージ起動時には値が 2 に戻ります。
- クラウドイメージには Portage ツリーが含まれていません（`make.profile` が `/var/db/repos/gentoo` を参照しているのみ）。`emerge-webrsync` でツリーを取得してからプロファイルの切り替えを行います。
- rootless Docker の依存コンポーネント（slirp4netns、fuse-overlayfs）は `~amd64` キーワード指定が必要なため、キーワード自動解除（`--autounmask-continue`）を付与してインストールします。
