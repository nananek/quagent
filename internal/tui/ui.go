package tui

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// form は起動画面の 1 ページ。1 つのフォームに選択肢・複数選択・入力・確認を並べる。
//
// huh のようなフォーム部品はマウスに対応しておらず、描画した行とクリック位置を
// 突き合わせることもできない。ここでは行を自前で組み立て、その行に「どの
// フィールドの何番目の選択肢か」を持たせてクリックを判定する。行の座標は
// 描画した本人が知っているので、当たり判定がずれない。
//
// 描画は代替画面 (alt screen) で行う。マウスの行座標は画面の絶対行なので、
// 端末の途中に描くとフォームの先頭とずれる。代替画面なら左上が (0, 0) になる。
type form struct {
	heading string
	note    string
	fields  []*field
	focus   int
	scroll  int
	// noteScroll は説明が長いときの表示位置 (行)。
	noteScroll int

	width  int
	height int

	done    bool
	back    bool
	aborted bool
	// noBack は Esc で戻らない (戻ると取り返しのつかない画面で使う)。
	noBack bool
}

// formState はフォームがどう終わったか。
type formState int

const (
	// formSubmitted は確定した (最後のフィールドで Enter、確認で「はい」)。
	formSubmitted formState = iota
	// formBack は Esc や確認の「いいえ」で戻った。
	formBack
	// formAborted は Ctrl-C などで中断した。
	formAborted
)

// option は選択肢 1 つ。value は選んだときに変数へ入る値。
type option struct {
	label string
	value string
	// dim は選べるが目立たせない選択肢 (未作成のイメージなど)。
	dim bool
}

type fieldKind int

const (
	fieldSelect fieldKind = iota
	fieldMulti
	fieldInput
	fieldConfirm
)

// field はフォームの 1 項目。
type field struct {
	kind  fieldKind
	title string
	desc  string
	opts  []option

	cursor int
	chosen []bool

	value  *string
	values *[]string
	input  textinput.Model
	flag   *bool

	yes, no string

	validate func(string) error
	err      error
}

var (
	styleHeading   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	styleLabel     = lipgloss.NewStyle().Bold(true)
	styleFocus     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("4"))
	styleDim       = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleCursor    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("4"))
	styleChosen    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("4"))
	styleButtonOn  = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("7"))
	styleButtonOff = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleError     = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleHelp      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// newForm は見出しだけの空のフォームを作る。
func newForm(heading string) *form {
	return &form{heading: heading}
}

// setNote はフォームの先頭に出す説明を入れる (改行可)。
func (f *form) setNote(note string) { f.note = note }

// addSelect は 1 つだけ選ぶ項目を足す。value の現在値からカーソルを合わせる。
func (f *form) addSelect(title, desc string, value *string, opts ...option) *field {
	fl := &field{kind: fieldSelect, title: title, desc: desc, opts: opts, value: value}
	for i, o := range opts {
		if o.value == *value {
			fl.cursor = i
		}
	}
	if len(opts) > 0 {
		*value = opts[fl.cursor].value
	}
	f.fields = append(f.fields, fl)
	return fl
}

// addMulti は 0 個以上選ぶ項目を足す。values の現在値からチェックを入れる。
func (f *form) addMulti(title, desc string, values *[]string, opts ...option) *field {
	fl := &field{kind: fieldMulti, title: title, desc: desc, opts: opts, values: values, chosen: make([]bool, len(opts))}
	for i, o := range opts {
		for _, v := range *values {
			if v == o.value {
				fl.chosen[i] = true
			}
		}
	}
	f.fields = append(f.fields, fl)
	return fl
}

// addInput は文字入力の項目を足す。validate は次へ進むときに呼ぶ。
func (f *form) addInput(title, desc string, value *string, validate func(string) error) *field {
	ti := textinput.New()
	ti.Prompt = ""
	ti.SetValue(*value)
	ti.Blur()
	fl := &field{kind: fieldInput, title: title, desc: desc, value: value, input: ti, validate: validate}
	f.fields = append(f.fields, fl)
	return fl
}

