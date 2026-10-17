package hub

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nananek/quagent/internal/paths"
)

func newTestServer() *Server {
	return New(Config{Addr: "127.0.0.1:0", BaseURL: "http://127.0.0.1:9999", Logger: func(string) {}})
}

// testConsolePath は run の作業ディレクトリ配下に console.sock のパスを作る。
func testConsolePath(t *testing.T, name string) string {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	p := filepath.Join(paths.RunsDir(), name, "console.sock")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRegisterRoundTrip(t *testing.T) {
	console := testConsolePath(t, "r1")
	sock := filepath.Join(t.TempDir(), "hub.sock")
	s := newTestServer()
	ln, err := s.listenRegister(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	reg, err := Register(sock, Request{
		ID: "r1", Name: "repo", Repo: "/repo", Agent: "opencode",
		Session: "quagent-r1", Console: console,
	}, 2*time.Second)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	sess, ok := s.reg.get("r1")
	if !ok {
		t.Fatal("登録されていない")
	}
	if !strings.HasPrefix(reg.URL(), "http://127.0.0.1:9999/s/") {
		t.Errorf("URL: %q", reg.URL())
	}
	if reg.URL() != sess.URL("http://127.0.0.1:9999") {
		t.Errorf("URL が一致しない: %q", reg.URL())
	}
	if sess.Tmux != "quagent-r1" || sess.Console != console {
		t.Errorf("メタデータ: %+v", sess)
	}

	// 接続を切ると登録が解除される。
	if err := reg.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := s.reg.get("r1"); !ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("切断後も登録が残っている")
}

func TestRegisterRejectsInvalid(t *testing.T) {
	console := testConsolePath(t, "r")
	sock := filepath.Join(t.TempDir(), "hub.sock")
	s := newTestServer()
	ln, err := s.listenRegister(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	cases := map[string]Request{
		"ID が空":         {ID: "", Session: "quagent-x", Console: console},
		"セッションが空":       {ID: "x", Session: "", Console: console},
		"セッション名が不正":     {ID: "x", Session: "evil; rm -rf /", Console: console},
		"console が別パス":  {ID: "x", Session: "quagent-x", Console: "/tmp/other.sock"},
		"console が外を指す": {ID: "x", Session: "quagent-x", Console: "/etc/passwd"},
		"console が空":    {ID: "x", Session: "quagent-x", Console: ""},
	}
	for name, req := range cases {
		if _, err := Register(sock, req, time.Second); err == nil {
			t.Errorf("%s: 登録できてしまう", name)
		}
	}
}

func TestRegisterLimit(t *testing.T) {
	console := testConsolePath(t, "limit")
	sock := filepath.Join(t.TempDir(), "hub.sock")
	s := newTestServer()
	s.reg = newRegistry(1) // テスト用に上限を 1 にする
	ln, err := s.listenRegister(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	first, err := Register(sock, Request{ID: "a", Session: "quagent-a", Console: console}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := Register(sock, Request{ID: "b", Session: "quagent-b", Console: console}, time.Second); err == nil {
		t.Error("上限を超えて登録できてしまう")
	}
}

func TestValidConsolePath(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := paths.RunsDir()
	ok := filepath.Join(root, "20260101-000000-1", "console.sock")
	if !validConsolePath(ok) {
		t.Errorf("正しいパスを拒否した: %s", ok)
	}
	for _, p := range []string{
		"",
		"/tmp/console.sock",
		"/etc/passwd",
		root,
		filepath.Join(root, "..", "console.sock"),
		filepath.Join(root, "x", "other.sock"),
	} {
		if validConsolePath(p) {
			t.Errorf("不正なパスを通した: %q", p)
		}
	}
}

func TestValidSessionName(t *testing.T) {
	for _, ok := range []string{"quagent-20260101-000000-1", "quagent-x"} {
		if !validSessionName(ok) {
			t.Errorf("正しい名前を拒否した: %s", ok)
		}
	}
	for _, ng := range []string{"", "quagent-", "other-x", "quagent-a; rm -rf /", "quagent-a b"} {
		if validSessionName(ng) {
			t.Errorf("不正な名前を通した: %q", ng)
		}
	}
}

func TestCleanMeta(t *testing.T) {
	if got := cleanMeta("a\x1b[2Jb\nc", 100); got != "a[2Jb c" {
		t.Errorf("control chars: %q", got)
	}
	if got := cleanMeta("abcdef", 3); got != "abc…" {
		t.Errorf("truncate: %q", got)
	}
}
