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

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/nananek/quagent/internal/access"
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

type tabIndex int

const (
	tabPending tabIndex = iota
	tabLogs
	tabHistory
)

const (
	maxLogsHistory    = 1000
	maxSettledHistory = 200
)

// historyEntry はセッション中に決着した申請の記録。
type historyEntry struct {
	ID        int
	Type      string
	Target    string
	Reason    string
	Status    access.Status
	Kind      access.Kind
	Question  string
	Result    string
	SettledAt time.Time
}

// socketMsg はサーバーから受信した JSON Msg。
type socketMsg Msg

// socketConnectedMsg はサーバーと接続できたときの通知。
type socketConnectedMsg struct{}

// socketDisconnectedMsg はサーバーとの接続が切れたときの通知。
type socketDisconnectedMsg struct{}

// socketTerminatedMsg はサーバーの socket が消失して終了したときの通知。
type socketTerminatedMsg struct{}

// Styles
var (
	colPrimary = lipgloss.Color("4")  // Blue
	colSuccess = lipgloss.Color("2")  // Green
	colWarning = lipgloss.Color("3")  // Yellow
	colDanger  = lipgloss.Color("1")  // Red
	colMagenta = lipgloss.Color("5")  // Magenta
	colCyan    = lipgloss.Color("6")  // Cyan
	colDim     = lipgloss.Color("8")  // Dim / Gray
	colText    = lipgloss.Color("15") // White

	styleTitle = lipgloss.NewStyle().Bold(true).Foreground(colCyan)

	styleActiveTab = lipgloss.NewStyle().
			Bold(true).
			Foreground(colCyan).
			Border(lipgloss.NormalBorder(), false, false, true, false).
			BorderForeground(colCyan).
			Padding(0, 1)

	styleInactiveTab = lipgloss.NewStyle().
				Foreground(colDim).
				Padding(0, 1)

	styleBadge = lipgloss.NewStyle().Bold(true)

	styleCard = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colPrimary).
			Padding(0, 1)

	styleModal = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colWarning).
			Padding(1, 2)

	styleFooter = lipgloss.NewStyle().
			Foreground(colDim)

	styleKey = lipgloss.NewStyle().
			Bold(true).
			Foreground(colCyan)

	styleLabel = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("7"))
)

// model は Bubbletea TUI の状態。
type model struct {
	sock      string
	activeTab tabIndex

	// 承認待ちキュー
	pending         []Msg
	selectedPending int

	// ログ
	logs          []string
	unreadLogs    int
	logViewport   viewport.Model
	logAutoScroll bool

	// 履歴
	history         []historyEntry
	selectedHistory int

	// 質問入力モード
	asking        bool
	questionInput textinput.Model

	// 終了確認モーダル
	quiting bool

	// ヘルプ表示
	showHelp bool

	// tmux 連携
	focused bool

	// ソケット接続
	connected bool

	// 端末サイズ
	width  int
	height int
	ready  bool

	// 送受信チャネル
	outChan   chan Msg
	eventChan chan tea.Msg
}

