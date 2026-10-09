// Package resourcemon はホスト側で VM のストレージ消費量およびホスト空き容量を動的監視し、
// 枯渇前の早期警戒 (80%) および安全停止 (95% / ホスト残容量 < 2GiB) を行う。
package resourcemon

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// DefaultVirtualDiskBytes はゲスト仮想ディスクの総容量 (40 GiB)。
const DefaultVirtualDiskBytes = int64(40) * 1024 * 1024 * 1024

// Config は監視のパラメータ。
type Config struct {
	WorkDir           string
	OverlayPath       string
	BaseDiskPath      string
	HostSafetyFreeGiB int
	DiskWarnPercent   int
	DiskStopPercent   int
	MaxLogSizeMiB     int
	Interval          time.Duration
}

// Monitor はストレージおよびログの動的監視を行う。
type Monitor struct {
	cfg              Config
	virtualDiskBytes int64
	baseAllocated    int64
	mu               sync.Mutex
	warnedDisk       bool
	stopped          bool
	OnAlert          func(msg string)
	OnStop           func(reason string)
}

// New は新しい Monitor を作成する。
func New(cfg Config) *Monitor {
	if cfg.HostSafetyFreeGiB <= 0 {
		cfg.HostSafetyFreeGiB = 2
	}
	if cfg.DiskWarnPercent <= 0 {
		cfg.DiskWarnPercent = 80
	}
	if cfg.DiskStopPercent <= 0 {
		cfg.DiskStopPercent = 95
	}
	if cfg.MaxLogSizeMiB <= 0 {
		cfg.MaxLogSizeMiB = 50
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 2 * time.Second
	}

	var baseAlloc int64
	if cfg.BaseDiskPath != "" {
		var st unix.Stat_t
		if err := unix.Stat(cfg.BaseDiskPath, &st); err == nil {
			baseAlloc = st.Blocks * 512
		}
	}

	return &Monitor{
		cfg:              cfg,
		virtualDiskBytes: DefaultVirtualDiskBytes,
		baseAllocated:    baseAlloc,
	}
}

// SetVirtualDiskBytes は仮想ディスクの総容量を明示設定する (テスト用)。
func (m *Monitor) SetVirtualDiskBytes(b int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.virtualDiskBytes = b
}

// SetBaseAllocated はベースイメージの割り当てサイズを明示設定する (テスト用)。
func (m *Monitor) SetBaseAllocated(b int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.baseAllocated = b
}

// CheckStorage はストレージ使用状況を検査し、警告メッセージまたは停止理由を返す。
func (m *Monitor) CheckStorage() (alertMsg string, stopReason string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 1. ホスト作業ディレクトリのファイルシステム空き容量検査
	targetDir := m.cfg.WorkDir
	if targetDir == "" && m.cfg.OverlayPath != "" {
		targetDir = filepath.Dir(m.cfg.OverlayPath)
	}
	if targetDir != "" {
		var sfs unix.Statfs_t
		if err := unix.Statfs(targetDir, &sfs); err == nil {
			hostFreeBytes := int64(sfs.Bavail) * int64(sfs.Bsize)
			safetyLimit := int64(m.cfg.HostSafetyFreeGiB) * 1024 * 1024 * 1024
			if hostFreeBytes < safetyLimit {
				freeGiB := float64(hostFreeBytes) / (1024 * 1024 * 1024)
				stopReason = fmt.Sprintf("ホスト空き容量が安全下限を切りました (残り %.2f GiB < 下限 %d GiB)", freeGiB, m.cfg.HostSafetyFreeGiB)
				return "", stopReason, nil
			}
		}
	}

	// 2. overlay.qcow2 の物理消費量とゲスト使用率の計算
	if m.cfg.OverlayPath != "" {
		var st unix.Stat_t
		if err := unix.Stat(m.cfg.OverlayPath, &st); err == nil {
			overlayAllocated := st.Blocks * 512
			totalAllocated := m.baseAllocated + overlayAllocated
			if m.virtualDiskBytes > 0 {
				pct := int(float64(totalAllocated) * 100.0 / float64(m.virtualDiskBytes))
				allocGiB := float64(totalAllocated) / (1024 * 1024 * 1024)
				totalGiB := float64(m.virtualDiskBytes) / (1024 * 1024 * 1024)

				if pct >= m.cfg.DiskStopPercent {
					stopReason = fmt.Sprintf("仮想マシンディスク使用量が %d%% に達しました (%.1f GiB / %.1f GiB)", pct, allocGiB, totalGiB)
					return "", stopReason, nil
				}
				if pct >= m.cfg.DiskWarnPercent && !m.warnedDisk {
					m.warnedDisk = true
					alertMsg = fmt.Sprintf("仮想マシンディスク使用量が %d%% を超えました (%.1f GiB / %.1f GiB)。不要なファイルやキャッシュの削除を推奨します", pct, allocGiB, totalGiB)
				}
			}
		}
	}

	return alertMsg, "", nil
}

