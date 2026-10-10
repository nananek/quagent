package guest

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
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

func TestFindInodeForPortExt(t *testing.T) {
	tmp := t.TempDir()
	tcpPath := filepath.Join(tmp, "tcp")
	content := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1B9E 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 12345 2 0000000000000000 99 0 0 10 0
   1: short line
   2: 0100007F:XXXX 00000000:0000 01 00000000:00000000 00:00000000 00000000  1000        0 22222 2 0000000000000000 99 0 0 10 0
   3: no_colon 00000000:0000 01 00000000:00000000 00:00000000 00000000  1000        0 33333 2 0000000000000000 99 0 0 10 0
   4: 0100007F:D431 0100007F:1B9E 01 00000000:00000000 00:00000000 00000000  1000        0 67890 2 0000000000000000 99 0 0 10 0
   5: 0100007F:D432 0100007F:1B9F 01 00000000:00000000 00:00000000 00000000  1000        0 badinode 2 0000000000000000 99 0 0 10 0
   6: 0100007F:D433 0100007F:1B9E 06 00000000:00000000 00:00000000 00000000  1000        0 77777 2 0000000000000000 99 0 0 10 0
`
	if err := os.WriteFile(tcpPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. onlyEstablished = true のとき、ESTABLISHED (01) のみマッチ
	inode, err := findInodeForPortExt(tcpPath, 54321, 0, true)
	if err != nil || inode != 67890 {
		t.Fatalf("expected inode 67890, got %d, err: %v", inode, err)
	}

	// 2. onlyEstablished = true のとき、TIME_WAIT (06) は除外される
	_, err = findInodeForPortExt(tcpPath, 54323, 0, true)
	if err == nil {
		t.Error("expected error for non-established socket when onlyEstablished=true")
	}

	// 3. targetRemPort の一致
	inode, err = findInodeForPortExt(tcpPath, 54321, 7070, true)
	if err != nil || inode != 67890 {
		t.Fatalf("expected inode 67890 with matching remPort, got %d, err: %v", inode, err)
	}

	// 4. targetRemPort の不一致
	_, err = findInodeForPortExt(tcpPath, 54321, 9999, true)
	if err == nil {
		t.Error("expected error for mismatched remPort")
	}

	// 5. 存在しないファイル
	_, err = findInodeForPortExt(filepath.Join(tmp, "nonexistent"), 54321, 0, true)
	if err == nil {
		t.Error("expected error for non-existent file")
	}

	// 6. 空ファイル
	emptyPath := filepath.Join(tmp, "empty")
	_ = os.WriteFile(emptyPath, []byte(""), 0o644)
	_, err = findInodeForPortExt(emptyPath, 54321, 0, true)
	if err == nil {
		t.Error("expected error for empty file")
	}
}

func TestFindPIDForInode_Edges(t *testing.T) {
	tmp := t.TempDir()

	// 1. 存在しないディレクトリ
	_, err := findPIDForInode(filepath.Join(tmp, "nonexistent"), 12345)
	if err == nil {
		t.Error("expected error for non-existent dir")
	}

	// 2. 数字以外のエントリ (self, sys 等)
	_ = os.MkdirAll(filepath.Join(tmp, "self"), 0o755)
	_ = os.MkdirAll(filepath.Join(tmp, "sys"), 0o755)

	// 3. fd ディレクトリが存在しない PID ディレクトリ
	_ = os.MkdirAll(filepath.Join(tmp, "101"), 0o755)

	// 4. socket 以外のリンクを持つ PID ディレクトリ
	pid102FD := filepath.Join(tmp, "102", "fd")
	_ = os.MkdirAll(pid102FD, 0o755)
	_ = os.Symlink("/dev/null", filepath.Join(pid102FD, "0"))
	_ = os.Symlink("pipe:[9999]", filepath.Join(pid102FD, "1"))

	_, err = findPIDForInode(tmp, 12345)
	if err == nil {
		t.Error("expected error when inode not found")
	}
}

func TestParentPID_Edges(t *testing.T) {
	tmp := t.TempDir()

	// 1. 存在しない PID
	_, err := ParentPID(tmp, 9999)
	if err == nil {
		t.Error("expected error for non-existent PID")
	}

	// 2. 括弧が閉じられていない stat
	pDir1 := filepath.Join(tmp, "1")
	_ = os.MkdirAll(pDir1, 0o755)
	_ = os.WriteFile(filepath.Join(pDir1, "stat"), []byte("1 (unclosed_name S 0 0\n"), 0o644)
	_, err = ParentPID(tmp, 1)
	if err == nil {
		t.Error("expected error for unclosed comm in stat")
	}

	// 3. フィールド不足の stat
	pDir2 := filepath.Join(tmp, "2")
	_ = os.MkdirAll(pDir2, 0o755)
	_ = os.WriteFile(filepath.Join(pDir2, "stat"), []byte("2 (name) \n"), 0o644)
	_, err = ParentPID(tmp, 2)
	if err == nil {
		t.Error("expected error for insufficient fields in stat")
	}
}

func TestProcessComm_Edges(t *testing.T) {
	tmp := t.TempDir()
	_, err := ProcessComm(tmp, 9999)
	if err == nil {
		t.Error("expected error for non-existent comm")
	}
}

func TestProcessExe(t *testing.T) {
	tmp := t.TempDir()
	pDir := filepath.Join(tmp, "100")
	_ = os.MkdirAll(pDir, 0o755)
	exePath := filepath.Join(tmp, "mybin")
	_ = os.WriteFile(exePath, []byte("#!/bin/sh\n"), 0o755)
	_ = os.Symlink(exePath, filepath.Join(pDir, "exe"))

	got, err := ProcessExe(tmp, 100)
	if err != nil {
		t.Fatalf("ProcessExe failed: %v", err)
	}
	if got != exePath {
		t.Errorf("ProcessExe got %q, want %q", got, exePath)
	}

	// 存在しない PID
	_, err = ProcessExe(tmp, 9999)
	if err == nil {
		t.Error("expected error for non-existent exe")
	}
}

func TestIsAllowedCaller_AdvancedCases(t *testing.T) {
	tmp := t.TempDir()

	makeProc := func(pid int, comm string, ppid int) {
		pDir := filepath.Join(tmp, fmt.Sprint(pid))
		_ = os.MkdirAll(pDir, 0o755)
		_ = os.WriteFile(filepath.Join(pDir, "stat"), []byte(fmt.Sprintf("%d (%s) S %d 100 0\n", pid, comm, ppid)), 0o644)
		_ = os.WriteFile(filepath.Join(pDir, "comm"), []byte(comm+"\n"), 0o644)
	}

	leader := 50
	makeProc(leader, "bash", 1)

	// 1. callerPID <= 0 の拒否
	if IsAllowedCaller(tmp, 0, leader) || IsAllowedCaller(tmp, -1, leader) {
		t.Error("expected callerPID <= 0 to be denied")
	}

	// 2. セッションリーダー自身 -> 即時許可
	if !IsAllowedCaller(tmp, leader, leader) {
		t.Error("expected session leader itself to be allowed")
	}

	// 3. opencode 本体と claude 本体
	makeProc(60, "opencode", leader)
	makeProc(70, "claude", leader)
	if !IsAllowedCaller(tmp, 60, leader) {
		t.Error("expected opencode to be allowed")
	}
	if !IsAllowedCaller(tmp, 70, leader) {
		t.Error("expected claude to be allowed")
	}

	// 4. 多段子孫プロセス (agy -> bash -> python -> pytest: 拒否)
	makeProc(80, "agy", leader)
	makeProc(81, "bash", 80)
	makeProc(82, "python", 81)
	makeProc(83, "pytest", 82)
	if IsAllowedCaller(tmp, 83, leader) {
		t.Error("expected deeply nested test process under agent to be denied")
	}

	// 5. セッションリーダー直下だが未許可のコマンド (curl, python 等)
	makeProc(90, "curl", leader)
	if IsAllowedCaller(tmp, 90, leader) {
		t.Error("expected curl under session leader to be denied")
	}

	// 6. 親を辿る途中で stat が読めなくなった場合 (安全に拒否)
	makeProc(95, "agy", 94) // PPID 94 は存在しない
	if IsAllowedCaller(tmp, 95, leader) {
		t.Error("expected denial when parent stat is missing")
	}

	// 7. 循環参照プロセスツリー (PID 101 <-> PID 102)
	makeProc(101, "loop1", 102)
	makeProc(102, "loop2", 101)
	if IsAllowedCaller(tmp, 101, leader) {
		t.Error("expected denial on circular parent loop without hanging")
	}
}

func TestDefaultCallerPID(t *testing.T) {
	tmp := t.TempDir()
	tcpFile := filepath.Join(tmp, "tcp")
	content := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:D431 0100007F:1B9E 01 00000000:00000000 00:00000000 00000000  1000        0 55555 2 0000000000000000 99 0 0 10 0
`
	if err := os.WriteFile(tcpFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// proc/500/fd/3 -> socket:[55555]
	fdDir := filepath.Join(tmp, "500", "fd")
	_ = os.MkdirAll(fdDir, 0o755)
	_ = os.Symlink("socket:[55555]", filepath.Join(fdDir, "3"))

	origTCP := procNetTCPPath
	origProc := procDir
	procNetTCPPath = tcpFile
	procDir = tmp
	defer func() {
		procNetTCPPath = origTCP
		procDir = origProc
	}()

	pid, err := defaultCallerPID(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 54321})
	if err != nil {
		t.Fatalf("defaultCallerPID failed: %v", err)
	}
	if pid != 500 {
		t.Errorf("got pid %d, want 500", pid)
	}

	// ソケットが見つからないポート
	_, err = defaultCallerPID(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9999})
	if err == nil {
		t.Error("expected error for non-existent port")
	}
}