func newModel(sock string) model {
	ti := textinput.New()
	ti.Placeholder = "エージェントへの質問を入力 (空で取り消し)..."
	ti.CharLimit = 500

	vp := viewport.New(80, 10)

	return model{
		sock:          sock,
		activeTab:     tabPending,
		logViewport:   vp,
		logAutoScroll: true,
		questionInput: ti,
		outChan:       make(chan Msg, 64),
		eventChan:     make(chan tea.Msg, 128),
		connected:     false,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(
		m.waitForEvent(),
		textinput.Blink,
	)
}

func (m model) waitForEvent() tea.Cmd {
	return func() tea.Msg {
		if m.eventChan == nil {
			return nil
		}
		return <-m.eventChan
	}
}

func (m model) decide(d Msg) {
	if m.outChan != nil {
		select {
		case m.outChan <- d:
		default:
		}
	}
}

func focusTmux() {
	fmt.Print("\a")
	if pane := os.Getenv("TMUX_PANE"); pane != "" {
		_ = exec.Command("tmux", "select-pane", "-t", pane).Run()
		_ = exec.Command("tmux", "display-message", "quagent: 確認が必要です").Run()
	}
}

func unfocusTmux() {
	if pane := os.Getenv("TMUX_PANE"); pane != "" {
		_ = exec.Command("tmux", "last-pane").Run()
	}
}

func (m model) appendLog(text string) model {
	sanitized := time.Now().Format("15:04:05 ") + Sanitize(text)
	m.logs = append(m.logs, sanitized)
	if len(m.logs) > maxLogsHistory {
		m.logs = m.logs[len(m.logs)-maxLogsHistory:]
	}
	if m.activeTab != tabLogs {
		m.unreadLogs++
	}
	m = m.updateLogViewportContent()
	if m.logAutoScroll {
		m.logViewport.GotoBottom()
	}
	return m
}

func (m model) updateLogViewportContent() model {
	var sb strings.Builder
	for _, l := range m.logs {
		switch {
		case strings.Contains(l, "拒否") || strings.Contains(l, "遮断") || strings.Contains(l, "[警告]") || strings.Contains(l, "[リソース保護停止]"):
			sb.WriteString(lipgloss.NewStyle().Foreground(colDanger).Render(l))
		case strings.Contains(l, "許可") || strings.Contains(l, "放棄") || strings.Contains(l, "承認"):
			sb.WriteString(lipgloss.NewStyle().Foreground(colSuccess).Render(l))
		case strings.Contains(l, "クリップボード"):
			sb.WriteString(lipgloss.NewStyle().Foreground(colWarning).Render(l))
		default:
			sb.WriteString(lipgloss.NewStyle().Foreground(colDim).Render(l))
		}
		sb.WriteString("\n")
	}
	m.logViewport.SetContent(sb.String())
	return m
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		vpHeight := max(3, m.height-4)
		m.logViewport.Width = max(20, m.width-2)
		m.logViewport.Height = vpHeight
		m = m.updateLogViewportContent()
		if m.logAutoScroll {
			m.logViewport.GotoBottom()
		}

	case socketConnectedMsg:
		m.connected = true
		cmds = append(cmds, m.waitForEvent())

	case socketDisconnectedMsg:
		m.connected = false
		cmds = append(cmds, m.waitForEvent())

	case socketTerminatedMsg:
		return m, tea.Quit

	case socketMsg:
		m = m.handleSocketMsg(Msg(msg))
		cmds = append(cmds, m.waitForEvent())

	case tea.KeyMsg:
		nextM, cmd := m.handleKeyMsg(msg)
		return nextM, cmd
	}

	return m, tea.Batch(cmds...)
}

func (m model) handleSocketMsg(msg Msg) model {
	switch msg.Type {
	case "log":
		return m.appendLog(msg.Text)

	case "request", "relaxrequest", "clip", "prrequest":
		for _, q := range m.pending {
			if q.Type == msg.Type && q.ID == msg.ID {
				return m
			}
		}
		m.pending = append(m.pending, msg)
		m.activeTab = tabPending
		if !m.focused {
			focusTmux()
			m.focused = true
		}
		if m.selectedPending >= len(m.pending) {
			m.selectedPending = 0
		}
		return m

	case "settled", "clipsettled", "prsettled", "relaxsettled":
		want := "request"
		switch msg.Type {
		case "clipsettled":
			want = "clip"
		case "prsettled":
			want = "prrequest"
		case "relaxsettled":
			want = "relaxrequest"
		}

		for i, q := range m.pending {
			if q.Type != want || q.ID != msg.ID {
				continue
			}

			// 履歴に追加
			entry := historyEntry{
				ID:        msg.ID,
				Type:      q.Type,
				Status:    msg.Status,
				Kind:      msg.Kind,
				SettledAt: time.Now(),
			}

			switch want {
			case "clip":
				entry.Target = fmt.Sprintf("クリップボード (%d バイト)", q.Size)
				entry.Result = msg.Text
				if entry.Result == "" {
					entry.Result = statusText[msg.Status]
				}
			case "prrequest":
				entry.Target = fmt.Sprintf("%s → %s: %s", q.Branch, q.Base, q.Title)
				switch msg.Status {
				case access.Approved:
					entry.Result = "承認して push した"
				case access.TimedOut:
					entry.Result = "時間切れ (push しなかった)"
				default:
					entry.Result = "拒否 (push しなかった)"
				}
			case "relaxrequest":
				entry.Target = fmt.Sprintf("%s (%s)", q.RelaxHost, strings.Join(q.RelaxMethods, ","))
				entry.Reason = q.Reason
				res := statusText[msg.Status]
				if k, ok := kindText[msg.Kind]; ok && msg.Status == access.Approved {
					res += " (" + k + ")"
				}
				entry.Result = res
			default:
				entry.Target = strings.Join(q.Domains, ", ")
				entry.Reason = q.Reason
				res := statusText[msg.Status]
				if k, ok := kindText[msg.Kind]; ok && msg.Status == access.Approved {
					res += " (" + k + ")"
				}
				entry.Result = res
			}

			m.history = append([]historyEntry{entry}, m.history...)
			if len(m.history) > maxSettledHistory {
				m.history = m.history[:maxSettledHistory]
			}

			// キューから削除
			m.pending = append(m.pending[:i], m.pending[i+1:]...)
			if m.selectedPending >= len(m.pending) && len(m.pending) > 0 {
				m.selectedPending = len(m.pending) - 1
			}

			if len(m.pending) == 0 {
				m.asking = false
				if m.focused {
					unfocusTmux()
					m.focused = false
				}
			}
			return m
		}
	}
	return m
}

