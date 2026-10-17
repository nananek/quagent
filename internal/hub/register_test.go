package hub

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestServer() *Server {
	return New(Config{Addr: "127.0.0.1:0", BaseURL: "http://127.0.0.1:9999", Logger: func(string) {}})
}

func TestRegisterRoundTrip(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "hub.sock")
	s := newTestServer()
	ln, err := s.listenRegister(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	reg, err := Register(sock, Request{
		ID: "r1", Name: "repo", Repo: "/repo", Agent: "opencode",
		Session: "quagent-r1", Console: "/tmp/c.sock",
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
	if sess.Tmux != "quagent-r1" || sess.Console != "/tmp/c.sock" {
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
	sock := filepath.Join(t.TempDir(), "hub.sock")
	s := newTestServer()
	ln, err := s.listenRegister(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// ID や tmux セッション名が空の登録は拒否される。
	if _, err := Register(sock, Request{ID: "", Session: "s"}, time.Second); err == nil {
		t.Error("ID が空でも登録できてしまう")
	}
	if _, err := Register(sock, Request{ID: "x", Session: ""}, time.Second); err == nil {
		t.Error("セッション名が空でも登録できてしまう")
	}
}

func TestRegisterLimit(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "hub.sock")
	s := newTestServer()
	s.reg = newRegistry(1) // テスト用に上限を 1 にする
	ln, err := s.listenRegister(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	first, err := Register(sock, Request{ID: "a", Session: "sa"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := Register(sock, Request{ID: "b", Session: "sb"}, time.Second); err == nil {
		t.Error("上限を超えて登録できてしまう")
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
