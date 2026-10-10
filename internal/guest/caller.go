package guest

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

var (
	// テストで差し替え可能な procfs パス
	procNetTCPPath = "/proc/net/tcp"
	procDir        = "/proc"

	sessionPIDMu sync.RWMutex
	sessionPID   int
)

// SetSessionPID は現在実行中のエージェントセッションのリーダー PID を設定する。
func SetSessionPID(pid int) {
	sessionPIDMu.Lock()
	defer sessionPIDMu.Unlock()
	sessionPID = pid
}

// GetSessionPID は現在実行中のエージェントセッションのリーダー PID を返す。
func GetSessionPID() int {
	sessionPIDMu.RLock()
	defer sessionPIDMu.RUnlock()
	return sessionPID
}

// CallerPIDFunc は CallerPID の実体。テストで差し替え可能。
var CallerPIDFunc = defaultCallerPID

// CallerPID はローカル TCP 接続の接続元プロセスの PID を特定する。
func CallerPID(remoteAddr net.Addr) (int, error) {
	return CallerPIDFunc(remoteAddr)
}

func defaultCallerPID(remoteAddr net.Addr) (int, error) {
	tcpAddr, ok := remoteAddr.(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("TCPAddr ではない: %v", remoteAddr)
	}
	inode, err := findInodeForPortExt(procNetTCPPath, tcpAddr.Port, 0, true)
	if err != nil {
		return 0, err
	}
	return findPIDForInode(procDir, inode)
}

// findInodeForPort は /proc/net/tcp から指定ポート番号のソケット inode を探す。
func findInodeForPort(tcpFile string, targetPort int) (uint64, error) {
	return findInodeForPortExt(tcpFile, targetPort, 0, false)
}

// findInodeForPortExt は /proc/net/tcp から指定ポートおよび任意のリモートポートのソケット inode を探す。
// onlyEstablished が true の場合、ソケット状態が ESTABLISHED (01) であることを検証する。
func findInodeForPortExt(tcpFile string, targetPort int, targetRemPort int, onlyEstablished bool) (uint64, error) {
	f, err := os.Open(tcpFile)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	if !s.Scan() {
		return 0, io.EOF
	}
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 10 {
			continue
		}
		// ソケット状態が TCP_ESTABLISHED (01) であることを検証 (TIME_WAIT, CLOSE, LISTEN等を除外)
		if onlyEstablished && fields[3] != "01" {
			continue
		}
		localAddr := fields[1]
		idx := strings.IndexByte(localAddr, ':')
		if idx < 0 {
			continue
		}
		portHex := localAddr[idx+1:]
		port, err := strconv.ParseUint(portHex, 16, 16)
		if err != nil {
			continue
		}
		if int(port) != targetPort {
			continue
		}
		if targetRemPort > 0 {
			remAddr := fields[2]
			ridx := strings.IndexByte(remAddr, ':')
			if ridx >= 0 {
				remPortHex := remAddr[ridx+1:]
				rport, err := strconv.ParseUint(remPortHex, 16, 16)
				if err == nil && int(rport) != targetRemPort {
					continue
				}
			}
		}
		inode, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil {
			continue
		}
		return inode, nil
	}
	return 0, fmt.Errorf("ポート %d のソケットが見つからない", targetPort)
}

// findPIDForInode は procDir を走査して指定ソケット inode を所有する PID を探す。
func findPIDForInode(dir string, targetInode uint64) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	want := fmt.Sprintf("socket:[%d]", targetInode)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		fdDir := filepath.Join(dir, e.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err == nil && link == want {
				return pid, nil
			}
		}
	}
	return 0, fmt.Errorf("inode %d を所有するプロセスが見つからない", targetInode)
}

