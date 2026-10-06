package sandbox

import (
	"encoding/binary"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// 書き込み系の操作だけを扱う (読み取りは制限しない)。値はカーネルの
// LANDLOCK_ACCESS_FS_*。ABI によっては使えないビットがあるので後で削る。
const (
	accessWriteFile  = uint64(unix.LANDLOCK_ACCESS_FS_WRITE_FILE)
	accessRemoveFile = uint64(unix.LANDLOCK_ACCESS_FS_REMOVE_FILE)
	accessRemoveDir  = uint64(unix.LANDLOCK_ACCESS_FS_REMOVE_DIR)
	accessMakeChar   = uint64(unix.LANDLOCK_ACCESS_FS_MAKE_CHAR)
	accessMakeDir    = uint64(unix.LANDLOCK_ACCESS_FS_MAKE_DIR)
	accessMakeReg    = uint64(unix.LANDLOCK_ACCESS_FS_MAKE_REG)
	accessMakeSock   = uint64(unix.LANDLOCK_ACCESS_FS_MAKE_SOCK)
	accessMakeFifo   = uint64(unix.LANDLOCK_ACCESS_FS_MAKE_FIFO)
	accessMakeBlock  = uint64(unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK)
	accessMakeSym    = uint64(unix.LANDLOCK_ACCESS_FS_MAKE_SYM)
	accessRefer      = uint64(unix.LANDLOCK_ACCESS_FS_REFER)    // ABI >= 2
	accessTruncate   = uint64(unix.LANDLOCK_ACCESS_FS_TRUNCATE) // ABI >= 3
)

// landlockPathBeneath はカーネルの struct landlock_path_beneath_attr (12 バイト、
// packed) と同じ並び。Go の構造体は 16 バイトだが、カーネルは先頭 12 バイトだけを
// 読むので、フィールド位置が合っていればそのまま渡せる。
type landlockPathBeneath struct {
	AllowedAccess uint64
	ParentFd      int32
}

// landlockABI は対応している Landlock の ABI を返す。未対応なら 0。
func landlockABI() (int, error) {
	r1, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		if errno == unix.ENOSYS || errno == unix.EOPNOTSUPP {
			return 0, nil
		}
		return 0, fmt.Errorf("Landlock の ABI を調べられない: %w", errno)
	}
	return int(r1), nil
}

// applyLandlock は書き込み・作成・削除を paths の下だけに限る。読み取りは制限しない。
func applyLandlock(paths []string) error {
	abi, err := landlockABI()
	if err != nil {
		return err
	}
	if abi < 1 {
		return fmt.Errorf("Landlock がこのカーネルでは使えない")
	}

	handled := accessWriteFile | accessRemoveFile | accessRemoveDir |
		accessMakeChar | accessMakeDir | accessMakeReg | accessMakeSock |
		accessMakeFifo | accessMakeBlock | accessMakeSym
	if abi >= 2 {
		handled |= accessRefer
	}
	if abi >= 3 {
		handled |= accessTruncate
	}

	// ruleset_attr の大きさは ABI で変わる (net は ABI >= 4、scoped は ABI >= 6)。
	size := 8
	if abi >= 4 {
		size = 16
	}
	if abi >= 6 {
		size = 24
	}
	attr := make([]byte, size)
	binary.LittleEndian.PutUint64(attr[0:], handled)
	rfd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr[0])), uintptr(len(attr)), 0)
	if errno != 0 {
		return fmt.Errorf("Landlock の ruleset を作れない: %w", errno)
	}
	defer unix.Close(int(rfd))

	for _, p := range paths {
		if p == "" {
			continue
		}
		fd, err := unix.Open(p, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err != nil {
			if err == unix.ENOENT || err == unix.ENOTDIR {
				continue // 無いパスは飛ばす (VM によっては /var/tmp が無い等)
			}
			return fmt.Errorf("Landlock: %s を開けない: %w", p, err)
		}
		rule := landlockPathBeneath{AllowedAccess: handled, ParentFd: int32(fd)}
		_, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, rfd,
			unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&rule)), 0, 0, 0)
		unix.Close(fd)
		if errno != 0 {
			return fmt.Errorf("Landlock: %s に規則を足せない: %w", p, errno)
		}
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, rfd, 0, 0); errno != 0 {
		return fmt.Errorf("Landlock を効かせられない: %w", errno)
	}
	return nil
}
