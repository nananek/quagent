# 開発

この文書は quagent 自体を開発する人向けの指針。使い方は [README](../README.md)、
内部の仕組みと脅威モデルは [design.md](design.md)、ベースイメージのレシピは
skill `recipe-authoring` を参照。

## ビルドとテスト

```sh
make build                     # bin/quagent (VM に持ち込むので静的リンク、CGO_ENABLED=0)
make install                   # ~/.local/bin/quagent に入れる (PREFIX で変更可)
make test                      # go test ./...
make vet                       # go vet ./...
```

CI (`.github/workflows/ci.yml`) は push と PR ごとに、整形・`go.mod`/`go.sum`・vet・
テスト・静的リンクのビルドを確かめる。PR を出す前にローカルでも次を通す:

```sh
test -z "$(gofmt -l .)"
go mod tidy -diff
go vet ./...
go test ./...
make build
```

依存の更新は Dependabot が週に一度まとめる。出たばかりの版をすぐには取り込まない
(乗っ取られた版を掴まないよう、公開から 7 日待つ設定にしてある)。

## リポジトリ構成

- `cmd/quagent` … CLI・TUI の入口と、run の組み立て (`run.go`・`guestops.go` など)
- `internal/image` … ベースイメージの焼き込み・管理。レシピは `recipes/<名前>/`
- `internal/vm` … qemu の起動部品 (seed ISO・overlay・コマンドライン)
- `internal/netns` … 子 netns の nftables・自前 DNS・透明プロキシ・TLS 終端
- `internal/sandbox` … VM の中の一枚 (seccomp / Landlock)
- `internal/guest` / `internal/hostsvc` … vsock の受け口 (guest) と host の窓口
- `internal/access` / `internal/mcpsrv` / `internal/console` … 接続先の申請・承認
- `internal/authproxy` / `internal/guard` / `internal/tlsmitm` … LLM プロキシ・
  コンテンツガード・使い捨て CA
- `internal/pr` … PR の作成と署名のやり直し
- `internal/config` / `internal/paths` / `internal/tui` … 設定・置き場・TUI

## 守るべき設計の約束 (コーディング指針)

1. **閉じ込めの本体は host 側に置く。** VM の中の仕組みで外側の許可制を置き換えない。
   VM の中の一枚 (seccomp / Landlock) は、危険な syscall の入口を減らす追加の一枚で、
   閉じ込めの本体は VM と host (netns の nft・vsock) にある。この順序を逆にしない。
2. **秘密を VM に入れない。** API キー・署名鍵・gh のトークンは host に残し、host の
   プロキシや取り込みの工程で付ける。VM に渡すのは run ごとの使い捨てトークンと、
   利用者の `user.name` / `user.email` だけ。
3. **非 root で動く。** host で特権が要る操作を足さない。VM の中も rootless docker を
   使い、rootful のデーモンは動かさない。
4. **VM から来る文字列を信用しない。** 理由・DNS の名前・クリップボードの中身などは、
   承認コンソールに出す前に制御文字と向きを入れ替える文字を無害化する。
5. **上限を設ける。** VM の中のエージェントが host の資源や承認者を使い潰せないよう、
   すべての窓口に上限 (件数・大きさ・時間) を付ける。新しい窓口を足すときも同じ。
6. **外向きは既定でゼロ。** 新しい通信を足すときは、許可制 (DNS + nft + 透明プロキシ) を
   通す。host の資源へ届く経路を増やすときは、この文書と `design.md` を更新する。
7. **テストを書く。** 純粋なロジックは unit test にする。外部コマンド (qemu・nft・gpg
   など) が要るものは、無ければ skip するか、テスト用の口を分ける。

## コミットと PR

- コミットメッセージは日本語で、`fix(範囲):` / `feat(範囲):` のように接頭辞を付ける
  (例 `fix(image/gentoo): …`、`feat(vm,image): …`)。
- PR は 1 つの関心事に絞る。無関係な整形を混ぜない。
- VM 内で作ったコミットは使い捨ての ssh 鍵で署名され、host が取り込んで署名し直す。
  仕組みは [design.md](design.md) の「PR の作成と署名」を参照。

## ドキュメントの置き場

- `README.md` … 使い方 (これから使う人が読む)。内部の話は書かない。
- `docs/design.md` … 設計と脅威モデル (仕組み・制限・プロキシ)。
- `docs/development.md` … この文書 (開発の進め方とコーディング指針)。
- `.opencode/skills/recipe-authoring/` … ベースイメージのレシピの約束
  (エージェントがレシピを足す・直すときに読む)。
