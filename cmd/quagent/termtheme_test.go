package main

import "testing"

func TestResolveTermTheme(t *testing.T) {
	// 明示指定はそのまま。
	if got := resolveTermTheme("light"); got != "light" {
		t.Errorf("resolveTermTheme(light) = %q", got)
	}
	if got := resolveTermTheme("dark"); got != "dark" {
		t.Errorf("resolveTermTheme(dark) = %q", got)
	}
	// auto は COLORFGBG (前景;背景 の ANSI 色番号) から推定する。
	t.Setenv("COLORFGBG", "15;0")
	if got := resolveTermTheme("auto"); got != "dark" {
		t.Errorf("COLORFGBG=15;0 -> %q, want dark", got)
	}
	t.Setenv("COLORFGBG", "0;15")
	if got := resolveTermTheme(""); got != "light" {
		t.Errorf("COLORFGBG=0;15 -> %q, want light", got)
	}
}

func TestParseBackgroundColor(t *testing.T) {
	cases := []struct {
		in      string
		r, g, b int
		ok      bool
	}{
		{"\x1b]11;rgb:f9f9/f8f8/f5f5\x1b\\", 249, 248, 245, true},
		{"\x1b]11;rgb:ff/ff/ff\x07", 255, 255, 255, true},
		{"\x1b]11;rgb:0000/0000/0000\x1b\\", 0, 0, 0, true},
		{"nonsense", 0, 0, 0, false},
		{"\x1b]11;rgb:ff/ff\x1b\\", 0, 0, 0, false},
	}
	for _, c := range cases {
		r, g, b, ok := parseBackgroundColor([]byte(c.in))
		if ok != c.ok || (ok && (r != c.r || g != c.g || b != c.b)) {
			t.Errorf("parseBackgroundColor(%q) = %d,%d,%d,%v", c.in, r, g, b, ok)
		}
	}
}