func (m model) handleKeyMsg(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// 終了確認モーダル表示中の操作
	if m.quiting {
		switch key {
		case "y", "Y":
			m.decide(Msg{Type: "quit"})
			return m, tea.Quit
		case "n", "N", "esc":
			m.quiting = false
			return m, nil
		}
		return m, nil
	}

	// ヘルプ表示中の操作
	if m.showHelp {
		if key == "esc" || key == "?" || key == "enter" {
			m.showHelp = false
		}
		return m, nil
	}

	// 質問入力中の操作
	if m.asking {
		switch key {
		case "esc":
			m.asking = false
			m.questionInput.Blur()
			return m, nil
		case "enter":
			text := strings.TrimSpace(m.questionInput.Value())
			m.asking = false
			m.questionInput.Blur()
			if text != "" && len(m.pending) > 0 && m.selectedPending < len(m.pending) {
				r := m.pending[m.selectedPending]
				if r.Type == "relaxrequest" {
					m.decide(Msg{Type: "relaxdecide", ID: r.ID, Status: access.Question, Question: text})
				} else {
					m.decide(Msg{Type: "decide", ID: r.ID, Status: access.Question, Question: text})
				}
			}
			return m, nil
		default:
			var cmd tea.Cmd
			m.questionInput, cmd = m.questionInput.Update(msg)
			return m, cmd
		}
	}

	// グローバルキー
	switch key {
	case "ctrl+c":
		m.quiting = true
		return m, nil
	case "?":
		m.showHelp = true
		return m, nil
	case "tab":
		m.activeTab = (m.activeTab + 1) % 3
		if m.activeTab == tabLogs {
			m.unreadLogs = 0
		}
		return m, nil
	case "shift+tab":
		m.activeTab = (m.activeTab + 2) % 3
		if m.activeTab == tabLogs {
			m.unreadLogs = 0
		}
		return m, nil
	}

	// タブごとのキー
	switch m.activeTab {
	case tabPending:
		if len(m.pending) == 0 {
			switch key {
			case "1":
				m.activeTab = tabPending
			case "2":
				m.activeTab = tabLogs
				m.unreadLogs = 0
			case "3":
				m.activeTab = tabHistory
			case "q", "Q":
				m.quiting = true
			}
			return m, nil
		}

		// 複数申請がある場合のキュー移動
		if len(m.pending) > 1 {
			switch key {
			case "up", "k", "left", "h":
				if m.selectedPending > 0 {
					m.selectedPending--
				}
				return m, nil
			case "down", "j", "right", "l":
				if m.selectedPending < len(m.pending)-1 {
					m.selectedPending++
				}
				return m, nil
			}
		}

		r := m.pending[m.selectedPending]
		switch r.Type {
		case "clip":
			switch key {
			case "y", "Y":
				m.decide(Msg{Type: "clipdecide", ID: r.ID, Status: access.Approved})
			case "n", "N":
				m.decide(Msg{Type: "clipdecide", ID: r.ID, Status: access.Denied})
			}
		case "prrequest":
			switch key {
			case "y", "Y":
				m.decide(Msg{Type: "prdecide", ID: r.ID, Status: access.Approved})
			case "n", "N":
				m.decide(Msg{Type: "prdecide", ID: r.ID, Status: access.Denied})
			}
		case "relaxrequest":
			switch key {
			case "1":
				m.decide(Msg{Type: "relaxdecide", ID: r.ID, Status: access.Approved, Kind: access.Once})
			case "2":
				m.decide(Msg{Type: "relaxdecide", ID: r.ID, Status: access.Approved, Kind: access.Session})
			case "d", "D":
				m.decide(Msg{Type: "relaxdecide", ID: r.ID, Status: access.Denied})
			case "q", "Q":
				m.asking = true
				m.questionInput.Reset()
				m.questionInput.Focus()
				return m, textinput.Blink
			}
		default: // request
			switch key {
			case "1":
				m.decide(Msg{Type: "decide", ID: r.ID, Status: access.Approved, Kind: access.Once})
			case "2":
				m.decide(Msg{Type: "decide", ID: r.ID, Status: access.Approved, Kind: access.Session})
			case "3":
				m.decide(Msg{Type: "decide", ID: r.ID, Status: access.Approved, Kind: access.Always})
			case "d", "D":
				m.decide(Msg{Type: "decide", ID: r.ID, Status: access.Denied})
			case "q", "Q":
				m.asking = true
				m.questionInput.Reset()
				m.questionInput.Focus()
				return m, textinput.Blink
			}
		}

	case tabLogs:
		switch key {
		case "1":
			m.activeTab = tabPending
		case "2":
			m.activeTab = tabLogs
		case "3":
			m.activeTab = tabHistory
		case "q", "Q":
			m.quiting = true
		case "up", "k":
			m.logViewport.LineUp(1)
			m.logAutoScroll = false
		case "down", "j":
			m.logViewport.LineDown(1)
			if m.logViewport.AtBottom() {
				m.logAutoScroll = true
			}
		case "pgup", "ctrl+u", "b":
			m.logViewport.HalfViewUp()
			m.logAutoScroll = false
		case "pgdown", "ctrl+d", "f":
			m.logViewport.HalfViewDown()
			if m.logViewport.AtBottom() {
				m.logAutoScroll = true
			}
		case "g", "home":
			m.logViewport.GotoTop()
			m.logAutoScroll = false
		case "G", "end":
			m.logViewport.GotoBottom()
			m.logAutoScroll = true
		}

	case tabHistory:
		switch key {
		case "1":
			m.activeTab = tabPending
		case "2":
			m.activeTab = tabLogs
			m.unreadLogs = 0
		case "3":
			m.activeTab = tabHistory
		case "q", "Q":
			m.quiting = true
		case "up", "k":
			if m.selectedHistory > 0 {
				m.selectedHistory--
			}
		case "down", "j":
			if m.selectedHistory < len(m.history)-1 {
				m.selectedHistory++
			}
		}
	}

	return m, nil
}