// addConfirm は「はい / いいえ」の項目を足す。
func (f *form) addConfirm(title, desc string, value *bool, yes, no string) *field {
	fl := &field{kind: fieldConfirm, title: title, desc: desc, flag: value, yes: yes, no: no}
	f.fields = append(f.fields, fl)
	return fl
}

// run は代替画面でフォームを動かし、どう終わったかを返す。
func (f *form) run() (formState, error) {
	// 端末が使えない (画面読み上げなど) ときは、1 項目ずつ尋ねる。
	if os.Getenv("TERM") == "dumb" {
		return f.runPlain()
	}
	if len(f.fields) > 0 {
		f.setFocus(0)
	}
	p := tea.NewProgram(f, tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		return formAborted, err
	}
	switch {
	case f.aborted:
		return formAborted, nil
	case f.back:
		return formBack, nil
	case f.done:
		return formSubmitted, nil
	}
	return formAborted, nil
}

func (f *form) Init() tea.Cmd { return nil }

// runPlain は端末の画面を使えないとき (TERM=dumb) に、1 項目ずつ尋ねる。
// 戻る操作は無く、Esc も効かない。
func (f *form) runPlain() (formState, error) {
	in := bufio.NewReader(os.Stdin)
	out := os.Stdout
	if f.heading != "" {
		fmt.Fprintln(out, f.heading)
	}
	if f.note != "" {
		fmt.Fprintln(out, f.note)
	}
	ask := func(prompt string) (string, bool) {
		fmt.Fprint(out, prompt)
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			return "", false
		}
		return strings.TrimSpace(line), true
	}

	for _, fl := range f.fields {
		if fl.title != "" {
			fmt.Fprintln(out, fl.title)
		}
		switch fl.kind {
		case fieldSelect:
			for j, o := range fl.opts {
				fmt.Fprintf(out, "  %d) %s\n", j+1, o.label)
			}
			for {
				s, ok := ask(fmt.Sprintf("番号 [%d]: ", fl.cursor+1))
				if !ok {
					return formAborted, nil
				}
				if s == "" {
					break
				}
				n, err := strconv.Atoi(s)
				if err != nil || n < 1 || n > len(fl.opts) {
					fmt.Fprintln(out, "  番号で入れる")
					continue
				}
				fl.to(n - 1)
				break
			}
		case fieldMulti:
			for j, o := range fl.opts {
				mark := " "
				if fl.chosen[j] {
					mark = "x"
				}
				fmt.Fprintf(out, "  [%s] %d) %s\n", mark, j+1, o.label)
			}
			s, ok := ask("切り替える番号 (空白区切り、空欄で決定): ")
			if !ok {
				return formAborted, nil
			}
			for _, tok := range strings.Fields(s) {
				n, err := strconv.Atoi(tok)
				if err == nil && n >= 1 && n <= len(fl.opts) {
					fl.chosen[n-1] = !fl.chosen[n-1]
				}
			}
			fl.commit()
		case fieldInput:
			for {
				s, ok := ask(fmt.Sprintf("値 [%s]: ", fl.input.Value()))
				if !ok {
					return formAborted, nil
				}
				if s != "" {
					fl.input.SetValue(s)
					fl.commit()
				}
				if err := fl.check(); err != nil {
					fmt.Fprintln(out, "  "+err.Error())
					continue
				}
				break
			}
		case fieldConfirm:
			def := "y/N"
			if fl.flag != nil && *fl.flag {
				def = "Y/n"
			}
			for {
				s, ok := ask(fmt.Sprintf("%s [%s]: ", fl.title, def))
				if !ok {
					return formAborted, nil
				}
				switch strings.ToLower(s) {
				case "":
					break
				case "y", "yes":
					fl.setFlag(true)
				case "n", "no":
					fl.setFlag(false)
				default:
					fmt.Fprintln(out, "  y か n で入れる")
					continue
				}
				break
			}
		}
	}

	for _, fl := range f.fields {
		if err := fl.check(); err != nil {
			fmt.Fprintln(out, "  "+err.Error())
			return formAborted, nil
		}
	}
	f.done = true
	return formSubmitted, nil
}