func TestIsAgentProcess(t *testing.T) {
	for _, name := range []string{"opencode", "claude", "agy", "codex"} {
		if !IsAgentProcess(name) {
			t.Errorf("expected %s to be agent process", name)
		}
	}
	for _, name := range []string{"bash", "sh", "python3", "pytest", "npm"} {
		if IsAgentProcess(name) {
			t.Errorf("expected %s NOT to be agent process", name)
		}
	}
}

func TestIsDescendantOfAgent(t *testing.T) {
	tmp := t.TempDir()
	makeProc := func(pid int, comm string, ppid int) {
		pDir := filepath.Join(tmp, strconv.Itoa(pid))
		_ = os.MkdirAll(pDir, 0o755)
		_ = os.WriteFile(filepath.Join(pDir, "comm"), []byte(comm+"\n"), 0o644)
		stat := fmt.Sprintf("%d (%s) S %d 1 1 0 0", pid, comm, ppid)
		_ = os.WriteFile(filepath.Join(pDir, "stat"), []byte(stat), 0o644)
	}

	// 1. targetPID <= 1
	if IsDescendantOfAgent(tmp, 1) {
		t.Error("expected false for pid 1")
	}
	if IsDescendantOfAgent(tmp, 0) {
		t.Error("expected false for pid 0")
	}

	// 2. proc tree: 1 -> bash(10) -> agy(20) -> bash(30) -> pytest(40)
	makeProc(10, "bash", 1)
	makeProc(20, "agy", 10)
	makeProc(30, "bash", 20)
	makeProc(40, "pytest", 30)

	// pytest(40) is descendant of agy(20)
	if !IsDescendantOfAgent(tmp, 40) {
		t.Error("expected pytest(40) to be descendant of agy")
	}
	// bash(30) is descendant of agy(20)
	if !IsDescendantOfAgent(tmp, 30) {
		t.Error("expected bash(30) to be descendant of agy")
	}
	// agy(20) parent is bash(10), not a descendant of another agent
	if IsDescendantOfAgent(tmp, 20) {
		t.Error("expected agy(20) parent to not be agent")
	}
	// bash(10) parent is init(1)
	if IsDescendantOfAgent(tmp, 10) {
		t.Error("expected bash(10) to not be descendant of agent")
	}

	// 3. Unknown PID
	if IsDescendantOfAgent(tmp, 9999) {
		t.Error("expected false for unknown pid")
	}
}