func (m model) View() string {
	if !m.ready {
		return "初期化中..."
	}

	var sb strings.Builder

	// 1. ヘッダー (タイトル、タブ、接続状態)
	sb.WriteString(m.renderHeader())
	sb.WriteString("\n")

	// 2. メインコンテンツ
	var content string
	switch {
	case m.quiting:
		content = m.renderQuitModal()
	case m.showHelp:
		content = m.renderHelpModal()
	case m.asking:
		content = m.renderAskingCard()
	case m.activeTab == tabPending:
		content = m.renderPendingTab()
	case m.activeTab == tabLogs:
		content = m.renderLogsTab()
	case m.activeTab == tabHistory:
		content = m.renderHistoryTab()
	}

	sb.WriteString(content)
	sb.WriteString("\n")

	// 3. フッター (操作ガイド)
	sb.WriteString(m.renderFooter())

	return sb.String()
}

func (m model) renderHeader() string {
	title := styleTitle.Render("quagent 承認コンソール")

	// タブ表示
	pendingBadge := ""
	if len(m.pending) > 0 {
		pendingBadge = fmt.Sprintf(" (%d)", len(m.pending))
	}
	tab1Text := "1: 承認待ち" + pendingBadge
	var tab1 string
	if m.activeTab == tabPending {
		tab1 = styleActiveTab.Render(tab1Text)
	} else {
		tab1 = styleInactiveTab.Render(tab1Text)
	}

	logsBadge := ""
	if m.unreadLogs > 0 {
		logsBadge = fmt.Sprintf(" ●%d", m.unreadLogs)
	}
	tab2Text := "2: ログ" + logsBadge
	var tab2 string
	if m.activeTab == tabLogs {
		tab2 = styleActiveTab.Render(tab2Text)
	} else {
		tab2 = styleInactiveTab.Render(tab2Text)
	}

	histBadge := ""
	if len(m.history) > 0 {
		histBadge = fmt.Sprintf(" (%d)", len(m.history))
	}
	tab3Text := "3: 履歴" + histBadge
	var tab3 string
	if m.activeTab == tabHistory {
		tab3 = styleActiveTab.Render(tab3Text)
	} else {
		tab3 = styleInactiveTab.Render(tab3Text)
	}

	tabs := lipgloss.JoinHorizontal(lipgloss.Top, tab1, " ", tab2, " ", tab3)

	connStatus := lipgloss.NewStyle().Foreground(colSuccess).Render("● 接続中")
	if !m.connected {
		connStatus = lipgloss.NewStyle().Foreground(colWarning).Render("○ 再接続待機")
	}

	space := max(1, m.width-lipgloss.Width(title)-lipgloss.Width(tabs)-lipgloss.Width(connStatus)-4)
	return lipgloss.JoinHorizontal(lipgloss.Center, title, "  ", tabs, strings.Repeat(" ", space), connStatus)
}

