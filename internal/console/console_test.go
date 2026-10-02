package console

import "testing"

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"pip install":                 "pip install",
		"a\x1b[2J\x1b]52;c;aGk=\x07b": `a\u001b[2J\u001b]52;c;aGk=\u0007b`,
		"line1\nline2":                "line1\nline2",
		"\u202eevil":                  `\u202eevil`,
		"\u009b31m":                   `\u009b31m`,
		"日本語の理由":                      "日本語の理由",
		"bad\xffutf8":                 `bad\ufffdutf8`,
	}
	for in, want := range cases {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}
