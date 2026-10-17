# hub のブラウザ資産

`index.html` / `app.css` / `app.js` は quagent 本体に同梱しています
(`internal/hub/assets.go` の `//go:embed`)。端末表示に使う次のファイルは
本体の実装とは独立に更新できるよう、`make hub-assets` で取得して同梱しています。

| ファイル | 取得元 | ライセンス |
| --- | --- | --- |
| `xterm.js` | `@xterm/xterm` 5.5.0 (`lib/xterm.js`) | MIT |
| `xterm.css` | `@xterm/xterm` 5.5.0 (`css/xterm.css`) | MIT |
| `addon-fit.js` | `@xterm/addon-fit` 0.10.0 (`lib/addon-fit.js`) | MIT |
| `XTERM-LICENSE` | `@xterm/xterm` 5.5.0 (`LICENSE`) | MIT |

版を上げるときは `Makefile` の `XTERM_VERSION` / `XTERM_FIT_VERSION` を書き換えて
`make hub-assets` を実行します。

- 取得物は**改変せずそのまま**同梱しています (末尾の `sourceMappingURL` は
  マップを同梱していないため開発者ツールで 404 になりますが、動作に影響は
  ありません)。
- xterm.js は CSP の `script-src 'self'` で動きます (`eval` / `new Function` を
  使いません)。`<style>` を注入するため `style-src` には `'unsafe-inline'` が
  必要です (`internal/hub/hub.go` の `csp` を参照)。
- `make hub-assets` を実行していない場合でも hub は起動し、承認パネルは動作
  します。端末ビューだけが「未同梱」の案内表示になります。
