package resourcemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckHostFreeSpace(t *testing.T) {
	tmp := t.TempDir()

	// 1 GiB は通常空いているはず
	if err := CheckHostFreeSpace(tmp, 1); err != nil {
		t.Fatalf("CheckHostFreeSpace(1GiB) unexpected error: %v", err)
	}

	// 999999 GiB は確実に不足エラーになるはず
	if err := CheckHostFreeSpace(tmp, 999999); err == nil {
		t.Fatal("expected error for 999999 GiB free space check, got nil")
	}
}

func TestCheckStorageThresholds(t *testing.T) {
	tmp := t.TempDir()
	overlay := filepath.Join(tmp, "overlay.qcow2")
	if err := os.WriteFile(overlay, []byte("dummy overlay content"), 0o644); err != nil {
		t.Fatal(err)
	}

	mon := New(Config{
		WorkDir:           tmp,
		OverlayPath:       overlay,
		HostSafetyFreeGiB: 1, // 通常通る値
		DiskWarnPercent:   80,
		DiskStopPercent:   95,
	})

	// 仮想ディスク 100 MiB、ベース割り当てを 50 MiB とする (50%)
	mon.SetVirtualDiskBytes(100 * 1024 * 1024)
	mon.SetBaseAllocated(50 * 1024 * 1024)

	// 50% 使用中 -> 警告も停止もなし
	alert, stop, err := mon.CheckStorage()
	if err != nil {
		t.Fatal(err)
	}
	if alert != "" || stop != "" {
		t.Fatalf("expected no alert or stop, got alert=%q, stop=%q", alert, stop)
	}

	// 85 MiB 使用中 (85%) -> 80% 警告
	mon.SetBaseAllocated(85 * 1024 * 1024)
	alert, stop, err = mon.CheckStorage()
	if err != nil {
		t.Fatal(err)
	}
	if alert == "" || !strings.Contains(alert, "85%") {
		t.Fatalf("expected 85%% alert, got alert=%q", alert)
	}
	if stop != "" {
		t.Fatalf("expected no stop at 85%%, got stop=%q", stop)
	}

	// 2 回目は警告が重複送信されないことを確認
	alert2, _, _ := mon.CheckStorage()
	if alert2 != "" {
		t.Fatalf("expected warning only once, got second alert=%q", alert2)
	}

	// 96 MiB 使用中 (96%) -> 95% 停止
	mon.SetBaseAllocated(96 * 1024 * 1024)
	_, stop, err = mon.CheckStorage()
	if err != nil {
		t.Fatal(err)
	}
	if stop == "" || !strings.Contains(stop, "96%") {
		t.Fatalf("expected 96%% stop reason, got stop=%q", stop)
	}
}