func (f *form) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		f.width, f.height = msg.Width, msg.Height
		f.ensureVisible()
		return f, nil
	case tea.KeyMsg:
		return f, f.handleKey(msg)
	case tea.MouseMsg:
		return f, f.handleMouse(msg)
	}
	// 入力欄のカーソル点滅など。
	if fl := f.focused(); fl != nil && fl.kind == fieldInput {
		var cmd tea.Cmd
		fl.input, cmd = fl.input.Update(msg)
		fl.commit()
		return f, cmd
	}
	return f, nil
}

func (f *form) handleKey(k tea.KeyMsg) tea.Cmd {
	key := k.String()
	if key == "ctrl+c" {
		f.aborted = true
		return tea.Quit
	}
	// 長い説明は PageUp / PageDown で送る。
	if f.noteOverflow() {
		switch key {
		case "pgup", "ctrl+u":
			f.noteScroll = clamp(f.noteScroll-5, 0, f.noteMaxScroll())
			return nil
		case "pgdown", "ctrl+d":
			f.noteScroll = clamp(f.noteScroll+5, 0, f.noteMaxScroll())
			return nil
		}
	}
	if len(f.fields) == 0 {
		// 説明だけの画面は、Enter かクリックで閉じる。
		if key == "esc" {
			f.back = true
		} else {
			f.done = true
		}
		return tea.Quit
	}

	fl := f.fields[f.focus]
	if key == "esc" {
		if f.noBack {
			return nil
		}
		f.back = true
		return tea.Quit
	}
	switch key {
	case "tab", "enter":
		if fl.kind == fieldInput {
			fl.commit()
		}
		if err := fl.check(); err != nil {
			fl.err = err
			f.ensureVisible()
			return nil
		}
		fl.err = nil
		return f.advance()
	case "shift+tab":
		if fl.kind == fieldInput {
			fl.commit()
		}
		if err := fl.check(); err != nil {
			fl.err = err
			f.ensureVisible()
			return nil
		}
		fl.err = nil
		return f.retreat()
	}

	switch fl.kind {
	case fieldSelect:
		switch key {
		case "up", "k":
			fl.move(-1)
		case "down", "j":
			fl.move(1)
		case "g", "home":
			fl.to(0)
		case "G", "end":
			fl.to(len(fl.opts) - 1)
		}
	case fieldMulti:
		switch key {
		case "up", "k":
			fl.move(-1)
		case "down", "j":
			fl.move(1)
		case "g", "home":
			fl.to(0)
		case "G", "end":
			fl.to(len(fl.opts) - 1)
		case " ", "x":
			fl.toggle()
		}
	case fieldConfirm:
		switch key {
		case "left", "h", "right", "l":
			if fl.flag != nil {
				fl.setFlag(!*fl.flag)
			}
		case "y", "Y":
			fl.setFlag(true)
			return f.submit()
		case "n", "N":
			fl.setFlag(false)
			return f.submit()
		}
	case fieldInput:
		var cmd tea.Cmd
		fl.input, cmd = fl.input.Update(k)
		fl.commit()
		fl.err = nil
		return cmd
	}
	return nil
}

