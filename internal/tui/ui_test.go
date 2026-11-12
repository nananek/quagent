package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// testForm は幅 80・指定の高さのフォームを作る (実際の端末は開かない)。
func testForm(t *testing.T, height int) *form {
	t.Helper()
	f := newForm("")
	f.Update(tea.WindowSizeMsg{Width: 80, Height: height})
	return f
}

// findLine は layout の中で条件に合う最初の行を返す。
func findLine(f *form, kind lineKind, field, opt int) int {
	for i, ln := range f.layout() {
		if ln.kind == kind && ln.field == field && ln.opt == opt {
			return i
		}
	}
	return -1
}

// clickLine は layout の行 i をクリックする。画面に映っている行に直す。
func clickLine(f *form, i int) tea.Cmd {
	_, cmd := f.Update(tea.MouseMsg{
		Y: i - f.scroll, X: 4,
		Button: tea.MouseButtonLeft, Action: tea.MouseActionPress,
	})
	return cmd
}

func TestFormSelectByMouse(t *testing.T) {
	var value string
	f := newForm("")
	f.addSelect("渡し方", "", &value,
		option{label: "git", value: "git"},
		option{label: "copy", value: "copy"},
	)
	f.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if value != "git" {
		t.Fatalf("初期値 = %q, want git", value)
	}

	i := findLine(f, lineOption, 0, 1)
	if i < 0 {
		t.Fatal("選択肢の行が見つからない")
	}
	clickLine(f, i)
	if value != "copy" {
		t.Errorf("クリック後 = %q, want copy", value)
	}
	if f.focus != 0 {
		t.Errorf("focus = %d, want 0", f.focus)
	}
}

func TestFormMultiByMouse(t *testing.T) {
	var values []string
	f := newForm("")
	f.addMulti("オプション", "", &values,
		option{label: "tmp", value: "tmp"},
		option{label: "nested", value: "nested"},
	)
	f.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	i := findLine(f, lineOption, 0, 0)
	if i < 0 {
		t.Fatal("選択肢の行が見つからない")
	}
	clickLine(f, i)
	if len(values) != 1 || values[0] != "tmp" {
		t.Fatalf("クリック後 = %v, want [tmp]", values)
	}
	clickLine(f, i)
	if len(values) != 0 {
		t.Errorf("もう一度クリックした後 = %v, want []", values)
	}
}

func TestFormConfirmByMouse(t *testing.T) {
	for _, tc := range []struct {
		name string
		x    func(formLine) int
		want bool
	}{
		{"はい", func(ln formLine) int { return ln.x[0] }, true},
		{"いいえ", func(ln formLine) int { return ln.x[2] }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok := !tc.want
			f := newForm("")
			f.addConfirm("起動する?", "", &ok, "起動", "戻る")
			f.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

			i := findLine(f, lineButtons, 0, 0)
			if i < 0 {
				t.Fatal("ボタンの行が見つからない")
			}
			ln := f.layout()[i]
			f.Update(tea.MouseMsg{
				Y: i - f.scroll, X: tc.x(ln),
				Button: tea.MouseButtonLeft, Action: tea.MouseActionPress,
			})
			if ok != tc.want {
				t.Errorf("ok = %v, want %v", ok, tc.want)
			}
			if !f.done {
				t.Error("確定していない")
			}
		})
	}
}

func TestFormInputValidation(t *testing.T) {
	value := ""
	f := newForm("")
	f.addInput("CPU", "", &value, positiveInt)
	f.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	f.setFocus(0)

	// 空のまま Enter すると、エラーを出してその場に留まる。
	f.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if f.done {
		t.Fatal("空の入力で確定してしまった")
	}
	if f.fields[0].err == nil {
		t.Fatal("エラーが出ていない")
	}

	// 正しい値を入れると確定できる。
	for _, r := range "4" {
		f.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	f.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !f.done {
		t.Error("確定していない")
	}
	if value != "4" {
		t.Errorf("value = %q, want 4", value)
	}
}

func TestFormTabMovesFocus(t *testing.T) {
	a, b := "", ""
	f := newForm("")
	f.addInput("A", "", &a, nil)
	f.addInput("B", "", &b, nil)
	f.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	f.Update(tea.KeyMsg{Type: tea.KeyTab})
	if f.focus != 1 {
		t.Fatalf("tab 後 focus = %d, want 1", f.focus)
	}
	f.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	if f.focus != 0 {
		t.Errorf("shift+tab 後 focus = %d, want 0", f.focus)
	}
}

func TestFormNoteOnlyCloses(t *testing.T) {
	f := newForm("お知らせ")
	f.setNote("説明だけの画面。")
	f.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	f.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !f.done {
		t.Error("Enter で閉じない")
	}
}

func TestFormMouseClickNoteCloses(t *testing.T) {
	f := newForm("お知らせ")
	f.setNote("説明だけの画面。")
	f.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	f.Update(tea.MouseMsg{Y: 1, X: 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if !f.done {
		t.Error("クリックで閉じない")
	}
}

func TestFormKeyboardSelect(t *testing.T) {
	value := "a"
	f := newForm("")
	f.addSelect("", "", &value,
		option{label: "a", value: "a"},
		option{label: "b", value: "b"},
	)
	f.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	f.Update(tea.KeyMsg{Type: tea.KeyDown})
	if value != "b" {
		t.Errorf("down 後 = %q, want b", value)
	}
	f.Update(tea.KeyMsg{Type: tea.KeyDown})
	if value != "a" {
		t.Errorf("末尾から down 後 = %q, want a", value)
	}
}

func TestFormScrollKeepsFocusVisible(t *testing.T) {
	f := newForm("")
	values := make([]string, 12)
	for i := range values {
		name := string(rune('a' + i))
		f.addInput(name, "説明", &values[i], nil)
	}
	f.Update(tea.WindowSizeMsg{Width: 80, Height: 10})

	for range values[1:] {
		f.Update(tea.KeyMsg{Type: tea.KeyTab})
	}
	if f.focus != len(values)-1 {
		t.Fatalf("focus = %d, want %d", f.focus, len(values)-1)
	}
	lines := f.layout()
	avail := 9
	first, last := -1, -1
	for i, ln := range lines {
		if ln.field == f.focus {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < f.scroll || last >= f.scroll+avail {
		t.Errorf("フォーカス中の項目が画面外: scroll=%d first=%d last=%d", f.scroll, first, last)
	}
}

func TestFormNoteScroll(t *testing.T) {
	f := newForm("")
	var note []string
	for i := 0; i < 40; i++ {
		note = append(note, "行")
	}
	f.setNote(strings.Join(note, "\n"))
	f.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if !f.noteOverflow() {
		t.Fatal("説明が長いのにスクロール扱いになっていない")
	}
	f.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	if f.noteScroll == 0 {
		t.Error("PageDown で説明がスクロールしない")
	}
}
