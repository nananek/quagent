package cgroup

import (
	"strings"
	"testing"
)

func TestAvailable(t *testing.T) {
	// Available() does not panic
	_ = Available()
}

func TestWrapCommand(t *testing.T) {
	cmd := []string{"unshare", "-Urm", "mycmd"}
	opts := Options{
		CPUQuotaPercent:   200,
		MemMiB:            4096,
		MemoryOverheadMiB: 512,
		TasksMax:          512,
	}

	wrapped := WrapCommand(cmd, opts)
	if !Available() {
		// If systemd-run is not available, it must return original cmd
		if len(wrapped) != len(cmd) {
			t.Fatalf("expected original cmd when not available, got %v", wrapped)
		}
		return
	}

	// When available, must start with systemd-run
	if wrapped[0] != "systemd-run" {
		t.Fatalf("expected wrapped[0] == systemd-run, got %q", wrapped[0])
	}

	joined := strings.Join(wrapped, " ")
	if !strings.Contains(joined, "CPUQuota=200%") {
		t.Errorf("expected CPUQuota=200%% in %q", joined)
	}
	if !strings.Contains(joined, "MemoryMax=4608M") {
		t.Errorf("expected MemoryMax=4608M in %q", joined)
	}
	if !strings.Contains(joined, "TasksMax=512") {
		t.Errorf("expected TasksMax=512 in %q", joined)
	}
	if !strings.Contains(joined, "unshare -Urm mycmd") {
		t.Errorf("expected trailing target cmd in %q", joined)
	}
}