// CensorLogs は console.log などのログ肥大化を防止し、上限サイズを超えた場合に末尾を残して切り詰める。
func (m *Monitor) CensorLogs() {
	if m.cfg.WorkDir == "" || m.cfg.MaxLogSizeMiB <= 0 {
		return
	}
	limitBytes := int64(m.cfg.MaxLogSizeMiB) * 1024 * 1024
	consoleLog := filepath.Join(m.cfg.WorkDir, "console.log")

	st, err := os.Stat(consoleLog)
	if err != nil || st.Size() <= limitBytes {
		return
	}

	// ログが上限を超えた場合、末尾半分を保持して切り詰める
	keepBytes := limitBytes / 2
	f, err := os.OpenFile(consoleLog, os.O_RDWR, 0o600)
	if err != nil {
		return
	}
	defer f.Close()

	if _, err := f.Seek(-keepBytes, io.SeekEnd); err != nil {
		return
	}
	buf := make([]byte, keepBytes)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return
	}

	_ = f.Truncate(0)
	_, _ = f.Seek(0, io.SeekStart)
	_, _ = f.WriteString("[quagent: console.log が上限を超えたため古いログを切り詰めました]\n")
	_, _ = f.Write(buf[:n])
}

// Start は監視ループを開始する。ctx がキャンセルされるか停止条件を満たすと終了する。
func (m *Monitor) Start(ctx context.Context) {
	ticker := time.NewTicker(m.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.CensorLogs()
			alert, stop, _ := m.CheckStorage()
			if alert != "" && m.OnAlert != nil {
				m.OnAlert(alert)
			}
			if stop != "" {
				m.mu.Lock()
				already := m.stopped
				m.stopped = true
				m.mu.Unlock()
				if !already && m.OnStop != nil {
					m.OnStop(stop)
				}
				return
			}
		}
	}
}

// CheckHostFreeSpace は指定ディレクトリの空き容量が minFreeGiB 以上あるか調べる。
func CheckHostFreeSpace(dir string, minFreeGiB int) error {
	if minFreeGiB <= 0 {
		return nil
	}
	var sfs unix.Statfs_t
	if err := unix.Statfs(dir, &sfs); err != nil {
		return fmt.Errorf("ディスク空き容量の取得に失敗: %w", err)
	}
	freeBytes := int64(sfs.Bavail) * int64(sfs.Bsize)
	requiredBytes := int64(minFreeGiB) * 1024 * 1024 * 1024
	if freeBytes < requiredBytes {
		freeGiB := float64(freeBytes) / (1024 * 1024 * 1024)
		return fmt.Errorf("ホストの空きディスク容量が不足しています (残り: %.1f GiB, 必要: %d GiB)", freeGiB, minFreeGiB)
	}
	return nil
}
