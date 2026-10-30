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

配布元によってはクラウドイメージのファイル名にビルド日時タイムスタンプが含まれ、URL を静的に固定できません。そのため、`latest_url` に「最新ファイル名が記載されたテキストファイル」の取得元を指定し、`cloud_image_url` / `checksum_url` / `signature_url` 内の `$FILE` をその解決されたファイル名で置換します。ビルドのたびに最新ファイル名を取得するため、配布元の更新によって URL が無効化されるのを防ぐことができます。

## ダウンロードの検証

- 取得したクラウドイメージは、配布元のチェックサム（`checksum_url`、必須）と照合・検証されます。
- 署名が設定されている場合（`signature_url` およびレシピディレクトリ内の公開鍵 `signing_key`）、`gpgv` を用いて指定の公開鍵のみで署名を検証します（ユーザー環境のキーリングは参照しません）。組み込みレシピの Arch Linux は arch-boxes の署名鍵（arch-boxes の README に記載の鍵）で検証します。
- レシピに同梱された署名鍵の有効期限は GitHub Actions ワークフロー（`signing-keys`）により毎週自動検証され、有効期限まで 60 日を切った場合は自動で issue が起票されます（`.github/scripts/check-signing-keys.sh`）。

## 起動ファームウェア (firmware)

`firmware` はビルド用 VM の起動ファームウェアを指定します。既定値は `bios`（SeaBIOS）です。配布イメージが UEFI 専用である場合は `uefi` を指定し、OVMF 経由で起動します（ホスト側に OVMF パッケージが必要です）。ビルドされたベースイメージには起動方式がメタデータ（`base-*.json`）として記録されるため、後からレシピの定義を変更した場合でも、イメージに適したファームウェアで起動されます。

## レシピが満たすべき要件・規約

`user-data.yaml` は、quagent が `{{.User}}`（作業ユーザー名）および `{{.Marker}}`（成功完了マーカー）を展開した上で cloud-init に渡します。レシピは以下の規約を満たす必要があります:

- ユーザー `{{.User}}` を UID 1000 で作成する（sudo 権限は付与しない）。
- rootless Docker、opencode、`git` をインストールする（`git` はリポジトリの同期および PR 作成時の fetch で使用されます）。root 権限の Docker デーモンは起動しない。
- rootless Docker の storage driver は `fuse-overlayfs` に固定する（`~/.config/docker/daemon.json` に `{"features":{"containerd-snapshotter":false},"storage-driver":"fuse-overlayfs"}` を置き、起動時にその driver になっていることを確かめる）。linux-hardened は非特権 user namespace での overlayfs マウントを許可しないため、Docker 29 以降の既定（containerd image store の overlayfs snapshotter）では複数レイヤーのイメージ展開が `operation not permitted` で失敗する。
- `/work` ディレクトリを作成し、所有者を当該ユーザーに設定する。
- 正常終了時は `{{.Marker}}` を `/dev/ttyS0` に出力した上でシステムをシャットダウン（電源オフ）する。マーカーが出力されない場合、ビルドは失敗として扱われます。
- 実行時の VM は外部ネットワーク通信が遮断されているため、起動時にネットワーク同期（NTP 等）を待機するシステムサービスは無効化しておく（例: Arch Linux では `systemd-time-wait-sync` が起動をブロックする要因となっていました）。
- ビルド時に使用した一時キャッシュ（パッケージマネージャーのキャッシュ等）や、実行時に不要なビルド専用の依存パッケージはイメージ内に残さず削除する。

## ビルドは常に焼き直し

`quagent image build` は前回のイメージを引き継がず、毎回レシピのクラウドイメージから新しいイメージを焼きます。そのためレシピの変更は常に反映されます。`--refresh` は取得済みのクラウドイメージのキャッシュを無視して取り直したい場合に使います。ビルド処理は `recipe.json` の `build_timeout_minutes`（既定値: 45 分）でタイムアウト打ち切りとなります。

## Arch Linux のセキュリティ強化カーネル (linux-hardened)

Arch Linux レシピは、公式パッケージの `linux-hardened` を導入し、GRUB 起動パラメータ（`slab_nomerge init_on_alloc=1 init_on_free=1 ...`）によりセキュリティを強化します。
- `linux-hardened` は攻撃面低減のために非特権 user namespace が既定で無効化（`kernel.unprivileged_userns_clone = 0`）されているため、rootless Docker のために `/etc/sysctl.d/99-quagent-hardened.conf` で `1` に設定します。
- 焼き込み用 VM は標準カーネルで起動しているため、ビルド途中に稼働中カーネルのモジュールが削除されないよう、`linux-hardened` を追加インストールして Docker などのセットアップを完了させた後、ビルドの最終段階で旧 `linux` パッケージを削除し `grub-mkconfig` を実行してブート設定を更新します。

