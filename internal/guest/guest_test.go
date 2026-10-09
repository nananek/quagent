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

func TestProtoReadWriteFrame(t *testing.T) {
	var buf bytes.Buffer
	fw := &frameWriter{w: &buf}

	// 1. 正常なフレーム書き込みと読み込み
	if err := fw.write(fStdout, []byte("hello world")); err != nil {
		t.Fatal(err)
	}
	tType, data, err := readFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if tType != fStdout || string(data) != "hello world" {
		t.Fatalf("got (%d, %q), want (%d, 'hello world')", tType, data, fStdout)
	}

	// 2. 枠上限を超えたサイズ
	var overBuf bytes.Buffer
	var hdr [5]byte
	hdr[0] = fStdout
	// maxFrame (1<<20) + 1
	hdr[1] = 0x00
	hdr[2] = 0x10
	hdr[3] = 0x00
	hdr[4] = 0x01
	overBuf.Write(hdr[:])
	if _, _, err := readFrame(&overBuf); err == nil || !strings.Contains(err.Error(), "枠が大きすぎる") {
		t.Fatalf("expected overflow error, got %v", err)
	}
}

func TestFrameStream(t *testing.T) {
	var buf bytes.Buffer
	fw := &frameWriter{w: &buf}
	stream := frameStream{fw: fw, t: fStderr}

	n, err := stream.Write([]byte("error output"))
	if err != nil || n != 12 {
		t.Fatalf("Write() = %d, %v", n, err)
	}
	tType, data, err := readFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if tType != fStderr || string(data) != "error output" {
		t.Fatalf("got (%d, %q), want (%d, 'error output')", tType, data, fStderr)
	}
}

type mockAddrConn struct {
	net.Conn
	addr net.Addr
}

func (c *mockAddrConn) RemoteAddr() net.Addr { return c.addr }

type mockGuestListener struct {
	conns chan net.Conn
}

func (m *mockGuestListener) Accept() (net.Conn, error) {
	c, ok := <-m.conns
	if !ok {
		return nil, net.ErrClosed
	}
	return c, nil
}
func (m *mockGuestListener) Close() error   { close(m.conns); return nil }
func (m *mockGuestListener) Addr() net.Addr { return &vsock.Addr{ContextID: 2} }

func TestHostOnlyVsockAddr(t *testing.T) {
	ml := &mockGuestListener{conns: make(chan net.Conn, 5)}
	l := &hostOnly{ml}

	p1, p2 := net.Pipe()
	p3, p4 := net.Pipe()
	defer p1.Close()
	defer p3.Close()

	// 1. CID 3 (Host 以外) -> 拒否
	ml.conns <- &mockAddrConn{Conn: p2, addr: &vsock.Addr{ContextID: 3}}
	// 2. CID 2 (vsock.Host) -> 受付
	ml.conns <- &mockAddrConn{Conn: p4, addr: &vsock.Addr{ContextID: vsock.Host}}

	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if a, ok := c.RemoteAddr().(*vsock.Addr); !ok || a.ContextID != vsock.Host {
		t.Fatalf("expected vsock.Host conn, got %v", c.RemoteAddr())
	}
}

func TestServeMaxConns(t *testing.T) {
	ml := &mockGuestListener{conns: make(chan net.Conn, 70)}
	defer ml.Close()

	go func() {
		_ = serve(ml)
	}()

	// maxGuestConns (64) 本の接続を流す (handle 内でヘッダ待ちになる)
	conns := make([]net.Conn, maxGuestConns)
	for i := 0; i < maxGuestConns; i++ {
		c1, c2 := net.Pipe()
		conns[i] = c1
		ml.conns <- c2
	}

	// 65本目を流す -> セマフォ満杯のため即座に切断される
	c65a, c65b := net.Pipe()
	ml.conns <- c65b

	buf := make([]byte, 1)
	_, err := c65a.Read(buf)
	if err == nil {
		t.Fatal("expected 65th connection to be closed immediately")
	}

	for _, c := range conns {
		_ = c.Close()
	}
	_ = c65a.Close()
}