// ParentPID は /proc/<pid>/stat から親プロセスの PID (PPID) を取得する。
func ParentPID(dir string, pid int) (int, error) {
	b, err := os.ReadFile(filepath.Join(dir, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	s := string(b)
	lastClose := strings.LastIndexByte(s, ')')
	if lastClose < 0 || lastClose+2 >= len(s) {
		return 0, fmt.Errorf("stat の形式が不正: %s", s)
	}
	fields := strings.Fields(s[lastClose+2:])
	if len(fields) < 2 {
		return 0, fmt.Errorf("ppid フィールドが見つからない")
	}
	return strconv.Atoi(fields[1])
}

// ProcessComm は /proc/<pid>/comm からプロセス名を取得する。
func ProcessComm(dir string, pid int) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, strconv.Itoa(pid), "comm"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// ProcessExe は /proc/<pid>/exe のシンボリックリンク先から実行ファイルのパスを取得する。
func ProcessExe(dir string, pid int) (string, error) {
	link, err := os.Readlink(filepath.Join(dir, strconv.Itoa(pid), "exe"))
	if err != nil {
		return "", err
	}
	return link, nil
}

// CheckAgentChildCommand はプロセスがエージェントの子孫かを調べる隠しサブコマンド名。
const CheckAgentChildCommand = "__check_agent_child"

// IsAgentProcess はプロセス名がエージェント本体 (opencode, claude, agy, codex) かを返す。
func IsAgentProcess(comm string) bool {
	switch comm {
	case "opencode", "claude", "agy", "codex":
		return true
	}
	return false
}

func isAgentProcess(comm string) bool { return IsAgentProcess(comm) }

// IsDescendantOfAgent は targetPID から親を辿り、エージェント本体の子孫プロセスかを判定する。
func IsDescendantOfAgent(dir string, targetPID int) bool {
	if targetPID <= 1 {
		return false
	}
	curr := targetPID
	const maxDepth = 64
	for depth := 0; depth < maxDepth && curr > 1; depth++ {
		ppid, err := ParentPID(dir, curr)
		if err != nil || ppid <= 1 {
			break
		}
		pcomm, err := ProcessComm(dir, ppid)
		if err == nil && IsAgentProcess(pcomm) {
			return true
		}
		curr = ppid
	}
	return false
}

// isShellProcess はプロセス名がシェル本体またはスクリプト実行シェルかを判定する。
// Linux カーネルは shebang スクリプトの実行時、/proc/<pid>/comm をスクリプトファイル名に設定するため、
// /entrypoint.sh や *.sh のスクリプト名もシェルプロセスとして扱う。
func isShellProcess(comm string) bool {
	return comm == "bash" || comm == "sh" || comm == "entrypoint.sh" || strings.HasSuffix(comm, ".sh")
}

// IsAllowedCaller は callerPID がエージェント本体 (またはセッションリーダー) かを判定する。
// エージェントが実行したサブプロセス (テスト、ビルドツール、シェル等) は拒否する。
// OpenCode のようにエージェント本体が内部で子プロセス (opencode serve 等) を起動する
// マルチプロセス構成の場合は、エージェント本体の子プロセスとしての接続を許可する。
func IsAllowedCaller(dir string, callerPID int, sessionLeaderPID int) bool {
	if sessionLeaderPID <= 0 || callerPID <= 0 {
		return false
	}
	// 1. セッションリーダー自身 (bash 等)
	if callerPID == sessionLeaderPID {
		return true
	}

	callerComm, err := ProcessComm(dir, callerPID)
	if err != nil {
		return false
	}

	callerIsAgent := isAgentProcess(callerComm)
	callerIsShell := isShellProcess(callerComm)
	if !callerIsAgent && !callerIsShell {
		// エージェント本体でもシェルでもない (テストツール、curl 等)
		return false
	}

	curr := callerPID
	reachesLeader := false
	seenNonAgent := !callerIsAgent // シェルの場合は最初から非エージェント扱い

	// 無限ループ・循環参照の防止 (最大 64 階層)
	const maxDepth = 64
	for depth := 0; depth < maxDepth && curr > 1 && curr != sessionLeaderPID; depth++ {
		ppid, err := ParentPID(dir, curr)
		if err != nil {
			return false
		}
		if ppid == sessionLeaderPID {
			reachesLeader = true
			break
		}
		if ppid <= 1 {
			break
		}

		pcomm, err := ProcessComm(dir, ppid)
		if err != nil {
			return false
		}

		pIsAgent := isAgentProcess(pcomm)
		pIsShell := isShellProcess(pcomm)

		if !pIsAgent && !pIsShell {
			// 親にシェルでもエージェントでもないプロセス (pytest, make, python 等) が介在している
			return false
		}

		if seenNonAgent && pIsAgent {
			// 既にシェル等の非エージェント層を経由した後に上位にエージェントが存在する
			// (エージェントが起動したサブシェルやテスト配下からの呼び出し)
			return false
		}

		if !pIsAgent {
			seenNonAgent = true
		}

		curr = ppid
	}

	return reachesLeader
}
