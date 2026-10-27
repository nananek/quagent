package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nananek/quagent/internal/access"
	"github.com/nananek/quagent/internal/console"
	"github.com/nananek/quagent/internal/paths"
)

// ctlExitError は ctl の終了コードを dispatch まで運ぶ。
type ctlExitError struct {
	Code int
	Msg  string
}

func (e *ctlExitError) Error() string { return e.Msg }

func ctlExitf(code int, format string, a ...any) error {
	return &ctlExitError{Code: code, Msg: fmt.Sprintf(format, a...)}
}

// ctlExitCode は ctl が返した終了コードを取り出す (それ以外のエラーは 1)。
func ctlExitCode(err error) int {
	if err == nil {
		return 0
	}
	var e *ctlExitError
	if errors.As(err, &e) {
		return e.Code
	}
	return 1
}

// runMeta は run の作業ディレクトリに置く meta.json。古い run には無いので、
// 読めなくても ctl は動くこと。
type runMeta struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Repo  string `json:"repo"`
	Agent string `json:"agent"`
	Tmux  string `json:"tmux"`
	PID   int    `json:"pid"`
}

// runEntry は RunsDir の 1 つの run。
type runEntry struct {
	Dir   string
	ID    string
	Meta  runMeta
	Alive bool
}

const ctlDefaultEvents = "request,settled,relaxrequest,relaxsettled,prrequest,prsettled,dockerrequest,dockersettled,batchrequest,batchsettled"

// cmdCtl は外部プロセス (Hermes など) が承認の流れを読むための CLI。
// 新しい socket やプロトコルは作らず、既存の console.sock を UI として使う。
func cmdCtl(args []string) error {
	if len(args) == 0 {
		return ctlExitf(2, "ctl: list / watch / decide を指定する")
	}
	switch args[0] {
	case "list":
		return ctlList(os.Stdout, args[1:])
	case "watch":
		return ctlWatch(os.Stdout, args[1:])
	case "decide":
		return ctlDecide(args[1:])
	default:
		return ctlExitf(2, "ctl: 不明なサブコマンド %q (list / watch / decide)", args[0])
	}
}

func ctlFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

func ctlParse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ctlExitf(2, "%s: 使い方を表示した", fs.Name())
		}
		return ctlExitf(2, "%v", err)
	}
	if fs.NArg() > 0 {
		return ctlExitf(2, "%s: 想定しない引数 %q", fs.Name(), fs.Arg(0))
	}
	return nil
}

// scanRuns は RunsDir の各サブディレクトリを調べる。meta.json が無い古い run も
// ディレクトリ名を id として扱う。
func scanRuns() ([]runEntry, error) {
	dir := paths.RunsDir()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var runs []runEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		work := filepath.Join(dir, e.Name())
		r := runEntry{Dir: work, ID: e.Name()}
		if b, err := os.ReadFile(filepath.Join(work, "meta.json")); err == nil {
			var m runMeta
			if json.Unmarshal(b, &m) == nil {
				r.Meta = m
				if m.ID != "" {
					r.ID = m.ID
				}
			}
		}
		r.Alive = consoleAlive(filepath.Join(work, "console.sock"))
		runs = append(runs, r)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].ID < runs[j].ID })
	return runs, nil
}

