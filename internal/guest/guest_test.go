package guest

import (
	"bytes"
	"errors"
	"net"
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
