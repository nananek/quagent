package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nananek/quagent/internal/access"
	"github.com/nananek/quagent/internal/console"
	"github.com/nananek/quagent/internal/paths"
)

// setupRunsDir は XDG_STATE_HOME を一時ディレクトリに向け、runs の親を返す。
func setupRunsDir(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	runs := paths.RunsDir()
	if err := os.MkdirAll(runs, 0o755); err != nil {
		t.Fatal(err)
	}
	return runs
}

// startFakeConsole は console.sock の代わりに待ち受け、ui を受けたら onHello を
// 呼んでから判断を読む。読んだ判断は返り値の channel に流れる。
func startFakeConsole(t *testing.T, work string, onHello func(enc *json.Encoder)) <-chan console.Msg {
	t.Helper()
	sock := filepath.Join(work, "console.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	got := make(chan console.Msg, 8)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				dec, enc := json.NewDecoder(c), json.NewEncoder(c)
				var hello console.Msg
				if dec.Decode(&hello) != nil || hello.Type != "ui" {
					return
				}
				if onHello != nil {
					onHello(enc)
				}
				for {
					var m console.Msg
					if dec.Decode(&m) != nil {
						return
					}
					got <- m
				}
			}(c)
		}
	}()
	return got
}

func makeRunDir(t *testing.T, runs, dir string, meta *runMeta) string {
	t.Helper()
	work := filepath.Join(runs, dir)
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if meta != nil {
		if err := writeRunMeta(work, *meta); err != nil {
			t.Fatal(err)
		}
	}
	return work
}