// consoleAlive は console.sock に短時間だけ繋いで生きているかを見る。
func consoleAlive(sock string) bool {
	c, err := net.DialTimeout("unix", sock, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// resolveRun は --run の解決を 1 か所に集約する。明示が無ければ、生きている
// console.sock が 1 つだけのときにそれを選ぶ。
func resolveRun(explicit string) (runEntry, error) {
	runs, err := scanRuns()
	if err != nil {
		return runEntry{}, ctlExitf(1, "run の一覧を読めない: %v", err)
	}
	if explicit != "" {
		var matches []runEntry
		for _, r := range runs {
			if r.ID == explicit || filepath.Base(r.Dir) == explicit || r.Dir == explicit {
				matches = append(matches, r)
			}
		}
		switch len(matches) {
		case 0:
			return runEntry{}, ctlExitf(1, "run %q は無い", explicit)
		case 1:
			return matches[0], nil
		default:
			return runEntry{}, ctlExitf(2, "run %q は複数に一致する (作業ディレクトリで指定する):\n%s", explicit, runCandidates(matches))
		}
	}
	var alive []runEntry
	for _, r := range runs {
		if r.Alive {
			alive = append(alive, r)
		}
	}
	switch len(alive) {
	case 0:
		return runEntry{}, ctlExitf(3, "生きている run が無い (quagent run を起動してから使う)")
	case 1:
		return alive[0], nil
	default:
		return runEntry{}, ctlExitf(2, "run が複数ある (--run で指定する):\n%s", runCandidates(alive))
	}
}

func runCandidates(runs []runEntry) string {
	var b strings.Builder
	for _, r := range runs {
		fmt.Fprintf(&b, "  %s", console.Sanitize(r.ID))
		if r.Meta.Repo != "" {
			fmt.Fprintf(&b, "  %s", console.Sanitize(r.Meta.Repo))
		}
		fmt.Fprintf(&b, "  (%s)\n", console.Sanitize(r.Dir))
	}
	return strings.TrimRight(b.String(), "\n")
}

// ctlList は run の一覧を出す。--json は JSON lines。
func ctlList(w io.Writer, args []string) error {
	fs := ctlFlags("ctl list")
	jsonOut := fs.Bool("json", false, "JSON lines で出す")
	if err := ctlParse(fs, args); err != nil {
		return err
	}
	runs, err := scanRuns()
	if err != nil {
		return ctlExitf(1, "run の一覧を読めない: %v", err)
	}
	if *jsonOut {
		enc := json.NewEncoder(w)
		for _, r := range runs {
			row := struct {
				ID    string `json:"id"`
				Alive bool   `json:"alive"`
				Name  string `json:"name,omitempty"`
				Repo  string `json:"repo,omitempty"`
				Agent string `json:"agent,omitempty"`
				Tmux  string `json:"tmux,omitempty"`
				PID   int    `json:"pid,omitempty"`
			}{
				ID: r.ID, Alive: r.Alive, Name: r.Meta.Name, Repo: r.Meta.Repo,
				Agent: r.Meta.Agent, Tmux: r.Meta.Tmux, PID: r.Meta.PID,
			}
			if err := enc.Encode(row); err != nil {
				return ctlExitf(1, "出力できない: %v", err)
			}
		}
		return nil
	}
	for _, r := range runs {
		status := "dead"
		if r.Alive {
			status = "alive"
		}
		fmt.Fprintf(w, "%s  %s", console.Sanitize(r.ID), status)
		if r.Meta.Repo != "" {
			fmt.Fprintf(w, "  repo=%s", console.Sanitize(r.Meta.Repo))
		}
		if r.Meta.Agent != "" {
			fmt.Fprintf(w, "  agent=%s", console.Sanitize(r.Meta.Agent))
		}
		if r.Meta.Name != "" {
			fmt.Fprintf(w, "  name=%s", console.Sanitize(r.Meta.Name))
		}
		fmt.Fprintln(w)
	}
	return nil
}

// ctlWatch は UI として console.sock に繋ぎ、イベントを stdout に流す。
// 切れたら 2 秒おきに繋ぎ直す。SIGINT で終了する。
func ctlWatch(w io.Writer, args []string) error {
	fs := ctlFlags("ctl watch")
	runFlag := fs.String("run", "", "run の ID (省略時は生きている run が 1 つだけならそれ)")
	jsonOut := fs.Bool("json", false, "JSON lines で出す")
	eventsFlag := fs.String("events", ctlDefaultEvents, "流すイベント (カンマ区切り)")
	if err := ctlParse(fs, args); err != nil {
		return err
	}
	events, err := parseCtlEvents(*eventsFlag)
	if err != nil {
		return err
	}
	run, err := resolveRun(*runFlag)
	if err != nil {
		return err
	}
	sock := filepath.Join(run.Dir, "console.sock")

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(interrupt)

	bw := bufio.NewWriter(w)
	defer bw.Flush()

	for {
		conn, err := console.Dial(sock)
		if err != nil {
			select {
			case <-interrupt:
				return nil
			case <-time.After(2 * time.Second):
			}
			continue
		}
		dropped := false
		for !dropped {
			select {
			case <-interrupt:
				_ = conn.Close()
				return nil
			case m, ok := <-conn.Events():
				if !ok {
					dropped = true
					break
				}
				if !events[m.Type] {
					continue
				}
				if err := writeCtlEvent(bw, *jsonOut, run.ID, m); err != nil {
					_ = conn.Close()
					return ctlExitf(1, "出力できない: %v", err)
				}
				if err := bw.Flush(); err != nil {
					_ = conn.Close()
					return ctlExitf(1, "出力できない: %v", err)
				}
			}
		}
		_ = conn.Close()
		select {
		case <-interrupt:
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

func parseCtlEvents(s string) (map[string]bool, error) {
	known := map[string]bool{
		"request": true, "settled": true,
		"relaxrequest": true, "relaxsettled": true,
		"prrequest": true, "prsettled": true,
		"dockerrequest": true, "dockersettled": true,
		"batchrequest": true, "batchsettled": true,
		"log": true, "clip": true, "clipsettled": true,
	}
	out := map[string]bool{}
	for _, name := range strings.Split(s, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !known[name] {
			return nil, ctlExitf(2, "ctl watch: 不明なイベント %q", name)
		}
		out[name] = true
	}
	if len(out) == 0 {
		return nil, ctlExitf(2, "ctl watch: --events が空")
	}
	return out, nil
}

// ctlEvent は Msg に run を足したもの。Msg 自体には run が無いため、watch の
// 出力でだけ付ける (hub のプロトコルは変えない)。
type ctlEvent struct {
	console.Msg
	Run string `json:"run"`
}

func writeCtlEvent(w io.Writer, jsonOut bool, run string, m console.Msg) error {
	if jsonOut {
		return json.NewEncoder(w).Encode(ctlEvent{Msg: sanitizeEvent(m), Run: run})
	}
	_, err := fmt.Fprintln(w, ctlEventLine(m))
	return err
}

// sanitizeEvent は JSON に出す前に、VM 側が決められる文字列を Sanitize する。
// body は仕様どおり --json のときだけ生のまま残す (下流で表示するときは
// エスケープが必要)。
func sanitizeEvent(m console.Msg) console.Msg {
	m.Domains = sanitizeStrings(m.Domains)
	m.Reason = console.Sanitize(m.Reason)
	m.Deadline = console.Sanitize(m.Deadline)
	m.Question = console.Sanitize(m.Question)
	m.Text = console.Sanitize(m.Text)
	m.URL = console.Sanitize(m.URL)
	m.Error = console.Sanitize(m.Error)
	m.Branch = console.Sanitize(m.Branch)
	m.Base = console.Sanitize(m.Base)
	m.Title = console.Sanitize(m.Title)
	m.RelaxHost = console.Sanitize(m.RelaxHost)
	m.RelaxHeaders = sanitizeStrings(m.RelaxHeaders)
	m.RelaxMethods = sanitizeStrings(m.RelaxMethods)
	m.DockerRegistry = console.Sanitize(m.DockerRegistry)
	m.DockerRepository = console.Sanitize(m.DockerRepository)
	m.DockerReference = console.Sanitize(m.DockerReference)
	m.BatchEcosystem = console.Sanitize(m.BatchEcosystem)
	if len(m.BatchItems) > 0 {
		items := make([]console.BatchItem, len(m.BatchItems))
		copy(items, m.BatchItems)
		for i := range items {
			items[i].Name = console.Sanitize(items[i].Name)
			items[i].Version = console.Sanitize(items[i].Version)
		}
		m.BatchItems = items
	}
	return m
}

func sanitizeStrings(in []string) []string {
	if len(in) == 0 {
		return in
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = console.Sanitize(s)
	}
	return out
}

// ctlEventLine は人間向けの 1 行。VM 側が決められる文字列は Sanitize を通す。
// body は端末に出さない (--json のときだけ)。
func ctlEventLine(m console.Msg) string {
	now := func() string {
		if m.Deadline != "" {
			return console.Sanitize(m.Deadline)
		}
		return time.Now().Format("15:04:05")
	}
	kind := func(name string) string { return fmt.Sprintf("%-7s  ", name) }
	switch m.Type {
	case "request":
		return fmt.Sprintf("[%s] %sid=%d  %s  %s", now(), kind("network"), m.ID,
			console.Sanitize(strings.Join(m.Domains, " ")), console.Sanitize(m.Reason))
	case "relaxrequest":
		var extra []string
		if len(m.RelaxHeaders) > 0 {
			extra = append(extra, "headers="+strings.Join(m.RelaxHeaders, ","))
		}
		if len(m.RelaxMethods) > 0 {
			extra = append(extra, "methods="+strings.Join(m.RelaxMethods, ","))
		}
		if m.AllowBody {
			extra = append(extra, "body")
		}
		return fmt.Sprintf("[%s] %sid=%d  %s  %s  %s", now(), kind("relax"), m.ID,
			console.Sanitize(m.RelaxHost), console.Sanitize(strings.Join(extra, " ")), console.Sanitize(m.Reason))
	case "prrequest":
		return fmt.Sprintf("[%s] %sid=%d  %s -> %s  %q", now(), kind("pr"), m.ID,
			console.Sanitize(m.Branch), console.Sanitize(m.Base), console.Sanitize(m.Title))
	case "dockerrequest":
		return fmt.Sprintf("[%s] %sid=%d  %s/%s:%s", now(), kind("docker"), m.ID,
			console.Sanitize(m.DockerRegistry), console.Sanitize(m.DockerRepository), console.Sanitize(m.DockerReference))
	case "batchrequest":
		var names []string
		for _, it := range m.BatchItems {
			names = append(names, console.Sanitize(it.Name+"@"+it.Version))
		}
		return fmt.Sprintf("[%s] %sid=%d  %s  %s", now(), kind("batch"), m.ID,
			console.Sanitize(m.BatchEcosystem), strings.Join(names, " "))
	case "settled", "relaxsettled", "prsettled", "dockersettled", "batchsettled":
		return fmt.Sprintf("[%s] %sid=%d  %s", now(), kind("settled"), m.ID, console.Sanitize(string(m.Status)))
	case "log":
		return fmt.Sprintf("[%s] %s", now(), console.Sanitize(m.Text))
	case "clip":
		return fmt.Sprintf("[%s] %sclip  id=%d  %d bytes", now(), kind("clip"), m.ID, m.Size)
	default:
		return fmt.Sprintf("[%s] %s", now(), console.Sanitize(m.Type))
	}
}

// decideKind は ctl decide の --kind と、それに対応するプロトコル上の種類。
type decideKind struct {
	name       string // network / relax / pr / docker / batch
	request    string // 保留一覧で待つ request の type
	decideType string // 本体へ送る type
}

var decideKinds = map[string]decideKind{
	"network": {name: "network", request: "request", decideType: "decide"},
	"relax":   {name: "relax", request: "relaxrequest", decideType: "relaxdecide"},
	"pr":      {name: "pr", request: "prrequest", decideType: "prdecide"},
	"docker":  {name: "docker", request: "dockerrequest", decideType: "dockerdecide"},
	"batch":   {name: "batch", request: "batchrequest", decideType: "batchdecide"},
}

// ctlDecide は保留中の申請に判断を 1 つ送る。1 接続 1 メッセージ。
// 自動判断はしない (リトライで allow を送り直すこともしない)。
func ctlDecide(args []string) error {
	fs := ctlFlags("ctl decide")
	runFlag := fs.String("run", "", "run の ID (省略時は watch と同じ規則)")
	kindFlag := fs.String("kind", "", "network / relax / pr / docker / batch")
	scopeFlag := fs.String("scope", "once", "承認の種類 (network / relax は once / session / always、batch は once / session)")
	questionFlag := fs.String("question", "", "question のときの問い")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ctlExitf(2, "ctl decide: 使い方を表示した")
		}
		return ctlExitf(2, "%v", err)
	}
	if fs.NArg() != 2 {
		return ctlExitf(2, "使い方: quagent ctl decide --kind KIND [--run ID] ID allow|deny|question")
	}
	id, err := strconv.Atoi(fs.Arg(0))
	if err != nil || id <= 0 {
		return ctlExitf(2, "申請 ID が不正: %q", fs.Arg(0))
	}
	kind, ok := decideKinds[*kindFlag]
	if !ok {
		return ctlExitf(2, "--kind は network / relax / pr / docker / batch のどれか: %q", *kindFlag)
	}
	var status access.Status
	switch fs.Arg(1) {
	case "allow":
		status = access.Approved
	case "deny":
		status = access.Denied
	case "question":
		status = access.Question
	default:
		return ctlExitf(2, "判断は allow / deny / question のどれか: %q", fs.Arg(1))
	}
	if status == access.Question {
		if strings.TrimSpace(*questionFlag) == "" {
			return ctlExitf(2, "question には --question で問いを書く")
		}
	} else if *questionFlag != "" {
		return ctlExitf(2, "--question は判断が question のときだけ使える")
	}
	var scope access.Kind
	switch kind.name {
	case "network", "relax":
		switch *scopeFlag {
		case "once":
			scope = access.Once
		case "session":
			scope = access.Session
		case "always":
			scope = access.Always
		default:
			return ctlExitf(2, "--scope は once / session / always のどれか: %q", *scopeFlag)
		}
	case "batch":
		// パッケージは「この run 中まとめて許可」まで。全プロジェクト共通の
		// 恒久許可は用意しない (任意の名前で外部へ出せる経路になるため)。
		switch *scopeFlag {
		case "once":
			scope = access.Once
		case "session":
			scope = access.Session
		default:
			return ctlExitf(2, "batch の --scope は once / session のどれか: %q", *scopeFlag)
		}
	}

	run, err := resolveRun(*runFlag)
	if err != nil {
		return err
	}
	conn, err := console.Dial(filepath.Join(run.Dir, "console.sock"))
	if err != nil {
		return ctlExitf(1, "console.sock に接続できない: %v", err)
	}
	pending := collectPending(conn, 200*time.Millisecond)
	key := pendingKey(kind.request, id)
	req, ok := pending[key]
	if !ok {
		_ = conn.Close()
		return ctlExitf(3, "申請 %d (%s) は保留中に無い (決着済み・期限切れの可能性)", id, kind.name)
	}

	msg := console.Msg{Type: kind.decideType, ID: id, Status: status}
	if kind.name == "batch" {
		// 一覧が合流で更新されている場合、古い版の判断はサーバーに拒否される。
		msg.BatchRevision = req.BatchRevision
	}
	if status == access.Question {
		// 下流 (Hermes / Discord など) の入力をそのままエージェントの端末へ
		// 流さない。制御文字と Bidi をエスケープする。
		msg.Question = console.Sanitize(*questionFlag)
	}
	if kind.name == "network" || kind.name == "relax" || kind.name == "batch" {
		msg.Kind = scope
	}
	// 監査を書けないなら判断を送らない (監査なしの承認を作らない)。
	if err := logCtlDecision(run.Dir, kind.name, id, status, scope); err != nil {
		_ = conn.Close()
		return ctlExitf(1, "監査ログを書けないので判断を送らなかった: %v", err)
	}
	if err := conn.Send(msg); err != nil {
		_ = conn.Close()
		return ctlExitf(4, "判断を送れなかった: %v", err)
	}
	_ = conn.Close()

	if scope == access.Always && status == access.Approved {
		fmt.Fprintf(os.Stderr, "警告: --scope always は %s に残り、以後すべてのプロジェクトで確認しない\n",
			access.AlwaysPath())
	}
	return nil
}

func pendingKey(requestType string, id int) string {
	return requestType + ":" + strconv.Itoa(id)
}

// collectPending は名乗り直後に送られてくる保留一覧を d のあいだ集める。
// 同じ ID でも種類ごとに別のカウンタなので、種類を含めた key で持つ。
// バッチは最新の版を判断に載せるため、メッセージ全体を覚えておく。
func collectPending(conn *console.Conn, d time.Duration) map[string]console.Msg {
	pending := map[string]console.Msg{}
	timer := time.NewTimer(d)
	defer timer.Stop()
	events := conn.Events()
	for {
		select {
		case <-timer.C:
			return pending
		case m, ok := <-events:
			if !ok {
				return pending
			}
			switch m.Type {
			case "request":
				pending[pendingKey("request", m.ID)] = m
			case "relaxrequest":
				pending[pendingKey("relaxrequest", m.ID)] = m
			case "prrequest":
				pending[pendingKey("prrequest", m.ID)] = m
			case "dockerrequest":
				pending[pendingKey("dockerrequest", m.ID)] = m
			case "batchrequest":
				pending[pendingKey("batchrequest", m.ID)] = m
			case "settled":
				delete(pending, pendingKey("request", m.ID))
			case "relaxsettled":
				delete(pending, pendingKey("relaxrequest", m.ID))
			case "prsettled":
				delete(pending, pendingKey("prrequest", m.ID))
			case "dockersettled":
				delete(pending, pendingKey("dockerrequest", m.ID))
			case "batchsettled":
				delete(pending, pendingKey("batchrequest", m.ID))
			}
		}
	}
}

// logCtlDecision は host 側のログに判断を 1 行残す。書けないときはエラーを返し、
// 呼び出し側は判断を送らずに中止する。
func logCtlDecision(work, kind string, id int, status access.Status, scope access.Kind) error {
	line := fmt.Sprintf("%s ctl decide kind=%s id=%d status=%s", time.Now().Format("15:04:05"), kind, id, status)
	if scope != "" && (kind == "network" || kind == "relax" || kind == "batch") {
		line += " scope=" + string(scope)
	}
	if actor := os.Getenv("QUAGENT_CTL_ACTOR"); actor != "" {
		line += " actor=" + singleLine(actor)
	}
	f, err := os.OpenFile(filepath.Join(work, "host.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(f, line); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// singleLine は監査ログの 1 行を壊す改行をエスケープし、長すぎる値を切る。
func singleLine(s string) string {
	s = console.Sanitize(s)
	s = strings.ReplaceAll(s, "\r", `\r`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\u2028", `\u2028`)
	s = strings.ReplaceAll(s, "\u2029", `\u2029`)
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "…"
	}
	return s
}
