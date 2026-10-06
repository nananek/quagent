package sandbox

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// classic BPF (cBPF) の命令コード。
const (
	bpfLD  = 0x20 // BPF_LD  | BPF_W | BPF_ABS
	bpfJEQ = 0x15 // BPF_JMP | BPF_JEQ | BPF_K
	bpfRET = 0x06 // BPF_RET | BPF_K
)

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
// nr が deny のどれかなら errno を返し、それ以外は許可する。
func buildSeccompFilter(arch uint32, errno uint32, deny []int) []unix.SockFilter {
	f := []unix.SockFilter{
		{Code: bpfLD, K: 4},                   // seccomp_data.arch
		{Code: bpfJEQ, Jt: 1, Jf: 0, K: arch}, // 一致すれば LD nr へ、しなければ次 (KILL)
		{Code: bpfRET, K: unix.SECCOMP_RET_KILL_PROCESS},
		{Code: bpfLD, K: 0}, // seccomp_data.nr
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
func applySeccomp(deny []int) error {
	arch, ok := auditArch()
	if !ok {
		return fmt.Errorf("このアーキテクチャ (%s) では seccomp の方針を組めない", runtime.GOARCH)
	}
	runtime.LockOSThread()
	filter := buildSeccompFilter(arch, uint32(unix.EPERM), deny)
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if _, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, 0, uintptr(unsafe.Pointer(&prog))); errno != 0 {
		return fmt.Errorf("seccomp をかけられない: %w", errno)
	}
	return nil
}
