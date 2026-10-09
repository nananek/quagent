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

// CallerPID はローカル TCP 接続の接続元プロセスの PID を特定する。
func CallerPID(remoteAddr net.Addr) (int, error) {
	tcpAddr, ok := remoteAddr.(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("TCPAddr ではない: %v", remoteAddr)
	}
	inode, err := findInodeForPort(procNetTCPPath, tcpAddr.Port)
	if err != nil {
		return 0, err
	}
	return findPIDForInode(procDir, inode)
}

// findInodeForPort は /proc/net/tcp から指定ポート番号のソケット inode を探す。
func findInodeForPort(tcpFile string, targetPort int) (uint64, error) {
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
		if int(port) == targetPort {
			inode, err := strconv.ParseUint(fields[9], 10, 64)
			if err != nil {
				continue
			}
			return inode, nil
		}
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

func isAgentProcess(comm string) bool {
	switch comm {
	case "opencode", "claude", "agy":
		return true
	}
	return false
}

// IsAllowedCaller は callerPID がエージェント本体 (またはセッションリーダー) かを判定する。
// エージェントが実行したサブプロセス (テスト、ビルドツール、シェル等) は拒否する。
func IsAllowedCaller(dir string, callerPID int, sessionLeaderPID int) bool {
	if sessionLeaderPID <= 0 || callerPID <= 0 {
		return false
	}
	// 1. セッションリーダー自身 (bash 等)
	if callerPID == sessionLeaderPID {
		return true
	}

	// callerPID から親を辿りながら、sessionLeaderPID までの経路を検査する
	curr := callerPID
	isChildOfAgent := false
	reachesLeader := false

	for curr > 1 && curr != sessionLeaderPID {
		ppid, err := ParentPID(dir, curr)
		if err != nil {
			return false
		}
		// 親プロセスの comm を調べる (エージェント本体の子孫プロセスかを判定)
		if ppid != sessionLeaderPID && ppid > 1 {
			pcomm, err := ProcessComm(dir, ppid)
			if err == nil && isAgentProcess(pcomm) {
				isChildOfAgent = true
			}
		}
		if ppid == sessionLeaderPID {
			reachesLeader = true
			break
		}
		curr = ppid
	}

	if !reachesLeader {
		// セッションリーダーの子孫ではない (セッション外の無関係なプロセス)
		return false
	}

	if isChildOfAgent {
		// エージェント本体が実行した子孫プロセス (テスト、ビルド、シェル等) なので拒否
		return false
	}

	// エージェント本体自身、または entrypoint 起動シェルなら許可
	callerComm, err := ProcessComm(dir, callerPID)
	if err != nil {
		return false
	}
	if isAgentProcess(callerComm) || callerComm == "bash" || callerComm == "sh" {
		return true
	}

	return false
}
