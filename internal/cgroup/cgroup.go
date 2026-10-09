// Package cgroup はホスト側 QEMU ランチャに cgroups v2 リソース制限を適用する。
package cgroup

import (
	"fmt"
	"os/exec"
)

// Options は cgroup スコープのオプション。
type Options struct {
	CPUQuotaPercent   int
	MemMiB            int
	MemoryOverheadMiB int
	TasksMax          int
}

// Available はホスト環境で systemd-run --user --scope が使用可能か判定する。
func Available() bool {
	cmd := exec.Command("systemd-run", "--user", "--scope", "--quiet", "true")
	return cmd.Run() == nil
}

// WrapCommand は systemd-run が利用可能であればコマンドラインを cgroup スコープでラップし、
// 利用できなければ元のコマンドラインをそのまま返す。
func WrapCommand(argv []string, opts Options) []string {
	if !Available() || len(argv) == 0 {
		return argv
	}
	args := []string{"--user", "--scope", "--quiet"}
	if opts.CPUQuotaPercent > 0 {
		args = append(args, "-p", fmt.Sprintf("CPUQuota=%d%%", opts.CPUQuotaPercent))
	}
	if opts.TasksMax > 0 {
		args = append(args, "-p", fmt.Sprintf("TasksMax=%d", opts.TasksMax))
	}
	if opts.MemMiB > 0 {
		totalM := opts.MemMiB
		if opts.MemoryOverheadMiB > 0 {
			totalM += opts.MemoryOverheadMiB
		}
		args = append(args, "-p", fmt.Sprintf("MemoryMax=%dM", totalM))
	}
	return append(append([]string{"systemd-run"}, args...), argv...)
}
