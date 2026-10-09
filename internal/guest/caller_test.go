package guest

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestFindInodeForPort(t *testing.T) {
	tmp := t.TempDir()
	tcpPath := filepath.Join(tmp, "tcp")
	content := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1B9E 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 12345 2 0000000000000000 99 0 0 10 0
   1: 0100007F:D431 0100007F:1B9E 01 00000000:00000000 00:00000000 00000000  1000        0 67890 2 0000000000000000 99 0 0 10 0
`
	if err := os.WriteFile(tcpPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// 0x1B9E = 7070
	inode, err := findInodeForPort(tcpPath, 7070)
	if err != nil {
		t.Fatalf("findInodeForPort failed: %v", err)
	}
	if inode != 12345 {
		t.Errorf("got inode %d, want 12345", inode)
	}

	// 0xD431 = 54321
	inode, err = findInodeForPort(tcpPath, 54321)
	if err != nil {
		t.Fatalf("findInodeForPort failed: %v", err)
	}
	if inode != 67890 {
		t.Errorf("got inode %d, want 67890", inode)
	}

	// 存在しないポート
	_, err = findInodeForPort(tcpPath, 9999)
	if err == nil {
		t.Error("expected error for non-existent port")
	}
}

func TestFindPIDForInode(t *testing.T) {
	tmp := t.TempDir()
	// /proc/100/fd/3 -> socket:[67890]
	// /proc/200/fd/4 -> socket:[99999]
	pid100Dir := filepath.Join(tmp, "100", "fd")
	pid200Dir := filepath.Join(tmp, "200", "fd")
	if err := os.MkdirAll(pid100Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(pid200Dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink("socket:[67890]", filepath.Join(pid100Dir, "3")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("socket:[99999]", filepath.Join(pid200Dir, "4")); err != nil {
		t.Fatal(err)
	}

	pid, err := findPIDForInode(tmp, 67890)
	if err != nil {
		t.Fatalf("findPIDForInode failed: %v", err)
	}
	if pid != 100 {
		t.Errorf("got pid %d, want 100", pid)
	}

	// 存在しない inode
	_, err = findPIDForInode(tmp, 11111)
	if err == nil {
		t.Error("expected error for non-existent inode")
	}
}

func TestParentPID(t *testing.T) {
	tmp := t.TempDir()
	// stat: pid (comm) state ppid ...
	pidDir := filepath.Join(tmp, "123")
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// コマンド名に括弧やスペースが含まれるエッジケース
	statContent := "123 (my special (app)) S 456 123 0 0 0\n"
	if err := os.WriteFile(filepath.Join(pidDir, "stat"), []byte(statContent), 0o644); err != nil {
		t.Fatal(err)
	}

	ppid, err := ParentPID(tmp, 123)
	if err != nil {
		t.Fatalf("ParentPID failed: %v", err)
	}
	if ppid != 456 {
		t.Errorf("got ppid %d, want 456", ppid)
	}
}

func TestIsAllowedCaller(t *testing.T) {
	tmp := t.TempDir()

	// プロセスツリー:
	// PID 100: sessionLeader (/entrypoint.sh)
	//   └─ PID 200: agent (agy) (comm: "agy")
	//       └─ PID 300: bash (comm: "bash")
	//           └─ PID 400: pytest (comm: "pytest")
	// PID 999: unrelated daemon
	makeProc := func(pid int, comm string, ppid int) {
		pDir := filepath.Join(tmp, fmt.Sprint(pid))
		_ = os.MkdirAll(pDir, 0o755)
		_ = os.WriteFile(filepath.Join(pDir, "stat"), []byte(fmt.Sprintf("%d (%s) S %d 100 0\n", pid, comm, ppid)), 0o644)
		_ = os.WriteFile(filepath.Join(pDir, "comm"), []byte(comm+"\n"), 0o644)
	}

	makeProc(100, "bash", 1)
	makeProc(200, "agy", 100)
	makeProc(300, "bash", 200)
	makeProc(400, "pytest", 300)
	makeProc(999, "daemon", 1)

	leader := 100

	// 1. セッションリーダー自身 -> 許可
	if !IsAllowedCaller(tmp, 100, leader) {
		t.Error("expected sessionLeader to be allowed")
	}

	// 2. セッションリーダー直下のエージェント本体 (PID 200) -> 許可
	if !IsAllowedCaller(tmp, 200, leader) {
		t.Error("expected agent process to be allowed")
	}

	// 3. エージェントが実行したシェル (PID 300) -> 拒否
	if IsAllowedCaller(tmp, 300, leader) {
		t.Error("expected subshell to be denied")
	}

	// 4. エージェント配下のテストツール (PID 400) -> 拒否
	if IsAllowedCaller(tmp, 400, leader) {
		t.Error("expected test process to be denied")
	}

	// 5. 無関係なプロセス (PID 999) -> 拒否
	if IsAllowedCaller(tmp, 999, leader) {
		t.Error("expected unrelated process to be denied")
	}

	// 6. leader <= 0 のとき -> 拒否
	if IsAllowedCaller(tmp, 200, 0) {
		t.Error("expected denial when session leader is not set")
	}
}

func TestCallerPID(t *testing.T) {
	// TCPAddr でない場合のエラー検証
	_, err := CallerPID(&net.UnixAddr{Name: "test.sock", Net: "unix"})
	if err == nil {
		t.Error("expected error for non-TCPAddr")
	}
}

func TestProcessComm(t *testing.T) {
	tmp := t.TempDir()
	pidDir := filepath.Join(tmp, "123")
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pidDir, "comm"), []byte("myprocess\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	comm, err := ProcessComm(tmp, 123)
	if err != nil {
		t.Fatalf("ProcessComm failed: %v", err)
	}
	if comm != "myprocess" {
		t.Errorf("got comm %q, want 'myprocess'", comm)
	}
}

func TestIsAllowedCaller_RebootFromShell(t *testing.T) {
	tmp := t.TempDir()

	// ユーザーがシェルから /entrypoint.sh を再起動したときのプロセスツリー:
	// PID 100: 対話型シェル (sessionLeader: bash -i)
	//   └─ PID 250: /entrypoint.sh (comm: "bash")
	//       └─ PID 260: 再起動されたエージェント (comm: "agy")
	//           └─ PID 270: エージェントが実行したテスト (comm: "pytest")
	makeProc := func(pid int, comm string, ppid int) {
		pDir := filepath.Join(tmp, fmt.Sprint(pid))
		_ = os.MkdirAll(pDir, 0o755)
		_ = os.WriteFile(filepath.Join(pDir, "stat"), []byte(fmt.Sprintf("%d (%s) S %d 100 0\n", pid, comm, ppid)), 0o644)
		_ = os.WriteFile(filepath.Join(pDir, "comm"), []byte(comm+"\n"), 0o644)
	}

	makeProc(100, "bash", 1)
	makeProc(250, "bash", 100)
	makeProc(260, "agy", 250)
	makeProc(270, "pytest", 260)

	leader := 100

	// 1. 再起動されたエージェント本体 (PID 260) は許可されること
	if !IsAllowedCaller(tmp, 260, leader) {
		t.Error("expected rebooted agent to be allowed")
	}

	// 2. 再起動されたエージェント配下のテストツール (PID 270) は拒否されること
	if IsAllowedCaller(tmp, 270, leader) {
		t.Error("expected test under rebooted agent to be denied")
	}
}

func TestSetGetSessionPID(t *testing.T) {
	orig := GetSessionPID()
	defer SetSessionPID(orig)

	SetSessionPID(12345)
	if got := GetSessionPID(); got != 12345 {
		t.Errorf("GetSessionPID() = %d, want 12345", got)
	}

	SetSessionPID(0)
	if got := GetSessionPID(); got != 0 {
		t.Errorf("GetSessionPID() = %d, want 0", got)
	}
}