func (f *form) handleMouse(m tea.MouseMsg) tea.Cmd {
	if len(f.fields) == 0 {
		// 説明だけの画面は、クリックで閉じる。
		if m.Button == tea.MouseButtonLeft && m.Action == tea.MouseActionPress {
			f.done = true
			return tea.Quit
		}
		return nil
	}
	lines := f.layout()
	h := f.height
	if h <= 0 {
		h = 24
	}
	avail := max(1, h-1)
	f.scroll = clamp(f.scroll, 0, max(0, len(lines)-avail))

	switch m.Button {
	case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
		up := m.Button == tea.MouseButtonWheelUp
		// 説明の上で回したときは、説明だけを送る。
		y := m.Y + f.scroll
		if f.noteOverflow() && y >= 0 && y < len(lines) && lines[y].kind == lineNote {
			if up {
				f.noteScroll = clamp(f.noteScroll-1, 0, f.noteMaxScroll())
			} else {
				f.noteScroll = clamp(f.noteScroll+1, 0, f.noteMaxScroll())
			}
			return nil
		}
		if len(lines) > avail {
			if up {
				f.scroll -= 3
			} else {
				f.scroll += 3
			}
			f.scroll = clamp(f.scroll, 0, len(lines)-avail)
			return nil
		}
		if fl := f.focused(); fl != nil && (fl.kind == fieldSelect || fl.kind == fieldMulti) {
			if up {
				fl.move(-1)
			} else {
				fl.move(1)
			}
		}
		return nil
	}
	if m.Button != tea.MouseButtonLeft || m.Action != tea.MouseActionPress {
		return nil
	}

	y := m.Y + f.scroll
	if y < 0 || y >= len(lines) {
		return nil
	}
	ln := lines[y]
	if ln.field < 0 || ln.field >= len(f.fields) {
		return nil
	}
	fl := f.fields[ln.field]
	switch ln.kind {
	case lineOption:
		if ln.opt < 0 || ln.opt >= len(fl.opts) {
			return nil
		}
		f.setFocus(ln.field)
		switch fl.kind {
		case fieldSelect:
			fl.to(ln.opt)
		case fieldMulti:
			fl.cursor = ln.opt
			fl.toggle()
		}
	case lineButtons:
		if m.X >= ln.x[0] && m.X < ln.x[1] {
			fl.setFlag(true)
			return f.submit()
		}
		if m.X >= ln.x[2] && m.X < ln.x[3] {
			fl.setFlag(false)
			return f.submit()
		}
	case lineTitle, lineDesc, lineInput, lineError:
		f.setFocus(ln.field)
	}
	return nil
}

// advance は次の項目へ進む。最後の項目なら確定する。
func (f *form) advance() tea.Cmd {
	if f.focus >= len(f.fields)-1 {
		return f.submit()
	}
	f.setFocus(f.focus + 1)
	return nil
}

func (f *form) retreat() tea.Cmd {
	if f.focus <= 0 {
		return nil
	}
	f.setFocus(f.focus - 1)
	return nil
}

// submit は全項目を検証してから確定する。マウスで項目を飛ばしても、
// 入力の検証はここで必ず通る。
func (f *form) submit() tea.Cmd {
	for i, fl := range f.fields {
		if err := fl.check(); err != nil {
			fl.err = err
			f.setFocus(i)
			f.ensureVisible()
			return nil
		}
		fl.err = nil
	}
	f.done = true
	return tea.Quit
}

func (f *form) focused() *field {
	if f.focus >= 0 && f.focus < len(f.fields) {
		return f.fields[f.focus]
	}
	return nil
}

func (f *form) setFocus(i int) {
	if i < 0 || i >= len(f.fields) {
		return
	}
	if cur := f.focused(); cur != nil && cur.kind == fieldInput {
		cur.input.Blur()
	}
	f.focus = i
	if fl := f.fields[i]; fl.kind == fieldInput {
		fl.input.Focus()
	}
	f.ensureVisible()
}

// ---- フィールドの操作 ----

func (fl *field) to(i int) {
	if i < 0 || i >= len(fl.opts) {
		return
	}
	fl.cursor = i
	fl.commit()
}

func (fl *field) move(d int) {
	n := len(fl.opts)
	if n == 0 {
		return
	}
	fl.cursor = (fl.cursor + d + n) % n
	fl.commit()
}

func (fl *field) toggle() {
	if fl.cursor < 0 || fl.cursor >= len(fl.chosen) {
		return
	}
	fl.chosen[fl.cursor] = !fl.chosen[fl.cursor]
	fl.commit()
}