func TestCtlList(t *testing.T) {
	runs := setupRunsDir(t)
	alive := makeRunDir(t, runs, "20261028-153012-aaaaaa", &runMeta{
		ID: "20261028-153012", Name: "foo", Repo: "/srv/repos/foo",
		Agent: "opencode", Tmux: "quagent-20261028-153012-aaaaaa", PID: 12345,
	})
	startFakeConsole(t, alive, nil)
	// meta.json が無い古い run も一覧に出る。
	makeRunDir(t, runs, "20261027-010203-bbbbbb", nil)

	var buf bytes.Buffer
	if err := ctlList(&buf, nil); err != nil {
		t.Fatalf("ctlList: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "20261028-153012  alive  repo=/srv/repos/foo  agent=opencode  name=foo") {
		t.Fatalf("一覧: %q", out)
	}
	if !strings.Contains(out, "20261027-010203-bbbbbb  dead") {
		t.Fatalf("meta 無しの run: %q", out)
	}

	buf.Reset()
	if err := ctlList(&buf, []string{"--json"}); err != nil {
		t.Fatalf("ctlList --json: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("JSON の行数: %d: %q", len(lines), buf.String())
	}
	var row struct {
		ID    string `json:"id"`
		Alive bool   `json:"alive"`
		Repo  string `json:"repo"`
		Agent string `json:"agent"`
	}
	var matched bool
	for _, line := range lines {
		var r struct {
			ID    string `json:"id"`
			Alive bool   `json:"alive"`
			Repo  string `json:"repo"`
			Agent string `json:"agent"`
		}
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		if r.ID == "20261028-153012" {
			row.ID, row.Alive, row.Repo, row.Agent = r.ID, r.Alive, r.Repo, r.Agent
			matched = true
		}
	}
	if !matched {
		t.Fatalf("JSON に run が無い: %q", buf.String())
	}
	if row.ID != "20261028-153012" || !row.Alive || row.Repo != "/srv/repos/foo" || row.Agent != "opencode" {
		t.Fatalf("JSON: %+v", row)
	}
}

func TestCtlDecideDeny(t *testing.T) {
	runs := setupRunsDir(t)
	work := makeRunDir(t, runs, "20261028-153012-aaaaaa", &runMeta{ID: "20261028-153012"})
	got := startFakeConsole(t, work, func(enc *json.Encoder) {
		_ = enc.Encode(console.Msg{Type: "request", ID: 3, Domains: []string{"pypi.org"}, Reason: "pip install"})
	})
	if err := ctlDecide([]string{"--run", "20261028-153012", "--kind", "network", "3", "deny"}); err != nil {
		t.Fatalf("ctlDecide: %v (code %d)", err, ctlExitCode(err))
	}
	select {
	case m := <-got:
		if m.Type != "decide" || m.ID != 3 || m.Status != access.Denied || m.Kind != access.Once {
			t.Fatalf("判断: %+v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("判断が届かない")
	}
	b, err := os.ReadFile(filepath.Join(work, "host.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "ctl decide kind=network id=3 status=denied") {
		t.Fatalf("host.log: %q", b)
	}
}

func TestCtlDecideQuestion(t *testing.T) {
	runs := setupRunsDir(t)
	work := makeRunDir(t, runs, "20261028-153012-aaaaaa", &runMeta{ID: "20261028-153012"})
	got := startFakeConsole(t, work, func(enc *json.Encoder) {
		_ = enc.Encode(console.Msg{Type: "relaxrequest", ID: 5, RelaxHost: "example.com", Reason: "header"})
	})
	if err := ctlDecide([]string{"--run", "20261028-153012", "--kind", "relax",
		"--question", "どのヘッダ?", "5", "question"}); err != nil {
		t.Fatalf("ctlDecide: %v (code %d)", err, ctlExitCode(err))
	}
	select {
	case m := <-got:
		if m.Type != "relaxdecide" || m.ID != 5 || m.Status != access.Question || m.Question != "どのヘッダ?" {
			t.Fatalf("問い返し: %+v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("問い返しが届かない")
	}
}

func TestCtlDecideMissingPending(t *testing.T) {
	runs := setupRunsDir(t)
	work := makeRunDir(t, runs, "20261028-153012-aaaaaa", &runMeta{ID: "20261028-153012"})
	startFakeConsole(t, work, nil) // 保留ゼロ
	err := ctlDecide([]string{"--run", "20261028-153012", "--kind", "network", "3", "allow"})
	if code := ctlExitCode(err); code != 3 {
		t.Fatalf("exit code = %d (%v), want 3", code, err)
	}
}

func TestCtlDecideAlreadySettled(t *testing.T) {
	runs := setupRunsDir(t)
	work := makeRunDir(t, runs, "20261028-153012-aaaaaa", &runMeta{ID: "20261028-153012"})
	got := startFakeConsole(t, work, func(enc *json.Encoder) {
		_ = enc.Encode(console.Msg{Type: "request", ID: 3, Domains: []string{"pypi.org"}})
		_ = enc.Encode(console.Msg{Type: "settled", ID: 3, Status: access.TimedOut})
	})
	err := ctlDecide([]string{"--run", "20261028-153012", "--kind", "network", "3", "allow"})
	if code := ctlExitCode(err); code != 3 {
		t.Fatalf("exit code = %d (%v), want 3", code, err)
	}
	select {
	case m := <-got:
		t.Fatalf("決着済みなのに判断を送った: %+v", m)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestCtlDecideKindMismatch(t *testing.T) {
	runs := setupRunsDir(t)
	work := makeRunDir(t, runs, "20261028-153012-aaaaaa", &runMeta{ID: "20261028-153012"})
	startFakeConsole(t, work, func(enc *json.Encoder) {
		// network の申請しか無い。
		_ = enc.Encode(console.Msg{Type: "request", ID: 5, Domains: []string{"pypi.org"}})
	})
	err := ctlDecide([]string{"--run", "20261028-153012", "--kind", "pr", "5", "allow"})
	if code := ctlExitCode(err); code != 3 {
		t.Fatalf("exit code = %d (%v), want 3", code, err)
	}
}

func TestCtlDecideValidation(t *testing.T) {
	cases := [][]string{
		{"--kind", "network", "3"},                                // 判断が無い
		{"--kind", "unknown", "3", "allow"},                       // 不明な kind
		{"--kind", "network", "x", "allow"},                       // ID が不正
		{"--kind", "network", "3", "yes"},                         // 不明な判断
		{"--kind", "network", "3", "question"},                    // question が無い
		{"--kind", "network", "--question", "q", "3", "allow"},    // question 以外
		{"--kind", "network", "--scope", "forever", "3", "allow"}, // 不明な scope
	}
	for _, args := range cases {
		if code := ctlExitCode(ctlDecide(args)); code != 2 {
			t.Errorf("%v: exit code = %d, want 2", args, code)
		}
	}
}

func TestCtlEventLine(t *testing.T) {
	line := ctlEventLine(console.Msg{
		Type: "prrequest", ID: 1, Branch: "foo", Base: "main",
		Title: "fix: \x1b[31mred", Body: "secret body",
	})
	if strings.Contains(line, "secret body") {
		t.Fatalf("body が端末出力に出ている: %q", line)
	}
	if strings.Contains(line, "\x1b") {
		t.Fatalf("制御文字が残っている: %q", line)
	}
	if !strings.Contains(line, "pr       id=1  foo -> main") {
		t.Fatalf("行: %q", line)
	}

	var buf bytes.Buffer
	if err := writeCtlEvent(&buf, true, "20261028-153012", console.Msg{
		Type: "request", ID: 3, Domains: []string{"pypi.org"}, Reason: "pip install",
		Deadline: "15:42:10",
	}); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["type"] != "request" || got["run"] != "20261028-153012" || got["deadline"] != "15:42:10" {
		t.Fatalf("JSON: %v", got)
	}
}

func TestCtlDecideSanitizesQuestion(t *testing.T) {
	runs := setupRunsDir(t)
	work := makeRunDir(t, runs, "20261028-153012-aaaaaa", &runMeta{ID: "20261028-153012"})
	got := startFakeConsole(t, work, func(enc *json.Encoder) {
		_ = enc.Encode(console.Msg{Type: "request", ID: 3, Domains: []string{"pypi.org"}})
	})
	q := "answer \x1b[2J \u202e spoof"
	if err := ctlDecide([]string{"--run", "20261028-153012", "--kind", "network",
		"--question", q, "3", "question"}); err != nil {
		t.Fatalf("ctlDecide: %v", err)
	}
	select {
	case m := <-got:
		if strings.ContainsAny(m.Question, "\x1b\u202e") {
			t.Fatalf("question に制御文字/Bidi が残っている: %q", m.Question)
		}
		if !strings.Contains(m.Question, `\u001b`) || !strings.Contains(m.Question, `\u202e`) {
			t.Fatalf("question が sanitize されていない: %q", m.Question)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("判断が届かない")
	}
}

func TestCtlWatchJSONSanitizesReason(t *testing.T) {
	var buf bytes.Buffer
	if err := writeCtlEvent(&buf, true, "20261028-153012", console.Msg{
		Type: "request", ID: 3, Domains: []string{"pypi.org"},
		Reason: "pip install \u202e evil",
	}); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(buf.Bytes(), []byte("\u202e")) {
		t.Fatalf("JSON に Bidi 制御文字が生で出ている: %q", buf.String())
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	reason, _ := got["reason"].(string)
	if !strings.Contains(reason, `\u202e`) {
		t.Fatalf("reason が sanitize されていない: %q", reason)
	}
}

func TestLogCtlDecisionSingleLine(t *testing.T) {
	work := t.TempDir()
	t.Setenv("QUAGENT_CTL_ACTOR", "alice\n2026-10-28 15:00:00 ctl decide kind=network id=99 status=approved")
	if err := logCtlDecision(work, "network", 3, access.Denied, access.Once); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(work, "host.log"))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(b), "\n"); lines != 1 {
		t.Fatalf("host.log が %d 行: %q", lines, b)
	}
	if !strings.Contains(string(b), `actor=alice\n2026-10-28`) {
		t.Fatalf("actor の改行がエスケープされていない: %q", b)
	}
}

func TestCtlDecideFailsWhenAuditUnavailable(t *testing.T) {
	runs := setupRunsDir(t)
	work := makeRunDir(t, runs, "20261028-153012-aaaaaa", &runMeta{ID: "20261028-153012"})
	got := startFakeConsole(t, work, func(enc *json.Encoder) {
		_ = enc.Encode(console.Msg{Type: "request", ID: 3, Domains: []string{"pypi.org"}})
	})
	if err := os.Mkdir(filepath.Join(work, "host.log"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := ctlDecide([]string{"--run", "20261028-153012", "--kind", "network", "3", "allow"})
	if code := ctlExitCode(err); code != 1 {
		t.Fatalf("exit code = %d (%v), want 1", code, err)
	}
	select {
	case m := <-got:
		t.Fatalf("監査ログを書けないのに判断を送った: %+v", m)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestParseCtlEvents(t *testing.T) {
	events, err := parseCtlEvents(ctlDefaultEvents)
	if err != nil {
		t.Fatal(err)
	}
	if !events["request"] || !events["batchsettled"] || events["log"] {
		t.Fatalf("既定のイベント: %v", events)
	}
	if code := ctlExitCode(func() error { _, err := parseCtlEvents("request,bogus"); return err }()); code != 2 {
		t.Fatalf("不明なイベントの exit code = %d", code)
	}
}