func TestIsAllowedCaller_MultiProcessAgent(t *testing.T) {
	tmp := t.TempDir()

	// OpenCode のマルチプロセス構成:
	// PID 100: 対話型シェル (sessionLeader: bash -i)
	//   └─ PID 200: /entrypoint.sh (comm: "bash")
	//       └─ PID 300: opencode CLI (comm: "opencode")
	//           └─ PID 310: opencode serve (comm: "opencode")
	makeProc := func(pid int, comm string, ppid int) {
		pDir := filepath.Join(tmp, fmt.Sprint(pid))
		_ = os.MkdirAll(pDir, 0o755)
		_ = os.WriteFile(filepath.Join(pDir, "stat"), []byte(fmt.Sprintf("%d (%s) S %d 100 0\n", pid, comm, ppid)), 0o644)
		_ = os.WriteFile(filepath.Join(pDir, "comm"), []byte(comm+"\n"), 0o644)
	}

	makeProc(100, "bash", 1)
	makeProc(200, "bash", 100)
	makeProc(300, "opencode", 200)
	makeProc(310, "opencode", 300)

	leader := 100

	// 1. opencode 親プロセスは許可されること
	if !IsAllowedCaller(tmp, 300, leader) {
		t.Error("expected opencode CLI (PID 300) to be allowed")
	}

	// 2. opencode 子プロセス (serve) も許可されること
	if !IsAllowedCaller(tmp, 310, leader) {
		t.Error("expected opencode serve (PID 310) to be allowed")
	}
}

