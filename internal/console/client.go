package console

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"

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

// RunClient は tmux のペインで動く承認 UI。
func RunClient(sock string) error {
	c, err := net.Dial("unix", sock)
	if err != nil {
		return err
	}
	defer c.Close()
	enc := json.NewEncoder(c)

	msgs := make(chan Msg)
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
	lines := make(chan string)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			lines <- strings.TrimSpace(sc.Text())
		}
		close(lines)
	}()

	ui := &clientUI{enc: enc}
	fmt.Println(bold + "quagent 承認コンソール" + reset + dim + "  (quit: VM を破棄して終了)" + reset)
	for {
		select {
		case m, ok := <-msgs:
			if !ok {
				fmt.Println("本体との接続が切れた")
				return nil
			}
			ui.onMsg(m)
		case line, ok := <-lines:
			if !ok {
				return nil
			}
			if ui.onLine(line) {
				return nil
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
		fmt.Println(dim + m.Text + reset)
	case "request":
		for _, q := range u.queue {
			if q.ID == m.ID {
				return
			}
		}
		u.queue = append(u.queue, m)
		if len(u.queue) == 1 {
			u.show()
		} else {
			fmt.Printf(dim+"(承認待ちがもう 1 件: #%d)"+reset+"\n", m.ID)
		}
	case "settled":
		for i, q := range u.queue {
			if q.ID != m.ID {
				continue
			}
			text := statusText[m.Status]
			if k, ok := kindText[m.Kind]; ok && m.Status == access.Approved {
				text += " (" + k + ")"
			}
			fmt.Printf("#%d: %s\n", m.ID, text)
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
	fmt.Printf("\n"+bold+cyan+"━━ 接続申請 #%d ━━"+reset+"\n", r.ID)
	fmt.Printf(bold+"理由:"+reset+" %s\n", r.Reason)
	fmt.Printf(bold+"ドメイン:"+reset+" %s\n", strings.Join(r.Domains, ", "))
	fmt.Printf(dim+"%s までに応答がなければ拒否"+reset+"\n", r.Deadline)
	fmt.Println("[1] 今回は許可 (5分)  [2] このセッションでは確認しない  [3] 以後確認しない")
	fmt.Print("[d] 拒否  [q] 質問を返す > ")
	fmt.Print("\a")
	if pane := os.Getenv("TMUX_PANE"); pane != "" {
		_ = exec.Command("tmux", "select-pane", "-t", pane).Run()
		_ = exec.Command("tmux", "display-message", "quagent: 接続申請があります").Run()
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
