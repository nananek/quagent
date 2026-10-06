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
