// quagent hub のテーマ状態。CSP でインライン script を使えないため外部ファイルにし、
// app.js より先に読み込んで描画前に配色を確定する (ライトとダークのちらつき防止)。
// 保存値は "auto" / "light" / "dark"。auto は CSS の prefers-color-scheme に任せる。
"use strict";

(function () {
  const KEY = "quagent-theme";
  const MODES = ["auto", "light", "dark"];

  function read() {
    try {
      const v = localStorage.getItem(KEY);
      if (MODES.indexOf(v) >= 0) return v;
    } catch (e) {
      // ストレージが使えない環境 (プライベートモードなど) は自動にフォールバックする。
    }
    return "auto";
  }

  function apply(mode) {
    const root = document.documentElement;
    if (mode === "auto") root.removeAttribute("data-theme");
    else root.setAttribute("data-theme", mode);
  }

  function resolved(mode) {
    if (mode === "auto") {
      return window.matchMedia && window.matchMedia("(prefers-color-scheme: light)").matches
        ? "light"
        : "dark";
    }
    return mode;
  }

  window.quagentTheme = { KEY: KEY, MODES: MODES, read: read, apply: apply, resolved: resolved };
  apply(read());
})();
