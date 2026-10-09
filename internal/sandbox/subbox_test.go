package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestMaskSensitivePaths_Empty(t *testing.T) {
	if err := MaskSensitivePaths("", []string{".claude"}); err != nil {
		t.Fatalf("expected nil for empty home, got: %v", err)
	}
	if err := MaskSensitivePaths("/some/path", nil); err != nil {
		t.Fatalf("expected nil for nil paths, got: %v", err)
	}
	if err := MaskSensitivePaths("/some/path", []string{""}); err != nil {
		t.Fatalf("expected nil for empty element, got: %v", err)
	}
}

func TestMaskSensitivePaths_NonExistent(t *testing.T) {
	dir := t.TempDir()
	if err := MaskSensitivePaths(dir, []string{"nonexistent_dir", "nonexistent_file"}); err != nil {
		t.Fatalf("expected no error for non-existent paths, got: %v", err)
	}
}

func TestRunSubbox_Empty(t *testing.T) {
	if err := RunSubbox(nil); err == nil {
		t.Errorf("expected error for nil argv")
	}
	if err := RunSubbox([]string{"--"}); err == nil {
		t.Errorf("expected error for argv with only '--'")
	}
}

func TestRunSubboxWith_InvalidPolicy(t *testing.T) {
	// Inside NS mode
	t.Setenv("QUAGENT_SUBBOX_NS", "1")
	dir := t.TempDir()
	badJSON := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(badJSON, []byte("{invalid json"), 0644); err != nil {
		t.Fatal(err)
	}

	err := RunSubboxWith([]string{"true"}, badJSON)
	if err == nil {
		t.Fatalf("expected error for invalid policy json")
	}
}

func TestRunSubboxWith_CommandNotFound(t *testing.T) {
	t.Setenv("QUAGENT_SUBBOX_NS", "1")
	dir := t.TempDir()
	emptyJSON := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(emptyJSON, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	err := RunSubboxWith([]string{"nonexistent_command_123456789"}, emptyJSON)
	if err == nil {
		t.Fatalf("expected error for non-existent command")
	}
}

func TestRunSubboxWith_ExecInsideNS(t *testing.T) {
	if os.Getenv("TEST_SUBBOX_EXEC_HELPER") == "1" {
		t.Setenv("QUAGENT_SUBBOX_NS", "1")
		policyFile := os.Getenv("TEST_POLICY_FILE")
		if err := RunSubboxWith([]string{"echo", "subbox_ok"}, policyFile); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}

	dir := t.TempDir()
	policyFile := filepath.Join(dir, "policy.json")
	p := Default()
	b, _ := p.JSON()
	_ = os.WriteFile(policyFile, b, 0644)

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunSubboxWith_ExecInsideNS$")
	cmd.Env = append(os.Environ(), "TEST_SUBBOX_EXEC_HELPER=1", "TEST_POLICY_FILE="+policyFile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper failed: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "subbox_ok") {
		t.Fatalf("expected subbox_ok in output: %s", out)
	}
}

func TestSubboxHelperProcess(t *testing.T) {
	if os.Getenv("QUAGENT_TEST_SUBBOX_HELPER") != "1" {
		return
	}
	// Subprocess called inside new user namespace
	home := os.Getenv("TEST_HOME")
	if home != "" {
		_ = MaskSensitivePaths(home, []string{".gemini", ".claude.json"})
		entries, _ := os.ReadDir(filepath.Join(home, ".gemini"))
		if len(entries) != 0 {
			os.Exit(3)
		}
		st, err := os.Stat(filepath.Join(home, ".claude.json"))
		if err != nil || st.Size() != 0 {
			os.Exit(4)
		}
	}
	os.Exit(0)
}

func TestRunSubbox_ForkNamespace(t *testing.T) {
	// Test spawning inside CLONE_NEWUSER and CLONE_NEWNS
	dir := t.TempDir()
	geminiDir := filepath.Join(dir, ".gemini")
	if err := os.MkdirAll(geminiDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(geminiDir, "token.txt"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	claudeFile := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(claudeFile, []byte("token"), 0600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestSubboxHelperProcess$")
	cmd.Env = append(os.Environ(), "QUAGENT_TEST_SUBBOX_HELPER=1", "TEST_HOME="+dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getuid(), Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getgid(), Size: 1},
		},
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("subbox helper execution failed: %v", err)
	}
}