func (fl *field) setFlag(v bool) {
	if fl.flag != nil {
		*fl.flag = v
	}
}

// commit はカーソルやチェックの状態を、呼び出し側の変数へ書き戻す。
func (fl *field) commit() {
	switch fl.kind {
	case fieldSelect:
		if fl.value != nil && fl.cursor >= 0 && fl.cursor < len(fl.opts) {
			*fl.value = fl.opts[fl.cursor].value
		}
	case fieldMulti:
		picked := make([]string, 0, len(fl.opts))
		for i, o := range fl.opts {
			if fl.chosen[i] {
				picked = append(picked, o.value)
			}
		}
		if fl.values != nil {
			*fl.values = picked
		}
	case fieldInput:
		if fl.value != nil {
			*fl.value = fl.input.Value()
		}
	}
}

func (fl *field) check() error {
	fl.commit()
	if fl.validate == nil {
		return nil
	}
	return fl.validate(fl.input.Value())
}

// summary はフォーカスしていないときに出す現在値。
func (fl *field) summary() string {
	switch fl.kind {
	case fieldSelect:
		if fl.cursor >= 0 && fl.cursor < len(fl.opts) {
			return fl.opts[fl.cursor].label
		}
	case fieldMulti:
		var labels []string
		for i, o := range fl.opts {
			if fl.chosen[i] {
				labels = append(labels, o.label)
			}
		}
		if len(labels) == 0 {
			return "(なし)"
		}
		return strings.Join(labels, ", ")
	case fieldInput:
		if fl.input.Value() == "" {
			return "(未入力)"
		}
		return fl.input.Value()
	case fieldConfirm:
		if fl.flag != nil && *fl.flag {
			return fl.yes
		}
		return fl.no
	}
	return ""
}

// ---- 描画 ----

type lineKind int

const (
	lineHeading lineKind = iota
	lineNote
	lineBlank
	lineTitle
	lineDesc
	lineOption
	lineInput
	lineButtons
	lineError
)

// formLine は描画する 1 行と、クリックされたときの行き先。
type formLine struct {
	text  string
	kind  lineKind
	field int
	opt   int
	// x は確認ボタンの当たり判定 (x[0]..x[1] がはい、x[2]..x[3] がいいえ)。
	x [4]int
}

func newLine(text string, kind lineKind, field, opt int) formLine {
	return formLine{text: text, kind: kind, field: field, opt: opt, x: [4]int{-1, -1, -1, -1}}
}

