// quagent hub のブラウザ側。端末 (xterm.js) と承認パネル (承認待ち / ログ / 履歴) を
// WebSocket で中継する。
"use strict";

(function () {
  const enc = new TextEncoder();
  let consoleWS = null;
  let termWS = null;

  // --- 接続状態の表示 ---
  const connEl = document.getElementById("conn");
  const setConn = (ok, text) => {
    connEl.className = "conn " + (ok ? "ok" : "ng");
    connEl.textContent = text;
  };

  // --- 端末 ---
  // xterm.js が同梱されていない場合は端末ビューだけを諦め、承認パネルは動かす。
  if (typeof Terminal === "undefined" || typeof FitAddon === "undefined") {
    document.getElementById("terminal").innerHTML =
      '<p class="notice">端末表示ライブラリ (xterm.js) が同梱されていません。' +
      "internal/hub/assets に xterm.js / addon-fit.js / xterm.css を配置してください " +
      "(Makefile の hub-assets を参照)。承認パネルは利用できます。</p>";
    setConn(false, "端末なし");
  } else {
    initTerminal();
  }

  function b64encode(str) {
    const bytes = enc.encode(str);
    let bin = "";
    for (const b of bytes) bin += String.fromCharCode(b);
    return btoa(bin);
  }

  function initTerminal() {
    const term = new Terminal({
      convertEol: false,
      cursorBlink: true,
      scrollback: 5000,
      fontFamily: 'Menlo, Consolas, "DejaVu Sans Mono", monospace',
      fontSize: 13,
      theme: { background: "#11131a", foreground: "#e6e6e6" },
    });
    const fit = new FitAddon.FitAddon();
    term.loadAddon(fit);
    term.open(document.getElementById("terminal"));

    function termFit() {
      try { fit.fit(); } catch (e) { /* 表示前は失敗しうる */ }
      if (termWS && termWS.readyState === WebSocket.OPEN) {
        termWS.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
      }
    }

    function connectTerminal() {
      const proto = location.protocol === "https:" ? "wss:" : "ws:";
      const ws = new WebSocket(proto + "//" + location.host + "/api/terminal");
      ws.binaryType = "arraybuffer";
      termWS = ws;
      ws.onopen = () => { setConn(true, "接続済み"); termFit(); };
      ws.onmessage = (ev) => {
        if (typeof ev.data === "string") term.write(ev.data);
        else term.write(new Uint8Array(ev.data));
      };
      ws.onclose = () => {
        setConn(false, "切断 (再接続中…)");
        termWS = null;
        setTimeout(connectTerminal, 1500);
      };
      ws.onerror = () => ws.close();
    }

    term.onData((d) => {
      if (termWS && termWS.readyState === WebSocket.OPEN) {
        termWS.send(JSON.stringify({ type: "input", data: b64encode(d) }));
      }
    });

    window.addEventListener("resize", termFit);
    window.addEventListener("orientationchange", () => setTimeout(termFit, 200));
    connectTerminal();
    setTimeout(termFit, 100);
  }

  // --- 承認パネル ---
  const listEl = document.getElementById("approval-list");
  const logEl = document.getElementById("log-list");
  const histEl = document.getElementById("history-list");
  const cards = new Map(); // key -> 承認待ちカードの要素
  const infos = new Map(); // key -> 申請メッセージ (決着時に履歴を作る)

  const MAX_LOGS = 1000;
  const MAX_HISTORY = 200;

  let activeTab = "pending";
  let unreadLogs = 0;

  function el(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }

  // --- タブ ---
  const tabButtons = Array.from(document.querySelectorAll("#tabs .tab"));

  function showTab(name) {
    activeTab = name;
    tabButtons.forEach((b) => {
      const on = b.dataset.tab === name;
      b.classList.toggle("active", on);
      b.setAttribute("aria-selected", on ? "true" : "false");
    });
    listEl.hidden = name !== "pending";
    logEl.hidden = name !== "logs";
    histEl.hidden = name !== "history";
    if (name === "logs") {
      unreadLogs = 0;
      logEl.scrollTop = logEl.scrollHeight;
    }
    updateBadges();
  }

  function updateBadges() {
    const set = (id, n) => {
      const e = document.getElementById(id);
      if (e) e.textContent = n > 0 ? String(n) : "";
    };
    set("badge-pending", cards.size);
    set("badge-logs", unreadLogs);
    set("badge-history", histEl.childElementCount);
  }

  tabButtons.forEach((b) => b.addEventListener("click", () => showTab(b.dataset.tab)));

  // --- ログ ---
  // 制御文字 (端末の表示を乱す) を落とす。改行とタブは残す。
  function sanitizeLog(s) {
    return String(s).replace(/[\u0000-\u0008\u000B\u000C\u000E-\u001F\u007F]/g, "");
  }

  function appendLog(text) {
    logEl.appendChild(el("div", "logline", sanitizeLog(text)));
    while (logEl.childElementCount > MAX_LOGS) logEl.removeChild(logEl.firstChild);
    if (activeTab === "logs") {
      logEl.scrollTop = logEl.scrollHeight;
    } else {
      unreadLogs++;
      updateBadges();
    }
  }

  // --- 承認待ちカード ---
  function clearEmpty() {
    const p = listEl.querySelector("p.reason");
    if (p) p.remove();
  }

  function showEmpty() {
    if (cards.size === 0 && !listEl.querySelector("p.reason")) {
      const p = el("p", "reason", "承認待ちはありません。");
      listEl.appendChild(p);
    }
  }

  function makeCard(key, msg, title) {
    clearEmpty();
    const card = el("div", "card");
    card.appendChild(el("h3", null, title));
    listEl.prepend(card);
    cards.set(key, card);
    infos.set(key, msg);
    // 新しい申請は承認待ちタブへ切り替える (TUI と同じ挙動)。
    if (activeTab !== "pending") showTab("pending");
    updateBadges();
    return card;
  }

  function button(card, label, cls, onclick) {
    const b = el("button", cls, label);
    b.addEventListener("click", onclick);
    card.querySelector(".row").appendChild(b);
    return b;
  }

  function decide(type, id, status, kind, question) {
    const body = { type: type, id: id, status: status };
    if (kind) body.kind = kind;
    if (question) body.question = question;
    consoleWS.send(JSON.stringify(body));
  }

  // --- 履歴 ---
  function targetOf(m) {
    switch (m.type) {
      case "request":
        return (m.domains || []).join(", ");
      case "relaxrequest": {
        const d = [m.relax_host || ""];
        if (m.relax_methods && m.relax_methods.length) d.push("(" + m.relax_methods.join(",") + ")");
        return d.join(" ");
      }
      case "prrequest":
        return (m.branch || "") + " → " + (m.base || "") + ": " + (m.title || "");
      case "dockerrequest":
        return (m.docker_registry || "") + "/" + (m.docker_repository || "") + ":" + (m.docker_reference || "");
      case "batchrequest":
        return "[" + (m.batch_ecosystem || "") + "] " + ((m.batch_items || []).length) + " 個のパッケージ";
      case "clip":
        return "クリップボード (" + (m.size || 0) + " バイト)";
    }
    return "";
  }

  function resultOf(m, s) {
    const kind = { once: "今回のみ", session: "このセッション", always: "以後確認しない" }[s.kind];
    if (s.status === "approved") {
      switch (m.type) {
        case "prrequest": return "承認して push した";
        case "dockerrequest": return "承認して取得した";
        case "batchrequest": return s.kind === "session" ? "セッション中常に承認" : "承認して取得した";
        case "clip": return s.text || "コピーを許可した";
        default: return "許可" + (kind ? " (" + kind + ")" : "");
      }
    }
    if (s.status === "timeout") {
      switch (m.type) {
        case "prrequest": return "時間切れ (push しなかった)";
        case "dockerrequest":
        case "batchrequest": return "時間切れ (取得しなかった)";
        default: return "時間切れ (拒否扱い)";
      }
    }
    if (s.status === "question") return "質問を返した";
    switch (m.type) {
      case "prrequest": return "拒否 (push しなかった)";
      case "dockerrequest":
      case "batchrequest": return "拒否 (取得しなかった)";
      default: return "拒否";
    }
  }

  function addHistory(m, s) {
    if (!m) return;
    const row = el("div", "histrow");
    row.appendChild(el("span", "histtime", new Date().toLocaleTimeString("ja-JP", { hour12: false })));
    row.appendChild(el("span", "histtarget", targetOf(m)));
    row.appendChild(el("span", "histresult", resultOf(m, s)));
    histEl.prepend(row);
    while (histEl.childElementCount > MAX_HISTORY) histEl.removeChild(histEl.lastChild);
    updateBadges();
  }

  function settle(key, s) {
    const card = cards.get(key);
    if (!card) return;
    const info = infos.get(key);
    cards.delete(key);
    infos.delete(key);
    card.remove();
    addHistory(info, s);
    showEmpty();
    updateBadges();
  }

  function domainList(domains) {
    if (!domains || !domains.length) return el("div");
    const d = el("div", "reason");
    domains.forEach((x) => {
      const c = el("code", null, x);
      d.appendChild(c);
      d.appendChild(document.createTextNode(" "));
    });
    return d;
  }

  function renderApproval(m) {
    switch (m.type) {
      case "request": {
        const key = "req:" + m.id;
        const card = makeCard(key, m, "接続先の申請");
        card.appendChild(domainList(m.domains));
        if (m.reason) card.appendChild(el("div", "reason", "理由: " + m.reason));
        card.appendChild(el("div", "row"));
        button(card, "今回だけ", "primary", () => decide("decide", m.id, "approved", "once"));
        button(card, "このセッション中", null, () => decide("decide", m.id, "approved", "session"));
        button(card, "今後ずっと", null, () => decide("decide", m.id, "approved", "always"));
        button(card, "拒否", "danger", () => decide("decide", m.id, "denied"));
        button(card, "質問", null, () => {
          const q = prompt("エージェントへの質問・指示");
          if (q) decide("decide", m.id, "question", null, q);
        });
        break;
      }
      case "relaxrequest": {
        const key = "relax:" + m.id;
        const card = makeCard(key, m, "ヘッダ・メソッドの緩和: " + (m.relax_host || ""));
        if (m.reason) card.appendChild(el("div", "reason", "理由: " + m.reason));
        const detail = [];
        if (m.relax_headers && m.relax_headers.length) detail.push("ヘッダ: " + m.relax_headers.join(", "));
        if (m.relax_methods && m.relax_methods.length) detail.push("メソッド: " + m.relax_methods.join(", "));
        if (m.allow_body) detail.push("リクエストボディの送信を許可");
        if (detail.length) card.appendChild(el("div", "reason", detail.join("\n")));
        card.appendChild(el("div", "row"));
        button(card, "許可", "primary", () => decide("relaxdecide", m.id, "approved"));
        button(card, "拒否", "danger", () => decide("relaxdecide", m.id, "denied"));
        break;
      }
      case "prrequest": {
        const key = "pr:" + m.id;
        const card = makeCard(key, m, "PR の作成: " + m.branch + " → " + m.base);
        card.appendChild(el("div", "reason", m.title || ""));
        if (m.body) card.appendChild(el("div", "reason", m.body));
        card.appendChild(el("div", "row"));
        button(card, "作成", "primary", () => decide("prdecide", m.id, "approved"));
        button(card, "中止", "danger", () => decide("prdecide", m.id, "denied"));
        break;
      }
      case "dockerrequest": {
        const key = "docker:" + m.id;
        const card = makeCard(key, m, "Docker イメージの取得");
        card.appendChild(el("div", "reason", m.docker_registry + "/" + m.docker_repository + ":" + m.docker_reference));
        card.appendChild(el("div", "row"));
        button(card, "取得", "primary", () => decide("dockerdecide", m.id, "approved"));
        button(card, "拒否", "danger", () => decide("dockerdecide", m.id, "denied"));
        break;
      }
      case "batchrequest": {
        const key = "batch:" + m.id;
        const n = (m.batch_items || []).length;
        const card = makeCard(key, m, "パッケージの一括取得 (" + (m.batch_ecosystem || "") + ", " + n + " 件)");
        if (n) {
          const names = m.batch_items.slice(0, 20).map((i) => i.name + (i.version ? "@" + i.version : "")).join(", ");
          card.appendChild(el("div", "reason", names + (n > 20 ? " …" : "")));
        }
        card.appendChild(el("div", "row"));
        button(card, "取得", "primary", () => decide("batchdecide", m.id, "approved"));
        button(card, "拒否", "danger", () => decide("batchdecide", m.id, "denied"));
        break;
      }
      case "clip": {
        const key = "clip:" + m.id;
        const card = makeCard(key, m, "クリップボードへの書き込み (" + (m.size || 0) + " バイト)");
        if (m.text) card.appendChild(el("div", "reason", m.text));
        card.appendChild(el("div", "row"));
        button(card, "コピー", "primary", () => decide("clipdecide", m.id, "approved"));
        button(card, "拒否", "danger", () => decide("clipdecide", m.id, "denied"));
        break;
      }
      // 決着
      case "settled": settle("req:" + m.id, m); break;
      case "relaxsettled": settle("relax:" + m.id, m); break;
      case "prsettled": settle("pr:" + m.id, m); break;
      case "dockersettled": settle("docker:" + m.id, m); break;
      case "batchsettled": settle("batch:" + m.id, m); break;
      case "clipsettled": settle("clip:" + m.id, m); break;
      // ログ
      case "log":
        appendLog(m.text);
        break;
    }
  }

  function connectConsole() {
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    const ws = new WebSocket(proto + "//" + location.host + "/api/console");
    consoleWS = ws;
    ws.onmessage = (ev) => {
      try { renderApproval(JSON.parse(ev.data)); } catch (e) { /* 壊れたメッセージは無視 */ }
    };
    ws.onclose = () => {
      consoleWS = null;
      setTimeout(connectConsole, 1500);
    };
    ws.onerror = () => ws.close();
  }
  connectConsole();
})();
