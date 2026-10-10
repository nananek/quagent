# 開発ガイドライン

本書は quagent 本体の開発者向けの開発指針およびコーディング規約です。基本的な使い方については [README](../README.md)、内部アーキテクチャや脅威モデルについては [design.md](design.md)、ベースイメージのレシピ作成については skill `recipe-authoring`、ドキュメントの構成・整理については skill `document-authoring` を参照してください。

## ビルドとテスト

```sh
make build                     # bin/quagent (VM 内にも持ち込むため静的リンク、CGO_ENABLED=0 でビルド)
make install                   # ~/.local/bin/quagent にインストール (PREFIX で変更可)
make test                      # go test ./...
make vet                       # go vet ./...
```

CI (`.github/workflows/ci.yml`) では push および PR 作成のたびに、コード整形・`go.mod`/`go.sum` の整合性・vet・テスト・静的リンクでのビルド検証が実行されます。PR を提出する前に、ローカル環境でも以下のチェックをすべて通過することを確認してください:

```sh
test -z "$(gofmt -l .)"
go mod tidy -diff
go vet ./...
go test ./...
make build
```

依存関係の更新は Dependabot により週 1 回自動でまとめられます。サプライチェーン攻撃等のリスクを低減するため、新規リリース直後のパッケージを即座に取り込まず、公開から 7 日間経過した安定版のみを取り込むよう設定されています。

## リポジトリ構成

- `cmd/quagent` … CLI および TUI のエントリポイント、VM 実行制御 (`run.go`、`guestops.go`、`skills.go` など)
- `internal/image` … ベースイメージのビルドおよび管理（レシピは `recipes/<名前>/`）
- `internal/vm` … QEMU 起動コンポーネント（seed ISO 作成、overlay、起動パラメータ構築）
- `internal/netns` … 子 netns 内の nftables 制御、自前 DNS、透過プロキシ、TLS 終端
- `internal/sandbox` … VM 内部の追加防御層（seccomp / Landlock サンドボックス）
- `internal/guest` / `internal/hostsvc` … vsock のゲスト側レシーバおよびホスト側サービス
- `internal/access` / `internal/mcpsrv` / `internal/console` … ネットワークアクセス申請・承認コンソール・MCP サーバー
- `internal/authproxy` / `internal/headerpolicy` / `internal/tlsmitm` … LLM 認証プロキシ、ヘッダ制限ポリシー、動的使い捨て CA
- `internal/pr` … VM 内コミットの取り込みとホスト側での再署名・PR 作成
- `internal/config` / `internal/paths` / `internal/tui` … 設定管理、パス解決、起動・終了 TUI

## 守るべき設計の原則 (コーディング指針)

1. **隔離の主体はホスト側に置く**
   VM 内部のセキュリティ機構でホスト側のアクセス制御を代替してはいけません。VM 内部の seccomp や Landlock は、危険なシステムコールの侵入経路を減らすための追加の防御層（多層防御）であり、隔離の根幹は VM 境界とホスト側（netns 内の nftables や vsock 制御）にあります。この主従関係を逆転させてはなりません。
2. **機密情報を VM 内に持ち込まない**
   API キー、コミット署名鍵、GitHub トークンなどの機密情報はホスト側にのみ保持し、ホスト側のプロキシや取り込み処理の段階で付与します。VM に渡すのは run ごとの一時認証トークンと、Git の `user.name` / `user.email` のみです。
3. **非 root（非特権）で動作させる**
   ホスト側で特権を要する操作を追加してはいけません。VM 内部でも rootless Docker を利用し、root 権限で動くデーモンは起動しません。
4. **VM から渡される入力値を信用しない**
   申請理由、DNS ホスト名、クリップボードの内容など VM から受け取る文字列は、承認コンソールに表示する前に制御文字や書字方向制御文字（Bidi）を無害化します。
5. **各種リソースや要求に上限を設ける**
   VM 内のエージェントがホストのリソースや人間の承認者を過剰に消費（枯渇）させないよう、すべてのインターフェースに上限（件数・データサイズ・タイムアウト）を設定します。新しいインターフェースを追加する場合も同様です。
6. **アウトバウンド通信は既定で遮断する**
   新たな通信経路を追加する場合は、必ず許可制（DNS + nftables + 透過プロキシ）を経由させます。ホストのリソースへ到達可能な経路を増やす場合は、本書および `design.md` を更新してください。
7. **テストコードを整備する**
   純粋なロジックは単体テスト（unit test）として実装します。外部コマンド（QEMU、nftables、GnuPG など）に依存するテストは、環境にコマンドが存在しない場合にスキップするか、テスト用のインターフェースを分離してください。

## コミットと PR

- コミットメッセージは日本語で記述し、`fix(範囲):` / `feat(範囲):` などの接頭辞を付与してください（例: `fix(image/gentoo): …`、`feat(vm,image): …`）。
- PR は単一の関心事に絞り、無関係なコード整形などを混在させないでください。
- VM 内で作成されたコミットは一時的な使い捨て SSH 鍵で署名され、ホスト側がこれを取り込んで正規の鍵で再署名します。仕組みの詳細は [design.md](design.md) の「PR の作成と署名」を参照してください。

## ドキュメントの配置と役割

- `README.md` … 基本的な使い方（利用者向け。内部アーキテクチャの詳細は記載しない）。
- `docs/design.md` … 設計と脅威モデル（隔離機構、アクセス制限、プロキシの内部仕様）。
- `docs/development.md` … 本書（開発の進め方とコーディング規約）。
- `.opencode/skills/recipe-authoring/` … ベースイメージレシピの作成規約（エージェントがレシピを追加・変更する際に参照）。
- `.opencode/skills/document-authoring/` … ドキュメント構成・作成規約（エージェントがドキュメントを作成・整理する際に参照）。