func (f *form) layout() []formLine {
	w := f.width
	if w <= 0 {
		w = 80
	}
	var out []formLine
	if f.heading != "" {
		out = append(out, newLine(styleHeading.Render(f.heading), lineHeading, -1, 0))
	}
	if f.note != "" {
		all := wrapWords(f.note, w)
		win := f.noteWindow()
		f.noteScroll = clamp(f.noteScroll, 0, max(0, len(all)-win))
		end := min(len(all), f.noteScroll+win)
		for _, l := range all[f.noteScroll:end] {
			out = append(out, newLine(l, lineNote, -1, 0))
		}
		if end < len(all) {
			out = append(out, newLine(styleDim.Render("…"), lineNote, -1, 0))
		}
	}
	if len(out) > 0 {
		out = append(out, newLine("", lineBlank, -1, 0))
	}

	const indent = "  "
	for i, fl := range f.fields {
		focused := i == f.focus
		if fl.title != "" {
			if focused {
				out = append(out, newLine(indent+styleFocus.Render("❯ "+fl.title), lineTitle, i, 0))
			} else {
				out = append(out, newLine(indent+styleLabel.Render(fl.title), lineTitle, i, 0))
			}
		}
		if focused && fl.desc != "" {
			for _, l := range wrapWords(fl.desc, w-4) {
				out = append(out, newLine(indent+indent+styleDim.Render(l), lineDesc, i, 0))
			}
		}

		switch fl.kind {
		case fieldSelect:
			if !focused {
				for _, l := range wrapWords(fl.summary(), w-4) {
					out = append(out, newLine(indent+indent+styleDim.Render(l), lineDesc, i, 0))
				}
				break
			}
			for j, o := range fl.opts {
				marker := "  "
				if j == fl.cursor {
					marker = styleCursor.Render("❯ ")
				}
				style := lipgloss.NewStyle()
				if o.dim {
					style = styleDim
				} else if j == fl.cursor {
					style = styleChosen
				}
				for k, l := range wrapWords(o.label, w-4) {
					lead := indent + marker
					if k > 0 {
						lead = indent + "  "
					}
					out = append(out, newLine(lead+style.Render(l), lineOption, i, j))
				}
			}

		case fieldMulti:
			if !focused {
				for _, l := range wrapWords(fl.summary(), w-4) {
					out = append(out, newLine(indent+indent+styleDim.Render(l), lineDesc, i, 0))
				}
				break
			}
			for j, o := range fl.opts {
				marker := "  "
				if j == fl.cursor {
					marker = styleCursor.Render("❯ ")
				}
				box := "[ ] "
				style := lipgloss.NewStyle()
				if fl.chosen[j] {
					box = "[x] "
					style = styleChosen
				}
				if o.dim {
					style = styleDim
				}
				for k, l := range wrapWords(o.label, w-8) {
					lead := indent + marker + box
					if k > 0 {
						lead = indent + "    "
					}
					out = append(out, newLine(lead+style.Render(l), lineOption, i, j))
				}
			}

		case fieldInput:
			if focused {
				fl.input.Width = max(8, w-6)
				out = append(out, newLine(indent+"> "+fl.input.View(), lineInput, i, 0))
			} else {
				for _, l := range wrapWords(fl.summary(), w-4) {
					out = append(out, newLine(indent+indent+styleDim.Render(l), lineDesc, i, 0))
				}
			}

		case fieldConfirm:
			if focused {
				yesStyle, noStyle := styleButtonOff, styleButtonOn
				if fl.flag != nil && *fl.flag {
					yesStyle, noStyle = styleButtonOn, styleButtonOff
				}
				yesText := "[ " + fl.yes + " ]"
				noText := "[ " + fl.no + " ]"
				x0 := runewidth.StringWidth(indent)
				x1 := x0 + runewidth.StringWidth(yesText)
				x2 := x1 + 2
				x3 := x2 + runewidth.StringWidth(noText)
				ln := newLine(indent+yesStyle.Render(yesText)+"  "+noStyle.Render(noText), lineButtons, i, 0)
				ln.x = [4]int{x0, x1, x2, x3}
				out = append(out, ln)
			} else {
				for _, l := range wrapWords(fl.summary(), w-4) {
					out = append(out, newLine(indent+indent+styleDim.Render(l), lineDesc, i, 0))
				}
			}
		}

		if fl.err != nil {
			for _, l := range wrapWords(fl.err.Error(), w-4) {
				out = append(out, newLine(indent+indent+styleError.Render(l), lineError, i, 0))
			}
		}
		if i < len(f.fields)-1 {
			out = append(out, newLine("", lineBlank, i, 0))
		}
	}
	return out
}

func (f *form) View() string {
	lines := f.layout()
	h := f.height
	if h <= 0 {
		h = 24
	}
	avail := max(1, h-1)
	f.scroll = clamp(f.scroll, 0, max(0, len(lines)-avail))
	start := f.scroll
	end := min(len(lines), start+avail)

	var b strings.Builder
	for _, ln := range lines[start:end] {
		b.WriteString(ln.text)
		b.WriteString("\n")
	}
	for i := end - start; i < avail; i++ {
		b.WriteString("\n")
	}
	b.WriteString(styleHelp.Render(truncate(f.help(), f.widthOr80())))
	return b.String()
}

