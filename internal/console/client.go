package console

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nananek/quagent/internal/access"
)

const (
	bold  = "\033[1m"
	cyan  = "\033[36m"
	dim   = "\033[2m"
	reset = "\033[0m"
)

var statusText = map[access.Status]string{
	access.Approved: "許可",
	access.Denied:   "拒否",
	access.Question: "質問を返した",
	access.TimedOut: "時間切れ (拒否扱い)",
}

var kindText = map[access.Kind]string{
	access.Once:    "今回のみ",
	access.Session: "このセッション",
	access.Always:  "以後確認しない",
}

// RunClient は tmux のペインで動く承認 UI。本体との接続が切れたらつなぎ直す
// (本体が承認待ちを送り直す)。本体の socket が無くなったら終わる。
func RunClient(sock string) (err error) {
	defer func() {
		// 落ちたら理由が見えるようにしてからペインを閉じる
		if r := recover(); r != nil {
			err = fmt.Errorf("承認コンソールが異常終了: %v\n%s", r, debug.Stack())
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			_ = os.WriteFile(filepath.Join(filepath.Dir(sock), "console.err"), []byte(err.Error()), 0o600)
			fmt.Fprint(os.Stderr, "Enter で閉じる")
			_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		}
	}()
	lines := make(chan string)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			lines <- strings.TrimSpace(sc.Text())
		}
		close(lines)
	}()
	fmt.Println(bold + "quagent 承認コンソール" + reset + dim + "  (quit: VM を破棄して終了)" + reset)
	for {
		c, err := net.Dial("unix", sock)
		if err != nil {
			if _, statErr := os.Stat(sock); statErr != nil {
				return nil // 本体が終わった
			}
			time.Sleep(time.Second)
			continue
		}
		done, err := session(c, lines)
		c.Close()
		if done || err != nil {
			return err
		}
		fmt.Println(dim + "本体との接続が切れた。つなぎ直す…" + reset)
		time.Sleep(time.Second)
	}
}

// session は 1 本の接続で UI を動かす。利用者が終了したら done を返す。
func session(c net.Conn, lines <-chan string) (done bool, err error) {
	enc := json.NewEncoder(c)
	if err := enc.Encode(Msg{Type: "ui"}); err != nil {
		return false, nil
	}
	msgs := make(chan Msg, 64)
	go func() {
		dec := json.NewDecoder(c)
		for {
			var m Msg
			if err := dec.Decode(&m); err != nil {
				close(msgs)
				return
			}
			msgs <- m
		}
	}()
	ui := &clientUI{enc: enc}
	for {
		select {
		case m, ok := <-msgs:
			if !ok {
				return false, nil
			}
			ui.onMsg(m)
		case line, ok := <-lines:
			if !ok {
				return true, nil
			}
			if ui.onLine(line) {
				return true, nil
			}
		}
	}
}

type clientUI struct {
	enc     *json.Encoder
	queue   []Msg
	asking  bool // 質問の文面を入力中
	focused bool // 申請のためにこのペインへフォーカスを移した
	quiting bool // 終了の確認中
}

func (u *clientUI) onMsg(m Msg) {
	switch m.Type {
	case "log":
		if len(u.queue) > 0 {
			fmt.Println()
		}
		fmt.Println(dim + Sanitize(m.Text) + reset)
		if len(u.queue) > 0 {
			fmt.Print(u.prompt())
		}
	case "request", "clip", "prrequest", "guardrequest":
		for _, q := range u.queue {
			if q.Type == m.Type && q.ID == m.ID {
				return
			}
		}
		u.queue = append(u.queue, m)
		if len(u.queue) == 1 {
			u.show()
		} else {
			fmt.Println(dim + "(確認待ちがもう 1 件)" + reset)
		}
	case "settled", "clipsettled", "prsettled", "guardsettled":
		want := "request"
		switch m.Type {
		case "clipsettled":
			want = "clip"
		case "prsettled":
			want = "prrequest"
		case "guardsettled":
			want = "guardrequest"
		}
		for i, q := range u.queue {
			if q.Type != want || q.ID != m.ID {
				continue
			}
			switch {
			case want == "clip":
				fmt.Printf("クリップボード #%d: %s\n", m.ID, Sanitize(m.Text))
			case want == "prrequest":
				switch m.Status {
				case access.Approved:
					fmt.Printf("PR #%d: 承認して push した\n", m.ID)
				case access.TimedOut:
					fmt.Printf("PR #%d: 時間切れ (push しなかった)\n", m.ID)
				default:
					fmt.Printf("PR #%d: 拒否 (push しなかった)\n", m.ID)
				}
			case want == "guardrequest":
				switch m.Status {
				case access.Approved:
					fmt.Printf("内容ガード #%d: 通した\n", m.ID)
				case access.TimedOut:
					fmt.Printf("内容ガード #%d: 時間切れ (止めた)\n", m.ID)
				default:
					fmt.Printf("内容ガード #%d: 止めた\n", m.ID)
				}
			default:
				text := statusText[m.Status]
				if k, ok := kindText[m.Kind]; ok && m.Status == access.Approved {
					text += " (" + k + ")"
				}
				fmt.Printf("#%d: %s\n", m.ID, text)
			}
			u.queue = append(u.queue[:i], u.queue[i+1:]...)
			if i == 0 {
				u.asking = false
				u.next()
			}
			return
		}
	}
}

