# 開発ガイドライン

quagent の開発では「隔離の主体はホスト側に置く」原則を厳守し、ホスト側の非特権制御と VM 内への機密情報非保持を前提として実装・テストを進めます。

関連ドキュメント:
- [README.md](../README.md) — 基本的な使い方と利用者向け設定
- [design.md](design.md) — 内部アーキテクチャ、アクセス制御、脅威モデル
- [.opencode/skills/recipe-authoring/](../.opencode/skills/recipe-authoring/SKILL.md) — ベースイメージレシピの作成規約
- [.opencode/skills/document-authoring/](../.opencode/skills/document-authoring/SKILL.md) — ドキュメント構成・整理規約

## ビルドとテスト: ローカルで静的バイナリを検証し CI を全通過させる

```sh
make build                     # bin/quagent (VM 内にも持ち込むため CGO_ENABLED=0 の静的リンクでビルド)
make install                   # ~/.local/bin/quagent にインストール (PREFIX で変更可)
make test                      # go test ./...
make vet                       # go vet ./...
```

CI (`.github/workflows/ci.yml`) では、push および PR 作成のたびに以下の項目を検証します。PR を提出する前に、ローカル環境ですべて通過することを確認してください。

- コード整形 (`gofmt`)
- 依存関係の整合性 (`go mod tidy -diff`)
- 静的解析 (`go vet ./...`)
- 単体テスト (`go test ./...`)
- 静的リンクビルド (`make build`)

```sh
test -z "$(gofmt -l .)"
go mod tidy -diff
go vet ./...
go test ./...
make build
```

依存パッケージの更新は Dependabot で週 1 回自動化しています。サプライチェーン攻撃のリスクを抑えるため、新規リリース直後のパッケージは即座に取り込まず、公開から 7 日間経過した安定版のみを取り込みます。

## リポジトリ構成: CLI と VM 隔離パッケージを分離する

- `cmd/quagent` — CLI および TUI のエントリポイント、VM 実行制御 (`run.go`, `guestops.go`, `skills.go` 等)
- `internal/image` — ベースイメージのビルドと管理 (レシピは `recipes/<名前>/`)
- `internal/vm` — QEMU 起動コンポーネント (seed ISO 作成、overlay、起動パラメータ構築)
- `internal/netns` — 子 netns 内の nftables 制御、自前 DNS、透過プロキシ、TLS 終端
- `internal/sandbox` — VM 内部の追加防御層 (seccomp / Landlock サンドボックス)
- `internal/guest` / `internal/hostsvc` — vsock のゲスト側レシーバおよびホスト側サービス
- `internal/access` / `internal/mcpsrv` / `internal/console` — ネットワークアクセス申請、承認コンソール、MCP サーバー
- `internal/authproxy` / `internal/headerpolicy` / `internal/tlsmitm` — LLM 認証プロキシ、ヘッダ制限ポリシー、動的使い捨て CA
- `internal/pr` — VM 内コミットの取り込み、ホスト側での再署名と PR 作成
- `internal/config` / `internal/paths` / `internal/tui` — 設定管理、パス解決、起動・終了 TUI

## 守るべき設計の原則: 7 つのアーキテクチャ要件

1. **隔離の主体はホスト側に置く**
   VM 内部のセキュリティ機構でホスト側のアクセス制御を代替してはいけません。VM 内部の seccomp や Landlock は、危険なシステムコールの侵入経路を減らす追加の防御層（多層防御）であり、隔離の根幹は VM 境界とホスト側（netns 内の nftables や vsock 制御）にあります。この主従関係を逆転させてはなりません。
2. **機密情報を VM 内に持ち込まない**
   API キー、コミット署名鍵、GitHub トークンなどの機密情報はホスト側にのみ保持し、ホスト側のプロキシや取り込み処理で付与します。VM に渡すのは run ごとの一時認証トークンと、Git の `user.name` / `user.email` のみです。
3. **非特権（非 root）で動作させる**
   ホスト側で root 特権を要する操作を追加してはいけません。VM 内部でも rootless Docker を利用し、root 権限で動くデーモンは起動しません。
4. **VM から渡される入力値を信用しない**
   申請理由、DNS ホスト名、クリップボードの内容など VM から受け取る文字列は、承認コンソールに表示する前に制御文字や書字方向制御文字（Bidi）を無害化します。
5. **各種リソースや要求に上限を設ける**
   VM 内のエージェントがホストのリソースや人間の承認者を過剰に消費（枯渇）させないよう、すべてのインターフェースに上限（件数・データサイズ・タイムアウト）を設定します。新しいインターフェースを追加する場合も同様です。
6. **アウトバウンド通信は既定で遮断する**
   新たな通信経路を追加する場合は、必ず許可制（DNS + nftables + 透過プロキシ）を経由させます。ホストのリソースへ到達可能な経路を増やす場合は、本書および `design.md` を更新してください。
7. **テストコードを整備する**
   純粋なロジックは単体テスト（unit test）として実装します。外部コマンド（QEMU、nftables、GnuPG など）に依存するテストは、環境にコマンドが存在しない場合にスキップするか、テスト用のインターフェースを分離してください。

## コミットと PR: 日本語コミットと単一関心事を徹底する

- コミットメッセージは日本語で記述し、`fix(範囲):` / `feat(範囲):` などの接頭辞を付与してください（例: `fix(image/gentoo): …`、`feat(vm,image): …`）。
- PR は単一の関心事に絞り、無関係なコード整形などを混在させないでください。
- VM 内で作成されたコミットは一時的な使い捨て SSH 鍵で署名され、ホスト側がこれを取り込んで正規の鍵で再署名します。仕組みの詳細は [design.md](design.md#pr-作成と再署名-使い捨て鍵コミットを取り込みホスト側で正規鍵に再署名する) の「PR 作成と再署名」を参照してください。

## ドキュメントの更新ワークフローと配置役割

機能の追加や仕様変更を行った際は、以下の順序でドキュメントを更新してください。

1. **`docs/design.md` を更新:** 内部仕様、脅威モデル、多層防御一覧への反映、制限事項を記録します。
2. **`README.md` を更新 (必要最小限):** 利用者が使うための CLI オプション、設定例、操作方法のみを追記します（内部アーキテクチャの詳細は含めません）。
3. **`docs/development.md` を更新:** 新しいパッケージや設計原則に関わる変更があれば反映します。
4. **相互リンクと用語の整合性を確認:** 相対パスのリンク切れや用語の揺れがないか確認します。

### ドキュメントの役割分担

- `README.md` — 利用者ガイド（基本的な使い方と設定。内部実装詳細は含めない）
- `docs/design.md` — 設計と脅威モデル（隔離機構、通信制御、各種プロキシの詳細仕様）
- `docs/development.md` — 開発ガイドライン（開発手順、コーディング規約、更新ワークフロー）
- `.opencode/skills/recipe-authoring/` — レシピ作成規約（ベースイメージの追加・変更時に参照）
- `.opencode/skills/document-authoring/` — ドキュメント規約（ドキュメント体系と記載基準）

開発や機能追加に着手する際は、まず `make test` で現在のテストが通ることを確認してから作業ブランチを作成してください。