func (m model) renderPendingTab() string {
	if len(m.pending) == 0 {
		box := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colDim).
			Padding(1, 2).
			Render("✓ 承認待ちの申請はありません (エージェントが作業中)")
		return box
	}

	r := m.pending[m.selectedPending]

	var sb strings.Builder
	if len(m.pending) > 1 {
		sb.WriteString(fmt.Sprintf(styleDim("申請キュー [%d/%d]  (↑/↓ で選択)\n"), m.selectedPending+1, len(m.pending)))
	}

	cardHeader := ""
	switch r.Type {
	case "clip":
		cardHeader = fmt.Sprintf(styleBadge.Foreground(colWarning).Render("━━ クリップボード書込 #%d (%d バイト) ━━ 期限: %s"), r.ID, r.Size, Sanitize(r.Deadline))
	case "prrequest":
		cardHeader = fmt.Sprintf(styleBadge.Foreground(colSuccess).Render("━━ PR 作成承認 #%d ━━ 期限: %s"), r.ID, Sanitize(r.Deadline))
	case "relaxrequest":
		cardHeader = fmt.Sprintf(styleBadge.Foreground(colMagenta).Render("━━ HTTP緩和申請 #%d ━━ 期限: %s"), r.ID, Sanitize(r.Deadline))
	default:
		cardHeader = fmt.Sprintf(styleBadge.Foreground(colCyan).Render("━━ ドメイン接続申請 #%d ━━ 期限: %s"), r.ID, Sanitize(r.Deadline))
	}
	sb.WriteString(cardHeader + "\n")

	switch r.Type {
	case "clip":
		sb.WriteString(Sanitize(r.Text) + "\n")
	case "prrequest":
		sb.WriteString(fmt.Sprintf("%s %s → %s\n", styleLabel.Render("ブランチ:"), Sanitize(r.Branch), Sanitize(r.Base)))
		sb.WriteString(fmt.Sprintf("%s %s\n", styleLabel.Render("タイトル:"), Sanitize(r.Title)))
		if b := strings.TrimSpace(r.Body); b != "" {
			sb.WriteString(fmt.Sprintf("%s\n%s\n", styleLabel.Render("本文:"), Sanitize(truncateRunes(b, 500))))
		}
	case "relaxrequest":
		sb.WriteString(fmt.Sprintf("%s %s\n", styleLabel.Render("対象ホスト:"), Sanitize(r.RelaxHost)))
		if len(r.RelaxHeaders) > 0 {
			sb.WriteString(fmt.Sprintf("%s %s\n", styleLabel.Render("許可ヘッダ:"), Sanitize(strings.Join(r.RelaxHeaders, ", "))))
		}
		if len(r.RelaxMethods) > 0 {
			sb.WriteString(fmt.Sprintf("%s %s\n", styleLabel.Render("許可メソッド:"), Sanitize(strings.Join(r.RelaxMethods, ", "))))
		}
		if r.AllowBody {
			sb.WriteString(fmt.Sprintf("%s 許可\n", styleLabel.Render("ボディ送信:")))
		}
		sb.WriteString(fmt.Sprintf("%s %s\n", styleLabel.Render("申請理由:"), Sanitize(r.Reason)))
	default:
		sb.WriteString(fmt.Sprintf("%s %s\n", styleLabel.Render("対象ドメイン:"), Sanitize(strings.Join(r.Domains, ", "))))
		sb.WriteString(fmt.Sprintf("%s %s\n", styleLabel.Render("申請理由:"), Sanitize(r.Reason)))
	}

	actions := ""
	switch r.Type {
	case "clip":
		actions = "[y] コピー許可    [n] 拒否"
	case "prrequest":
		actions = "[y] 承認して push    [n] 拒否"
	case "relaxrequest":
		actions = "[1] 今回のみ (5分)    [2] セッション許可    [d] 拒否    [q] 質問を返す"
	default:
		actions = "[1] 今回のみ (5分)    [2] セッション許可    [3] 恒久許可    [d] 拒否    [q] 質問を返す"
	}
	sb.WriteString("\n" + styleKey.Render(actions))

	return styleCard.Width(max(30, m.width-4)).Render(sb.String())
}