func (f *form) help() string {
	if len(f.fields) == 0 {
		return "Enter / クリック: 閉じる    Ctrl-C: 終了"
	}
	type hint struct {
		text     string
		optional bool
	}
	var hints []hint
	switch f.fields[f.focus].kind {
	case fieldSelect:
		hints = append(hints, hint{"↑↓: 選ぶ", true}, hint{"Enter: 決定", false}, hint{"クリック: 選択", true})
	case fieldMulti:
		hints = append(hints, hint{"↑↓: 移動", true}, hint{"Space: 選択", true}, hint{"Enter: 決定", false}, hint{"クリック: 選択", true})
	case fieldInput:
		hints = append(hints, hint{"Enter: 次へ", false}, hint{"Tab: 移動", true})
	case fieldConfirm:
		hints = append(hints, hint{"←→: 切り替え", true}, hint{"y / n: 決定", false}, hint{"クリック: 選択", true})
	}
	if !f.noBack {
		hints = append(hints, hint{"Esc: 戻る", false})
	}
	hints = append(hints, hint{"Ctrl-C: 終了", false})
	if f.noteOverflow() {
		hints = append(hints, hint{"PgUp/PgDn: 説明", true})
	}

	w := f.widthOr80()
	render := func() string {
		parts := make([]string, len(hints))
		for i, h := range hints {
			parts[i] = h.text
		}
		return strings.Join(parts, "    ")
	}
	// 幅に入らない注記から落とす (決定に要るものは残す)。
	for runewidth.StringWidth(render()) > w {
		drop := -1
		for i := len(hints) - 1; i >= 0; i-- {
			if hints[i].optional {
				drop = i
				break
			}
		}
		if drop < 0 {
			break
		}
		hints = append(hints[:drop], hints[drop+1:]...)
	}
	return truncate(render(), w)
}

func (f *form) widthOr80() int {
	if f.width > 0 {
		return f.width
	}
	return 80
}

// noteWindow は説明を一度に見せる行数。長い説明で選択肢が画面外へ出ないよう、
// 端末の高さの 1/3 までにする。
func (f *form) noteWindow() int {
	h := f.height
	if h <= 0 {
		h = 24
	}
	return clamp(h/3, 3, 12)
}

func (f *form) noteMaxScroll() int {
	if f.note == "" {
		return 0
	}
	return max(0, len(wrapWords(f.note, f.widthOr80()))-f.noteWindow())
}

func (f *form) noteOverflow() bool { return f.noteMaxScroll() > 0 }

// ensureVisible はフォーカス中の項目が画面に入るようスクロールを合わせる。
func (f *form) ensureVisible() {
	if f.height <= 0 {
		return
	}
	lines := f.layout()
	avail := max(1, f.height-1)
	first, last := -1, -1
	for i, ln := range lines {
		if ln.field == f.focus && ln.kind != lineBlank {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		return
	}
	if first < f.scroll {
		f.scroll = first
	}
	if last >= f.scroll+avail {
		f.scroll = last - avail + 1
	}
	f.scroll = clamp(f.scroll, 0, max(0, len(lines)-avail))
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// wrapWords は表示幅 w で折り返す。日本語のように空白で区切られない文でも
// 崩れないよう、文字の表示幅で切る。
func wrapWords(s string, w int) []string {
	if w < 1 {
		w = 1
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line == "" {
			out = append(out, "")
			continue
		}
		var b strings.Builder
		col := 0
		for _, r := range line {
			rw := runewidth.RuneWidth(r)
			if col+rw > w && col > 0 {
				out = append(out, b.String())
				b.Reset()
				col = 0
			}
			b.WriteRune(r)
			col += rw
		}
		out = append(out, b.String())
	}
	return out
}

// truncate は表示幅 w に収まるよう末尾を切る。
func truncate(s string, w int) string {
	if runewidth.StringWidth(s) <= w {
		return s
	}
	var b strings.Builder
	col := 0
	for _, r := range s {
		rw := runewidth.RuneWidth(r)
		if col+rw > w {
			break
		}
		b.WriteRune(r)
		col += rw
	}
	return b.String()
}
