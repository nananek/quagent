package guest

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRelay_NoSessionLeader(t *testing.T) {
	orig := GetSessionPID()
	defer SetSessionPID(orig)
	SetSessionPID(0)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	dialCalled := false
	go relay(l, func() (net.Conn, error) {
		dialCalled = true
		c1, _ := net.Pipe()
		return c1, nil
	})

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// 即座に切断されるため、読み取りは EOF または エラー
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 10)
	n, err := conn.Read(buf)
	if n > 0 {
		t.Errorf("expected 0 bytes read, got %d", n)
	}
	if err == nil {
		t.Error("expected error/EOF on immediate disconnect, got nil")
	}
	if dialCalled {
		t.Error("dial should not be called when session leader is not set")
	}
}

func TestRelay_CallerPIDError(t *testing.T) {
	origLeader := GetSessionPID()
	defer SetSessionPID(origLeader)
	SetSessionPID(100)

	origCaller := CallerPIDFunc
	defer func() { CallerPIDFunc = origCaller }()
	CallerPIDFunc = func(net.Addr) (int, error) {
		return 0, errors.New("cannot identify caller")
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	go relay(l, func() (net.Conn, error) {
		c1, _ := net.Pipe()
		return c1, nil
	})

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 10)
	_, err = conn.Read(buf)
	if err == nil {
		t.Error("expected read error due to rejected connection")
	}
}

func TestRelay_DeniedCaller(t *testing.T) {
	origLeader := GetSessionPID()
	defer SetSessionPID(origLeader)
	SetSessionPID(100)

	tmp := t.TempDir()
	origProc := procDir
	procDir = tmp
	defer func() { procDir = origProc }()

	// PID 999: 未許可プロセス (curl)
	pDir := filepath.Join(tmp, "999")
	_ = os.MkdirAll(pDir, 0o755)
	_ = os.WriteFile(filepath.Join(pDir, "stat"), []byte("999 (curl) S 100 100 0\n"), 0o644)
	_ = os.WriteFile(filepath.Join(pDir, "comm"), []byte("curl\n"), 0o644)

	origCaller := CallerPIDFunc
	defer func() { CallerPIDFunc = origCaller }()
	CallerPIDFunc = func(net.Addr) (int, error) {
		return 999, nil
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	dialCalled := false
	go relay(l, func() (net.Conn, error) {
		dialCalled = true
		c1, _ := net.Pipe()
		return c1, nil
	})

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 10)
	_, err = conn.Read(buf)
	if err == nil {
		t.Error("expected read error due to denied caller")
	}
	if dialCalled {
		t.Error("dial should not be called for denied caller")
	}
}

func TestRelay_AllowedCallerAndTransfer(t *testing.T) {
	origLeader := GetSessionPID()
	defer SetSessionPID(origLeader)
	SetSessionPID(100)

	tmp := t.TempDir()
	origProc := procDir
	procDir = tmp
	defer func() { procDir = origProc }()

	// PID 200: 許可されたエージェント本体 (agy)
	pDir := filepath.Join(tmp, "200")
	_ = os.MkdirAll(pDir, 0o755)
	_ = os.WriteFile(filepath.Join(pDir, "stat"), []byte("200 (agy) S 100 100 0\n"), 0o644)
	_ = os.WriteFile(filepath.Join(pDir, "comm"), []byte("agy\n"), 0o644)

	origCaller := CallerPIDFunc
	defer func() { CallerPIDFunc = origCaller }()
	CallerPIDFunc = func(net.Addr) (int, error) {
		return 200, nil
	}

	// 上流（dial 先）のモックサーバー
	upstreamL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamL.Close()

	go func() {
		for {
			upConn, err := upstreamL.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 128)
				n, _ := c.Read(buf)
				if n > 0 {
					_, _ = c.Write([]byte("echo:" + string(buf[:n])))
				}
			}(upConn)
		}
	}()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	go relay(l, func() (net.Conn, error) {
		return net.Dial("tcp", upstreamL.Addr().String())
	})

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// 送信
	msg := "hello-quagent"
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatalf("failed to write: %v", err)
	}

	// 応答確認
	buf := make([]byte, 128)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("failed to read echoed response: %v", err)
	}
	want := "echo:" + msg
	if string(buf[:n]) != want {
		t.Errorf("got response %q, want %q", string(buf[:n]), want)
	}
}

func TestRelay_DialError(t *testing.T) {
	origLeader := GetSessionPID()
	defer SetSessionPID(origLeader)
	SetSessionPID(100)

	tmp := t.TempDir()
	origProc := procDir
	procDir = tmp
	defer func() { procDir = origProc }()

	pDir := filepath.Join(tmp, "200")
	_ = os.MkdirAll(pDir, 0o755)
	_ = os.WriteFile(filepath.Join(pDir, "stat"), []byte("200 (agy) S 100 100 0\n"), 0o644)
	_ = os.WriteFile(filepath.Join(pDir, "comm"), []byte("agy\n"), 0o644)

	origCaller := CallerPIDFunc
	defer func() { CallerPIDFunc = origCaller }()
	CallerPIDFunc = func(net.Addr) (int, error) {
		return 200, nil
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	go relay(l, func() (net.Conn, error) {
		return nil, errors.New("upstream unreachable")
	})

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 10)
	_, err = conn.Read(buf)
	if err == nil {
		t.Error("expected read error due to upstream dial failure")
	}
}

func TestRelay_MaxConnsLimit(t *testing.T) {
	origLeader := GetSessionPID()
	defer SetSessionPID(origLeader)
	SetSessionPID(100)

	tmp := t.TempDir()
	origProc := procDir
	procDir = tmp
	defer func() { procDir = origProc }()

	pDir := filepath.Join(tmp, "200")
	_ = os.MkdirAll(pDir, 0o755)
	_ = os.WriteFile(filepath.Join(pDir, "stat"), []byte("200 (agy) S 100 100 0\n"), 0o644)
	_ = os.WriteFile(filepath.Join(pDir, "comm"), []byte("agy\n"), 0o644)

	origCaller := CallerPIDFunc
	defer func() { CallerPIDFunc = origCaller }()
	CallerPIDFunc = func(net.Addr) (int, error) {
		return 200, nil
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	blockCh := make(chan struct{})
	go relay(l, func() (net.Conn, error) {
		c1, c2 := net.Pipe()
		go func() {
			<-blockCh
			c2.Close()
		}()
		return c1, nil
	})

	// 64本の接続を確立してブロックさせる
	var conns []net.Conn
	defer func() {
		close(blockCh)
		for _, c := range conns {
			c.Close()
		}
	}()

	for i := 0; i < 64; i++ {
		c, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatalf("connection %d failed: %v", i, err)
		}
		conns = append(conns, c)
	}

	// 65本目の接続は上限超過で直ちに切断される
	extraConn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer extraConn.Close()

	_ = extraConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 10)
	_, err = extraConn.Read(buf)
	if err == nil {
		t.Error("expected error/EOF on 65th connection exceeding max limit")
	}
}
