package main

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nananek/quagent/internal/config"
)

// defaultSkillsMaxIn は設定未指定時の合計サイズ上限 (skills_max_mib の既定)。
const defaultSkillsMaxIn = config.DefaultSkillsMaxMiB << 20

func TestArchiveSkillsNonExistent(t *testing.T) {
	buf, count, err := archiveSkills(filepath.Join(t.TempDir(), "not-exist"), defaultSkillsMaxIn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 || buf != nil {
		t.Errorf("expected count=0 and buf=nil, got count=%d, buf=%v", count, buf)
	}
}

func TestArchiveSkillsEmpty(t *testing.T) {
	tmp := t.TempDir()
	buf, count, err := archiveSkills(tmp, defaultSkillsMaxIn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 || buf != nil {
		t.Errorf("expected count=0 and buf=nil, got count=%d, buf=%v", count, buf)
	}
}

func TestArchiveSkillsValid(t *testing.T) {
	tmp := t.TempDir()

	// 2 つのスキルディレクトリを作成
	skill1 := filepath.Join(tmp, "skill-one")
	if err := os.MkdirAll(skill1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill1, "SKILL.md"), []byte("# Skill 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(skill1, "run.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho ok"), 0o755); err != nil {
		t.Fatal(err)
	}

	skill2 := filepath.Join(tmp, "skill-two")
	if err := os.MkdirAll(skill2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill2, "SKILL.md"), []byte("# Skill 2"), 0o644); err != nil {
		t.Fatal(err)
	}

	buf, count, err := archiveSkills(tmp, defaultSkillsMaxIn)
	if err != nil {
		t.Fatalf("archiveSkills failed: %v", err)
	}
	if count != 3 {
		t.Errorf("expected count=3, got %d", count)
	}
	if buf == nil {
		t.Fatal("expected non-nil buf")
	}

	// tar の中身を検証
	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))
	entries := make(map[string]int64)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading tar failed: %v", err)
		}
		entries[hdr.Name] = hdr.Mode
	}

	wantEntries := []struct {
		name string
		mode int64
	}{
		{"skill-one/", 0o755},
		{"skill-one/SKILL.md", 0o644},
		{"skill-one/run.sh", 0o755},
		{"skill-two/", 0o755},
		{"skill-two/SKILL.md", 0o644},
	}
	for _, we := range wantEntries {
		mode, ok := entries[we.name]
		if !ok {
			t.Errorf("missing entry in tar: %q", we.name)
		} else if mode != we.mode {
			t.Errorf("entry %q mode = %o, want %o", we.name, mode, we.mode)
		}
	}
}

func TestArchiveSkillsTooLarge(t *testing.T) {
	tmp := t.TempDir()
	skillDir := filepath.Join(tmp, "large-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 上限 (16 MiB) を超えるファイルを作成
	bigFile := filepath.Join(skillDir, "big.dat")
	f, err := os.Create(bigFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(defaultSkillsMaxIn + 1024); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	_, _, err = archiveSkills(tmp, defaultSkillsMaxIn)
	if err == nil {
		t.Fatal("expected error for oversized skills directory, got nil")
	}
	if !strings.Contains(err.Error(), "大きすぎる") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestArchiveSkillsCustomLimit(t *testing.T) {
	tmp := t.TempDir()
	skillDir := filepath.Join(tmp, "large-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bigFile := filepath.Join(skillDir, "big.dat")
	f, err := os.Create(bigFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(defaultSkillsMaxIn + 1024); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	// 既定の上限では弾かれる
	if _, _, err := archiveSkills(tmp, defaultSkillsMaxIn); err == nil {
		t.Fatal("expected error with the default limit")
	}

	// 上限を緩めれば通る
	buf, count, err := archiveSkills(tmp, 64<<20)
	if err != nil {
		t.Fatalf("archiveSkills with relaxed limit failed: %v", err)
	}
	if count != 1 || buf == nil {
		t.Fatalf("expected count=1 and non-nil buf, got count=%d, buf=%v", count, buf)
	}
}

func TestSkillsMaxBytes(t *testing.T) {
	if got, want := (&config.Config{}).SkillsMaxBytes(), int64(defaultSkillsMaxIn); got != want {
		t.Errorf("default = %d, want %d", got, want)
	}
	if got, want := (&config.Config{SkillsMaxMiB: 64}).SkillsMaxBytes(), int64(64)<<20; got != want {
		t.Errorf("skills_max_mib=64 = %d, want %d", got, want)
	}
}

func TestLinkSkillsScript(t *testing.T) {
	script := linkSkillsScript()
	for _, expectedPath := range []string{
		"~/.config/opencode/skills",
		"~/.claude/skills",
		"~/.gemini/config/skills",
		"~/.gemini/antigravity-cli/skills",
		"ln -sfn",
	} {
		if !strings.Contains(script, expectedPath) {
			t.Errorf("linkSkillsScript missing %q", expectedPath)
		}
	}
}