func (u *clientUI) show() {
	r := u.queue[0]
	switch r.Type {
	case "clip":
		fmt.Printf("\n"+bold+cyan+"━━ クリップボードへの書き込み #%d (%d バイト) ━━"+reset+"\n", r.ID, r.Size)
		fmt.Println(Sanitize(r.Text))
		fmt.Printf(dim+"%s までに応答がなければ拒否"+reset+"\n", Sanitize(r.Deadline))
		fmt.Print(u.prompt())
		u.focus()
		return
	case "prrequest":
		fmt.Printf("\n"+bold+cyan+"━━ PR の作成承認 #%d ━━"+reset+"\n", r.ID)
		fmt.Printf(bold+"ブランチ:"+reset+" %s → %s\n", Sanitize(r.Branch), Sanitize(r.Base))
		fmt.Printf(bold+"タイトル:"+reset+" %s\n", Sanitize(r.Title))
		if body := strings.TrimSpace(r.Body); body != "" {
			fmt.Printf(bold+"本文:"+reset+"\n%s\n", Sanitize(truncateRunes(body, 2000)))
		}
		fmt.Printf(dim+"%s までに応答がなければ拒否 (push しない)"+reset+"\n", Sanitize(r.Deadline))
		fmt.Print(u.prompt())
		u.focus()
		return
	case "guardrequest":
		fmt.Printf("\n"+bold+cyan+"━━ 内容ガードの確認 #%d ━━"+reset+"\n", r.ID)
		fmt.Printf(bold+"理由:"+reset+" %s\n", Sanitize(r.Reason))
		fmt.Printf(bold+"リクエスト:"+reset+" %s %s (%s)\n", Sanitize(r.Method), Sanitize(r.URL), Sanitize(r.Provider))
		if len(r.Headers) > 0 {
			fmt.Print(bold + "ヘッダ:" + reset + "\n")
			for _, h := range r.Headers {
				fmt.Printf("  %s\n", Sanitize(h))
			}
		}
		if body := strings.TrimSpace(r.Body); body != "" {
			fmt.Printf(bold+"本文 (先頭):"+reset+"\n%s\n", Sanitize(truncateRunes(body, 2000)))
		}
		fmt.Printf(dim+"%s までに応答がなければ拒否 (通さない)"+reset+"\n", Sanitize(r.Deadline))
		fmt.Print(u.prompt())
		u.focus()
		return
	}
	fmt.Printf("\n"+bold+cyan+"━━ 接続申請 #%d ━━"+reset+"\n", r.ID)
	fmt.Printf(bold+"理由:"+reset+" %s\n", Sanitize(r.Reason))
	fmt.Printf(bold+"ドメイン:"+reset+" %s\n", Sanitize(strings.Join(r.Domains, ", ")))
	fmt.Printf(dim+"%s までに応答がなければ拒否"+reset+"\n", Sanitize(r.Deadline))
	fmt.Print(u.prompt())
	u.focus()
}

