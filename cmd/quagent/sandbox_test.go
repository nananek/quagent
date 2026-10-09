package main

import (
	"strings"
	"testing"

	"github.com/nananek/quagent/internal/sandbox"
)

func TestSandboxWriteFile(t *testing.T) {
	p := sandbox.Default()
	b, err := p.JSON()
	if err != nil {
		t.Fatal(err)
	}
	got := sandboxWriteFile(b)

	if !strings.HasPrefix(got, "  - path: "+sandbox.ConfigPath+"\n") {
		t.Fatalf("置き場所が先頭にない: %q", got)
	}
	if !strings.Contains(got, "\n    permissions: '0644'\n") {
		t.Fatalf("権限が 0644 でない: %q", got)
	}
	if !strings.Contains(got, "\n    content: |\n      {\n") {
		t.Fatalf("JSON がブロックスカラーになっていない: %q", got)
	}
	// 中身の各行がブロックスカラーの字下げ (6 スペース) か空であること
	for _, line := range strings.Split(got, "\n") {
		if line == "" || strings.HasPrefix(line, "  - path:") ||
			strings.HasPrefix(line, "    permissions:") || strings.HasPrefix(line, "    content:") {
			continue
		}
		if !strings.HasPrefix(line, "      ") {
			t.Fatalf("字下げが浅い行がある: %q", line)
		}
	}
}

func TestDockerWriteFiles(t *testing.T) {
	p := sandbox.Default()
	seccompJSON, err := p.DockerSeccompJSON()
	if err != nil {
		t.Fatal(err)
	}
	daemonJSON, err := sandbox.GenerateDockerDaemonJSON(dockerSeccompGuestPath)
	if err != nil {
		t.Fatal(err)
	}
	got := dockerWriteFiles(seccompJSON, daemonJSON)
	if !strings.Contains(got, "path: "+dockerSeccompGuestPath) {
		t.Errorf("missing dockerSeccompGuestPath in %q", got)
	}
	if !strings.Contains(got, "path: /home/agent/.config/docker/daemon.json") {
		t.Errorf("missing daemon.json path in %q", got)
	}
	if !strings.Contains(got, "permissions: '0644'") {
		t.Errorf("missing permissions 0644 in %q", got)
	}
}

func TestSubboxWrapperFiles(t *testing.T) {
	got := subboxWrapperFiles()
	if !strings.Contains(got, "path: /usr/local/bin/bash") {
		t.Errorf("missing /usr/local/bin/bash in %q", got)
	}
	if !strings.Contains(got, "path: /usr/local/bin/sh") {
		t.Errorf("missing /usr/local/bin/sh in %q", got)
	}
	if !strings.Contains(got, "permissions: '0755'") {
		t.Errorf("missing permissions 0755 in %q", got)
	}
	if !strings.Contains(got, "__check_agent_child") {
		t.Errorf("missing __check_agent_child check in %q", got)
	}
	if !strings.Contains(got, "__subbox") {
		t.Errorf("missing __subbox in %q", got)
	}
}
