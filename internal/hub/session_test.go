package hub

import (
	"strings"
	"testing"
)

func TestSessionExchangeOnce(t *testing.T) {
	s, err := newSession("id1", "name", "/repo", "opencode", "quagent-x", "/tmp/c.sock")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := s.URL("http://h:1/"), "http://h:1/s/"+s.secret; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
	// 交換前は基底 URL ではなくワンタイム URL を案内する。
	if !strings.Contains(s.LinkURL("http://h:1"), "/s/") {
		t.Errorf("交換前の LinkURL: %q", s.LinkURL("http://h:1"))
	}

	tok, ok := s.Exchange(s.secret)
	if !ok || tok == "" {
		t.Fatal("交換に失敗した")
	}
	if _, ok := s.Exchange(s.secret); ok {
		t.Error("同じシークレットで二度交換できてしまう")
	}
	if !s.Authorize(tok) {
		t.Error("交換後のトークンで認可できない")
	}
	if s.Authorize("bogus") {
		t.Error("不正なトークンで認可できてしまう")
	}
	if s.Authorize(tok + "x") {
		t.Error("長さ違いのトークンで認可できてしまう")
	}
	if strings.Contains(s.LinkURL("http://h:1"), "/s/") {
		t.Errorf("交換後の LinkURL にシークレットが残っている: %q", s.LinkURL("http://h:1"))
	}
}

func TestSessionNoExchangeWithoutMatch(t *testing.T) {
	s, err := newSession("id", "n", "", "opencode", "s", "c")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Exchange("wrong-secret"); ok {
		t.Fatal("一致しないシークレットで交換できてしまう")
	}
	if s.Authorize("anything") {
		t.Fatal("未交換なのに認可できてしまう")
	}
}

func TestRegistryLimitsAndLookup(t *testing.T) {
	r := newRegistry(2)
	mk := func(id string) *Session {
		s, err := newSession(id, id, "", "opencode", "s", "c")
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	a, b, c := mk("a"), mk("b"), mk("c")
	if err := r.add(a); err != nil {
		t.Fatal(err)
	}
	if err := r.add(b); err != nil {
		t.Fatal(err)
	}
	if err := r.add(c); err == nil {
		t.Fatal("上限を超えて登録できてしまう")
	}
	// 同じ ID は上書きできる (上限に数えない)。
	if err := r.add(a); err != nil {
		t.Fatalf("同じ ID の上書き: %v", err)
	}
	if got := r.bySecret(a.secret); got != a {
		t.Error("bySecret で引けない")
	}
	if r.bySecret("nope") != nil {
		t.Error("存在しないシークレットで引けてしまう")
	}
	tok, ok := a.Exchange(a.secret)
	if !ok || tok == "" {
		t.Fatal("交換に失敗した")
	}
	if r.byCookie("nope") != nil {
		t.Error("存在しない Cookie で引けてしまう")
	}
	r.remove(a.ID)
	if _, ok := r.get("a"); ok {
		t.Error("remove されていない")
	}
}

func TestHostOnly(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:8787":              "127.0.0.1",
		"http://host.tail.ts.net:443": "host.tail.ts.net",
		"host.tail.ts.net":            "host.tail.ts.net",
		"[::1]:8787":                  "[::1]",
		"":                            "",
	}
	for in, want := range cases {
		if got := hostOnly(in); got != want {
			t.Errorf("hostOnly(%q) = %q, want %q", in, got, want)
		}
	}
}
