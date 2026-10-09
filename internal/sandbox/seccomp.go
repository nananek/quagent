package sandbox

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// classic BPF (cBPF) の命令コード。
const (
	bpfLD   = 0x20 // BPF_LD  | BPF_W | BPF_ABS
	bpfJEQ  = 0x15 // BPF_JMP | BPF_JEQ | BPF_K
	bpfJSET = 0x45 // BPF_JMP | BPF_JSET | BPF_K
	bpfRET  = 0x06 // BPF_RET | BPF_K
)

// x32SyscallBit は amd64 で x32 ABI の syscall 番号に立つビット (__X32_SYSCALL_BIT)。
// x32 は seccomp_data.arch を x86_64 のまま名乗るので、arch 検査だけでは
// 64bit と同じ番号の deny 一覧に当たらず、そのまま素通りしてしまう。
const x32SyscallBit = 0x40000000

// auditArch は seccomp のデータが名乗るべきアーキ。別の入口 (32bit ABI など) で
// 同じ番号の syscall を呼ばれるのを防ぐため、一致しなければ殺す。
func auditArch() (uint32, bool) {
	switch runtime.GOARCH {
	case "amd64":
		return unix.AUDIT_ARCH_X86_64, true
	case "arm64":
		return unix.AUDIT_ARCH_AARCH64, true
	default:
		return 0, false
	}
}

// buildSeccompFilter は denylist 方式の BPF を組む。arch が一致しなければ殺し、
// denyVsock が有効なら AF_VSOCK ソケットの作成を拒否し、
// nr が deny のどれかなら errno を返し、それ以外は許可する。
func buildSeccompFilter(arch uint32, errno uint32, deny []int, denyVsock bool) []unix.SockFilter {
	f := []unix.SockFilter{
		{Code: bpfLD, K: 4},                   // seccomp_data.arch
		{Code: bpfJEQ, Jt: 1, Jf: 0, K: arch}, // 一致すれば LD nr へ、しなければ次 (KILL)
		{Code: bpfRET, K: unix.SECCOMP_RET_KILL_PROCESS},
		{Code: bpfLD, K: 0}, // seccomp_data.nr
	}
	// x32 ABI は arch が x86_64 のまま nr の bit30 が立つ。deny 一覧をすり抜けるので
	// 別アーキ (i386 など) と同じく殺す。
	if arch == unix.AUDIT_ARCH_X86_64 {
		f = append(f,
			unix.SockFilter{Code: bpfJSET, Jt: 0, Jf: 1, K: x32SyscallBit}, // nr & X32 なら次 (KILL)
			unix.SockFilter{Code: bpfRET, K: unix.SECCOMP_RET_KILL_PROCESS},
		)
	}
	if denyVsock {
		var sysSocket uint32
		switch arch {
		case unix.AUDIT_ARCH_X86_64:
			sysSocket = unix.SYS_SOCKET
		case unix.AUDIT_ARCH_AARCH64:
			sysSocket = 198
		}
		if sysSocket != 0 {
			f = append(f,
				unix.SockFilter{Code: bpfJEQ, Jt: 0, Jf: 4, K: sysSocket}, // nr == SYS_SOCKET なら次へ、不一致なら 4 命令スキップ
				unix.SockFilter{Code: bpfLD, K: 16},                       // A = args[0] (domain)
				unix.SockFilter{Code: bpfJEQ, Jt: 0, Jf: 1, K: unix.AF_VSOCK}, // domain == AF_VSOCK なら次へ、不一致なら 1 命令スキップ
				unix.SockFilter{Code: bpfRET, K: unix.SECCOMP_RET_ERRNO | (errno & 0xffff)},
				unix.SockFilter{Code: bpfLD, K: 0}, // A = seccomp_data.nr を復元
			)
		}
	}
	for _, nr := range deny {
		f = append(f,
			unix.SockFilter{Code: bpfJEQ, Jt: 0, Jf: 1, K: uint32(nr)}, // 一致 -> RET errno、不一致 -> 次へ
			unix.SockFilter{Code: bpfRET, K: unix.SECCOMP_RET_ERRNO | (errno & 0xffff)},
		)
	}
	return append(f, unix.SockFilter{Code: bpfRET, K: unix.SECCOMP_RET_ALLOW})
}

// applySeccomp は現在のスレッドにフィルタをかける。フィルタはスレッド単位だが
// exec でほかのスレッドは消えるので、exec を呼ぶこのスレッドに固定すれば新しい
// イメージ (とその子孫) に必ず継承される。
func applySeccomp(deny []int, denyVsock bool) error {
	arch, ok := auditArch()
	if !ok {
		return fmt.Errorf("このアーキテクチャ (%s) では seccomp の方針を組めない", runtime.GOARCH)
	}
	runtime.LockOSThread()
	filter := buildSeccompFilter(arch, uint32(unix.EPERM), deny, denyVsock)
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if _, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, 0, uintptr(unsafe.Pointer(&prog))); errno != 0 {
		return fmt.Errorf("seccomp をかけられない: %w", errno)
	}
	return nil
}

// ApplySeccomp は現在のスレッドに seccomp フィルタをかける。
func ApplySeccomp(deny []int) error {
	return applySeccomp(deny, false)
}

// ApplySeccompWithVsock は現在のスレッドに seccomp フィルタをかけ、任意で AF_VSOCK を拒否する。
func ApplySeccompWithVsock(deny []int, denyVsock bool) error {
	return applySeccomp(deny, denyVsock)
}