func (m model) renderLogsTab() string {
	scrollHint := "[最新]"
	if !m.logAutoScroll {
		scrollHint = "[過去ログ閲覧中: G で最新へ]"
	}
	header := fmt.Sprintf(styleDim("ログ一覧 (%d 件)  %s  [↑/↓/PgUp/PgDn スクロール]"), len(m.logs), scrollHint)
	return header + "\n" + m.logViewport.View()
}

func (m model) renderHistoryTab() string {
	if len(m.history) == 0 {
		return lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colDim).
			Padding(1, 2).
			Render("まだ決着した申請はありません")
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(styleDim("決着履歴 (%d 件)  [↑/↓ で選択]\n"), len(m.history)))

	maxItems := max(3, m.height-6)
	for i, h := range m.history {
		if i >= maxItems {
			sb.WriteString(styleDim(fmt.Sprintf("... 他 %d 件省略\n", len(m.history)-maxItems)))
			break
		}

		cursor := "  "
		if i == m.selectedHistory {
			cursor = "▶ "
		}

		statusColor := colSuccess
		if strings.Contains(h.Result, "拒否") || strings.Contains(h.Result, "時間切れ") {
			statusColor = colDanger
		} else if strings.Contains(h.Result, "質問") {
			statusColor = colCyan
		}

		line := fmt.Sprintf("%s%s #%-2d [%s] %-30s → %s",
			cursor,
			h.SettledAt.Format("15:04:05"),
			h.ID,
			h.Type,
			truncateRunes(h.Target, 30),
			lipgloss.NewStyle().Bold(true).Foreground(statusColor).Render(h.Result),
		)
		sb.WriteString(line + "\n")
	}

	return sb.String()
}

func (m model) renderAskingCard() string {
	var sb strings.Builder
	sb.WriteString(styleTitle.Render("エージェントへの質問を入力") + "\n\n")
	sb.WriteString(m.questionInput.View() + "\n\n")
	sb.WriteString(styleDim("[Enter] 送信    [Esc] キャンセル"))
	return styleModal.Width(max(30, m.width-6)).Render(sb.String())
}

func (m model) renderQuitModal() string {
	var sb strings.Builder
	sb.WriteString(styleTitle.Render("VM を破棄して終了しますか?") + "\n\n")
	sb.WriteString(styleKey.Render("[y] 終了する") + "    " + styleDim("[n / Esc] キャンセル"))
	return styleModal.Width(max(30, m.width-6)).Render(sb.String())
}

