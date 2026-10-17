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

// TestSessionReissue は URL の再発行でシークレットが替わり、既存の Cookie は残ることを
// 確かめる。
func TestSessionReissue(t *testing.T) {
	s, err := newSession("id", "n", "", "opencode", "s", "c")
	if err != nil {
		t.Fatal(err)
	}
	oldSecret := s.secret
	tok, ok := s.Exchange(oldSecret)
	if !ok {
		t.Fatal("交換に失敗した")
	}

	url, err := s.Reissue("http://h:1")
	if err != nil {
		t.Fatal(err)
	}
	newSecret := strings.TrimPrefix(url, "http://h:1/s/")
	if !strings.HasPrefix(url, "http://h:1/s/") || newSecret == oldSecret {
		t.Fatalf("再発行した URL が不正: %q", url)
	}
	// 再発行後は新しいシークレットを案内する。
	if got := s.LinkURL("http://h:1"); got != url {
		t.Errorf("再発行後の LinkURL = %q, want %q", got, url)
	}
	// 以前のシークレットは使えない。
	if _, ok := s.Exchange(oldSecret); ok {
		t.Error("再発行で古いシークレットが無効になっていない")
	}
	// 既存の Cookie は有効なまま。
	if !s.Authorize(tok) {
		t.Error("再発行で既存の Cookie が失効した")
	}
	// 新しいシークレットは交換でき、別の Cookie が発行される。
	tok2, ok := s.Exchange(newSecret)
	if !ok || tok2 == "" {
		t.Fatal("再発行後のシークレットで交換できない")
	}
	if tok2 == tok {
		t.Error("交換のたびに同じトークンが発行されている")
	}
	if !s.Authorize(tok) || !s.Authorize(tok2) {
		t.Error("複数の端末の Cookie が同時に有効でない")
	}
	// 交換後は基点を案内する。
	if strings.Contains(s.LinkURL("http://h:1"), "/s/") {
		t.Errorf("交換後の LinkURL にシークレットが残っている: %q", s.LinkURL("http://h:1"))
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
	// 同じ ID の別セッションは拒否する (作業ディレクトリ名の衝突で先の端末を
	// 追い出さないため)。同じポインタの再追加は無害なので通る。
	if err := r.add(a); err != nil {
		t.Fatalf("同じポインタの再追加: %v", err)
	}
	dup, err := newSession("a", "dup", "", "opencode", "s", "c")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.add(dup); err == nil {
		t.Error("同じ ID の別セッションを登録できてしまう")
	}
	if got, _ := r.get("a"); got != a {
		t.Error("拒否したのに登録が差し替わっている")
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

// TestRegistryRemoveIf は、古い接続の切断処理が後から登録されたセッションを
// 消さないことを確かめる。
func TestRegistryRemoveIf(t *testing.T) {
	r := newRegistry(4)
	old, err := newSession("same", "old", "", "opencode", "s", "c")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.add(old); err != nil {
		t.Fatal(err)
	}
	// 登録中の ID は拒否されるので、差し替えは removeIf の後に限る。
	r.removeIf("same", old)
	newer, err := newSession("same", "newer", "", "opencode", "s", "c")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.add(newer); err != nil {
		t.Fatal(err)
	}
	// 古い接続の切断が遅れて届いても、新しい登録は残る。
	r.removeIf("same", old)
	if got, _ := r.get("same"); got != newer {
		t.Error("古い接続の removeIf が新しいセッションを消した")
	}
	r.removeIf("same", newer)
	if _, ok := r.get("same"); ok {
		t.Error("自分自身の removeIf で消えない")
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