func TestIsAllowedCaller_AgentUnderSubprocessDenied(t *testing.T) {
	tmp := t.TempDir()

	// エージェント配下のテストツール等がエージェントを実行または偽装した場合:
	// PID 100: sessionLeader (bash)
	//   └─ PID 200: agy (comm: "agy")
	//       └─ PID 300: pytest (comm: "pytest")
	//           └─ PID 310: opencode (comm: "opencode")
	makeProc := func(pid int, comm string, ppid int) {
		pDir := filepath.Join(tmp, fmt.Sprint(pid))
		_ = os.MkdirAll(pDir, 0o755)
		_ = os.WriteFile(filepath.Join(pDir, "stat"), []byte(fmt.Sprintf("%d (%s) S %d 100 0\n", pid, comm, ppid)), 0o644)
		_ = os.WriteFile(filepath.Join(pDir, "comm"), []byte(comm+"\n"), 0o644)
	}

	makeProc(100, "bash", 1)
	makeProc(200, "agy", 100)
	makeProc(300, "pytest", 200)
	makeProc(310, "opencode", 300)

	leader := 100

	// テストツール配下の opencode は拒否されること
	if IsAllowedCaller(tmp, 310, leader) {
		t.Error("expected opencode under pytest to be denied")
	}
}

func TestIsAllowedCaller_AgentUnderSubshellDenied(t *testing.T) {
	tmp := t.TempDir()

	// エージェント配下のサブシェルがエージェントを実行した場合:
	// PID 100: sessionLeader (bash)
	//   └─ PID 200: agy (comm: "agy")
	//       └─ PID 300: bash (comm: "bash")
	//           └─ PID 310: opencode (comm: "opencode")
	makeProc := func(pid int, comm string, ppid int) {
		pDir := filepath.Join(tmp, fmt.Sprint(pid))
		_ = os.MkdirAll(pDir, 0o755)
		_ = os.WriteFile(filepath.Join(pDir, "stat"), []byte(fmt.Sprintf("%d (%s) S %d 100 0\n", pid, comm, ppid)), 0o644)
		_ = os.WriteFile(filepath.Join(pDir, "comm"), []byte(comm+"\n"), 0o644)
	}

	makeProc(100, "bash", 1)
	makeProc(200, "agy", 100)
	makeProc(300, "bash", 200)
	makeProc(310, "opencode", 300)

	leader := 100

	// サブシェル配下の opencode は拒否されること
	if IsAllowedCaller(tmp, 310, leader) {
		t.Error("expected opencode under subshell to be denied")
	}
}