func (m model) renderHelpModal() string {
	var sb strings.Builder
	sb.WriteString(styleTitle.Render("quagent 承認コンソール ヘルプ") + "\n\n")
	sb.WriteString("Tab / Shift+Tab   タブ切り替え (承認待ち / ログ / 履歴)\n")
	sb.WriteString("1 / 2 / 3         各タブへ直接ジャンプ\n")
	sb.WriteString("↑ / ↓ / j / k     キュー/履歴選択、ログスクロール\n")
	sb.WriteString("g / G             ログの先頭/末尾(最新)へジャンプ\n")
	sb.WriteString("1, 2, 3, d, q     承認待ちの判断アクション\n")
	sb.WriteString("y / n             クリップボード/PRの許可・拒否\n")
	sb.WriteString("Q / Ctrl+C        VM を破棄して終了確認\n\n")
	sb.WriteString(styleDim("任意のキーまたは Esc で閉じる"))
	return styleModal.Width(max(40, m.width-6)).Render(sb.String())
}

func (m model) renderFooter() string {
	var hints []string
	hints = append(hints, "[Tab] タブ切替")

	if m.activeTab == tabPending && len(m.pending) > 0 {
		hints = append(hints, "[1/2/3/d/q/y/n] 判定")
	} else if m.activeTab == tabLogs {
		hints = append(hints, "[↑/↓/G] ログ閲覧")
	} else if m.activeTab == tabHistory {
		hints = append(hints, "[↑/↓] 履歴閲覧")
	}

	hints = append(hints, "[?] ヘルプ", "[Q] 終了")
	return styleFooter.Render(strings.Join(hints, "  "))
}

func styleDim(s string) string {
	return lipgloss.NewStyle().Foreground(colDim).Render(s)
}

// RunClient は tmux のペインで動く承認 UI。本体との接続が切れたらつなぎ直す。
func RunClient(sock string) (err error) {
	defer func() {
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

	m := newModel(sock)
	p := tea.NewProgram(m, tea.WithAltScreen())

	quitCh := make(chan struct{})
	defer close(quitCh)

	go runSocketWorker(sock, m.outChan, m.eventChan, quitCh)

	_, err = p.Run()
	return err
}

func runSocketWorker(sock string, outChan <-chan Msg, eventChan chan<- tea.Msg, quitCh <-chan struct{}) {
	for {
		select {
		case <-quitCh:
			return
		default:
		}

		if _, err := os.Stat(sock); err != nil {
			select {
			case eventChan <- socketTerminatedMsg{}:
			case <-quitCh:
			}
			return
		}

		conn, err := net.Dial("unix", sock)
		if err != nil {
			select {
			case eventChan <- socketDisconnectedMsg{}:
			case <-quitCh:
			}
			time.Sleep(time.Second)
			continue
		}

		enc := json.NewEncoder(conn)
		if err := enc.Encode(Msg{Type: "ui"}); err != nil {
			conn.Close()
			time.Sleep(time.Second)
			continue
		}

		select {
		case eventChan <- socketConnectedMsg{}:
		case <-quitCh:
			conn.Close()
			return
		}

		done := make(chan struct{})
		// 送信ルーチン
		go func() {
			for {
				select {
				case <-done:
					return
				case <-quitCh:
					return
				case msg, ok := <-outChan:
					if !ok {
						return
					}
					_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
					if err := enc.Encode(msg); err != nil {
						conn.Close()
						return
					}
				}
			}
		}()

		// 受信ルーチン
		dec := json.NewDecoder(conn)
		for {
			var m Msg
			if err := dec.Decode(&m); err != nil {
				break
			}
			select {
			case eventChan <- socketMsg(m):
			case <-quitCh:
				return
			}
		}

		close(done)
		conn.Close()

		select {
		case eventChan <- socketDisconnectedMsg{}:
		case <-quitCh:
			return
		}

		time.Sleep(time.Second)
	}
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

// truncateRunes は s を最大 n ルーンに切り、切ったら末尾に … を付ける。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
