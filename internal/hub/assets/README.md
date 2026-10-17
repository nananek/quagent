# hub のブラウザ資産

このディレクトリの `index.html` と `app.js` は quagent 本体に同梱されています
(`internal/hub/assets.go` の `//go:embed`)。端末表示に使う次の 3 ファイルは
サイズが大きいため本体には含めておらず、`make hub-assets` で取得して同梱します。

| ファイル | 取得元 | ライセンス |
| --- | --- | --- |
| `xterm.js` | `@xterm/xterm` (`lib/xterm.js`) | MIT |
| `xterm.css` | `@xterm/xterm` (`css/xterm.css`) | MIT |
| `addon-fit.js` | `@xterm/addon-fit` (`lib/addon-fit.js`) | MIT |

```sh
make hub-assets   # ネットワークがある環境で 1 度だけ実行する
```

- 取得する版は `Makefile` の `XTERM_VERSION` / `XTERM_FIT_VERSION` で固定しています。
- 取得物のライセンス表示は併せて取得する `XTERM-LICENSE` です。
- 未取得でも hub は起動し、承認パネルは動作します。端末ビューだけが
  「未同梱」の案内表示になります。
