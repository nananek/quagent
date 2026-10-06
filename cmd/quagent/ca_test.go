package main

import (
	"strings"
	"testing"
)

const testCAPEM = `-----BEGIN CERTIFICATE-----
MIIBtest
-----END CERTIFICATE-----`

func TestInjectCA(t *testing.T) {
	ep := "#!/bin/bash\n# quagent\ncd /work\nopencode --auto /work"
	got, files, cmd := injectCA(ep, testCAPEM)

	lines := strings.Split(got, "\n")
	if lines[0] != "#!/bin/bash" {
		t.Fatalf("shebang が先頭でない: %q", lines[0])
	}
	if lines[1] != caEnv {
		t.Fatalf("export が shebang の直後にない: %q", lines[1])
	}
	if !strings.Contains(got, "opencode --auto /work") {
		t.Fatalf("元の entrypoint が消えた: %q", got)
	}
	if strings.Index(got, caEnv) > strings.Index(got, "opencode") {
		t.Fatal("export がエージェントの起動より後にある")
	}

	if !strings.Contains(files, "  - path: "+caGuestPath) {
		t.Fatalf("CA の置き場所が無い: %q", files)
	}
	if !strings.Contains(files, "\n    content: |\n      -----BEGIN CERTIFICATE-----\n      MIIBtest\n      -----END CERTIFICATE-----") {
		t.Fatalf("CA の PEM がブロックスカラーになっていない: %q", files)
	}
	if !strings.Contains(cmd, "update-ca-certificates") {
		t.Fatalf("信頼ストアを更新する行が無い: %q", cmd)
	}
}

func TestInjectCANoop(t *testing.T) {
	ep := "#!/bin/bash\ncd /work"
	got, files, cmd := injectCA(ep, "")
	if got != ep || files != "" || cmd != "" {
		t.Fatalf("CA 無しで変わった: %q %q %q", got, files, cmd)
	}
}
