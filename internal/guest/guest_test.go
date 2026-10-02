package guest

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unix socket の上で受け口を動かし、Dial をそこへ向ける。
func startServer(t *testing.T) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "g.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go serve(l)
	orig := Dial
	Dial = func(uint32) (net.Conn, error) { return net.Dial("unix", sock) }
	t.Cleanup(func() { Dial = orig })
}

func TestExecPipes(t *testing.T) {
	startServer(t)
	var out, errb bytes.Buffer
	err := Exec(3, Header{Argv: []string{"sh", "-c", "cat; echo err >&2; exit 3"}},
		strings.NewReader("hello\n"), &out, &errb)
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 3 {
		t.Fatalf("終了コードが伝わらない: %v", err)
	}
	if out.String() != "hello\n" || errb.String() != "err\n" {
		t.Fatalf("stdout=%q stderr=%q", out.String(), errb.String())
	}
}

func TestExecLargeInput(t *testing.T) {
	startServer(t)
	in := bytes.Repeat([]byte("0123456789abcdef"), 1<<17) // 2 MiB (枠の上限を超える)
	var out bytes.Buffer
	if err := Exec(3, Header{Argv: []string{"wc", "-c"}}, bytes.NewReader(in), &out, nil); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "2097152" {
		t.Fatalf("届いた量が違う: %q", out.String())
	}
}

func TestExecNoSuchCommand(t *testing.T) {
	startServer(t)
	err := Exec(3, Header{Argv: []string{"/nonexistent"}}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "起動できない") {
		t.Fatalf("起動失敗が伝わらない: %v", err)
	}
}

func TestExecTTY(t *testing.T) {
	startServer(t)
	var out bytes.Buffer
	err := Exec(3, Header{Argv: []string{"sh", "-c", "tty >/dev/null && stty size"}, TTY: true, Rows: 33, Cols: 77},
		nil, &out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "33 77") {
		t.Fatalf("端末サイズが伝わらない: %q", out.String())
	}
}

func TestHostOnlyRejectsOthers(t *testing.T) {
	l, err := net.Listen("unix", filepath.Join(t.TempDir(), "h.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go serve(&hostOnly{l})
	c, err := net.Dial("unix", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte(`{"argv":["true"]}` + "\n"))
	if n, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatalf("vsock の host 以外からの接続に %d バイト応答した", n)
	}
}

func TestReadLineBounded(t *testing.T) {
	br := bufio.NewReader(strings.NewReader(strings.Repeat("a", 100) + "\n"))
	if _, err := readLine(br, 50); err == nil {
		t.Fatal("上限を超えたヘッダを受け付けた")
	}
	br = bufio.NewReader(strings.NewReader(strings.Repeat("a", 10000) + "\nrest"))
	line, err := readLine(br, 1<<20)
	if err != nil || len(line) != 10001 {
		t.Fatalf("len=%d err=%v", len(line), err)
	}
}

func TestReadEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locale.conf")
	if err := os.WriteFile(path, []byte("# c\nLANG=\"en_US.UTF-8\"\nLC_TIME=C\nPATH=/evil\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"LANG": "C.UTF-8", "PATH": "/bin"}
	readEnvFile(path, vars, func(k string) bool { return k == "LANG" || strings.HasPrefix(k, "LC_") })
	if vars["LANG"] != "en_US.UTF-8" || vars["LC_TIME"] != "C" || vars["PATH"] != "/bin" {
		t.Fatalf("vars = %v", vars)
	}
}