// truncateRunes は s を最大 n ルーンに切り、切ったら末尾に … を付ける。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// prompt は今の確認の入力の案内を返す。
func (u *clientUI) prompt() string {
	switch {
	case len(u.queue) == 0:
		return ""
	case u.queue[0].Type == "clip":
		return "[y] コピーする  [n] 拒否 > "
	case u.queue[0].Type == "prrequest":
		return "[y] 承認して PR を作る  [n] 拒否 > "
	case u.queue[0].Type == "guardrequest":
		return "[y] 通す  [n] 止める > "
	case u.asking:
		return "エージェントへの質問 (空で取り消し): "
	default:
		return "[1] 今回は許可 (5分)  [2] このセッションでは確認しない  [3] 以後確認しない\n[d] 拒否  [q] 質問を返す > "
	}
}

// focus は確認のためにこのペインへフォーカスを移す。
func (u *clientUI) focus() {
	fmt.Print("\a")
	if pane := os.Getenv("TMUX_PANE"); pane != "" {
		_ = exec.Command("tmux", "select-pane", "-t", pane).Run()
		_ = exec.Command("tmux", "display-message", "quagent: 確認が必要です").Run()
		u.focused = true
	}
}

func (u *clientUI) next() {
	if len(u.queue) > 0 {
		u.show()
		return
	}
	if u.focused {
		_ = exec.Command("tmux", "last-pane").Run()
		u.focused = false
	}
}

func (u *clientUI) decide(d Msg) {
	_ = u.enc.Encode(d)
	// 結果は settled で届き、そこで次の申請に進む
}

// onLine は入力 1 行を処理する。終了するなら true。
func (u *clientUI) onLine(line string) bool {
	if u.quiting {
		u.quiting = false
		if line == "y" || line == "Y" {
			_ = u.enc.Encode(Msg{Type: "quit"})
			return true
		}
		fmt.Println("取り消した")
		return false
	}
	if len(u.queue) == 0 {
		switch line {
		case "":
		case "quit":
			u.quiting = true
			fmt.Print("VM を破棄して終了する? [y/N] ")
		default:
			fmt.Println(dim + "承認待ちの申請はない" + reset)
		}
		return false
	}
	r := u.queue[0]
	if r.Type == "clip" {
		switch line {
		case "y":
			u.decide(Msg{Type: "clipdecide", ID: r.ID, Status: access.Approved})
		case "n":
			u.decide(Msg{Type: "clipdecide", ID: r.ID, Status: access.Denied})
		default:
			fmt.Print("y / n のどちらか > ")
		}
		return false
	}
	if r.Type == "prrequest" {
		switch line {
		case "y":
			u.decide(Msg{Type: "prdecide", ID: r.ID, Status: access.Approved})
		case "n":
			u.decide(Msg{Type: "prdecide", ID: r.ID, Status: access.Denied})
		default:
			fmt.Print("y / n のどちらか > ")
		}
		return false
	}
	if r.Type == "guardrequest" {
		switch line {
		case "y":
			u.decide(Msg{Type: "guarddecide", ID: r.ID, Status: access.Approved})
		case "n":
			u.decide(Msg{Type: "guarddecide", ID: r.ID, Status: access.Denied})
		default:
			fmt.Print("y / n のどちらか > ")
		}
		return false
	}
	if u.asking {
		if line == "" {
			u.asking = false
			fmt.Println("取り消した")
			u.show()
			return false
		}
		u.asking = false
		u.decide(Msg{Type: "decide", ID: r.ID, Status: access.Question, Question: line})
		return false
	}
	switch line {
	case "1":
		u.decide(Msg{Type: "decide", ID: r.ID, Status: access.Approved, Kind: access.Once})
	case "2":
		u.decide(Msg{Type: "decide", ID: r.ID, Status: access.Approved, Kind: access.Session})
	case "3":
		u.decide(Msg{Type: "decide", ID: r.ID, Status: access.Approved, Kind: access.Always})
	case "d":
		u.decide(Msg{Type: "decide", ID: r.ID, Status: access.Denied})
	case "q":
		u.asking = true
		fmt.Print("エージェントへの質問 (空で取り消し): ")
	default:
		fmt.Print("1 / 2 / 3 / d / q のどれか > ")
	}
	return false
}

// Sanitize は VM 側が決められる文字列 (理由、DNS の名前など) を端末に出せる形にする。
// 制御文字 (エスケープシーケンスで表示を偽装したり、クリップボードを書き換えたり
// できる) と、文字の向きを入れ替える Unicode 文字を \u 表記にする。改行は残す。
func Sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteRune(r)
		case r == '\t':
			b.WriteString("    ")
		case unicode.IsControl(r), r == utf8.RuneError,
			r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069, r == 0x200E, r == 0x200F:
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