func TestCensorLogs(t *testing.T) {
	tmp := t.TempDir()
	consoleLog := filepath.Join(tmp, "console.log")

	// 1 MB 制限に対して 2 MB のログを生成
	content := strings.Repeat("0123456789abcdef", 128*1024) // 2 MiB
	if err := os.WriteFile(consoleLog, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	mon := New(Config{
		WorkDir:       tmp,
		MaxLogSizeMiB: 1, // 1 MiB 制限
	})

	mon.CensorLogs()

	st, err := os.Stat(consoleLog)
	if err != nil {
		t.Fatal(err)
	}

	// 切り詰められて 1 MiB 以下になっていることを確認
	if st.Size() > 1024*1024+100 {
		t.Fatalf("expected truncated log size <= ~1MB, got %d", st.Size())
	}

	b, _ := os.ReadFile(consoleLog)
	if !strings.Contains(string(b), "[quagent: console.log が上限を超えたため古いログを切り詰めました]") {
		t.Fatal("expected truncation notice in log file")
	}
}

func TestMonitorStartStop(t *testing.T) {
	tmp := t.TempDir()
	overlay := filepath.Join(tmp, "overlay.qcow2")
	_ = os.WriteFile(overlay, []byte("test"), 0o644)

	mon := New(Config{
		WorkDir:         tmp,
		OverlayPath:     overlay,
		DiskWarnPercent: 80,
		DiskStopPercent: 90,
		Interval:        10 * time.Millisecond,
	})
	mon.SetVirtualDiskBytes(100 * 1024 * 1024)
	mon.SetBaseAllocated(95 * 1024 * 1024) // 95% -> 即停止

	stopChan := make(chan string, 1)
	mon.OnStop = func(reason string) {
		stopChan <- reason
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go mon.Start(ctx)

	select {
	case reason := <-stopChan:
		if !strings.Contains(reason, "95%") {
			t.Errorf("expected 95%% in stop reason, got %q", reason)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for OnStop callback")
	}
}

func TestCheckHostFreeSpace_Zero(t *testing.T) {
	if err := CheckHostFreeSpace("/any/path", 0); err != nil {
		t.Fatalf("expected nil when minFreeGiB is 0, got %v", err)
	}
	if err := CheckHostFreeSpace("/any/path", -1); err != nil {
		t.Fatalf("expected nil when minFreeGiB is -1, got %v", err)
	}
}

func TestNewWithBaseDisk(t *testing.T) {
	tmp := t.TempDir()
	baseDisk := filepath.Join(tmp, "base.raw")
	if err := os.WriteFile(baseDisk, make([]byte, 8192), 0o644); err != nil {
		t.Fatal(err)
	}

	mon := New(Config{
		BaseDiskPath: baseDisk,
	})
	if mon.baseAllocated <= 0 {
		t.Errorf("expected baseAllocated > 0, got %d", mon.baseAllocated)
	}
}

func TestCheckStorage_HostSafetyLimit(t *testing.T) {
	tmp := t.TempDir()
	mon := New(Config{
		WorkDir:           tmp,
		HostSafetyFreeGiB: 999999, // 確実に現在の空き容量を上回る
	})

	_, stopReason, err := mon.CheckStorage()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stopReason, "ホスト空き容量が安全下限を切りました") {
		t.Fatalf("expected host safety stop reason, got %q", stopReason)
	}
}

func TestMonitorStartAlert(t *testing.T) {
	tmp := t.TempDir()
	overlay := filepath.Join(tmp, "overlay.qcow2")
	_ = os.WriteFile(overlay, []byte("test"), 0o644)

	mon := New(Config{
		WorkDir:         tmp,
		OverlayPath:     overlay,
		DiskWarnPercent: 80,
		DiskStopPercent: 95,
		Interval:        10 * time.Millisecond,
	})
	mon.SetVirtualDiskBytes(100 * 1024 * 1024)
	mon.SetBaseAllocated(85 * 1024 * 1024) // 85% -> 警告発報

	alertChan := make(chan string, 1)
	mon.OnAlert = func(msg string) {
		select {
		case alertChan <- msg:
		default:
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go mon.Start(ctx)

	select {
	case msg := <-alertChan:
		if !strings.Contains(msg, "85%") {
			t.Errorf("expected 85%% in alert msg, got %q", msg)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for OnAlert callback")
	}
}

func TestCensorLogs_EdgeCases(t *testing.T) {
	tmp := t.TempDir()

	// 1. WorkDir が空
	mon1 := New(Config{WorkDir: ""})
	mon1.CensorLogs()

	// 2. MaxLogSizeMiB が 0 以下
	mon2 := New(Config{WorkDir: tmp, MaxLogSizeMiB: -1})
	mon2.CensorLogs()

	// 3. ファイルが存在しない
	mon3 := New(Config{WorkDir: tmp, MaxLogSizeMiB: 10})
	mon3.CensorLogs()

	// 4. ファイルが存在するが上限以下
	consoleLog := filepath.Join(tmp, "console.log")
	_ = os.WriteFile(consoleLog, []byte("small log"), 0o644)
	mon3.CensorLogs()

	data, _ := os.ReadFile(consoleLog)
	if string(data) != "small log" {
		t.Errorf("expected untouched log, got %q", string(data))
	}
}
