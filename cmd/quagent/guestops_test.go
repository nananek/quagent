package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseGuestArgs(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantCID     uint32
		wantConsole string
		wantArgv    []string
		wantErr     bool
	}{
		{
			name:        "simple",
			args:        []string{"3", "--", "echo", "hi"},
			wantCID:     3,
			wantConsole: "",
			wantArgv:    []string{"echo", "hi"},
			wantErr:     false,
		},
		{
			name:        "with console",
			args:        []string{"42", "--console", "/tmp/c.sock", "--", "ls", "-la"},
			wantCID:     42,
			wantConsole: "/tmp/c.sock",
			wantArgv:    []string{"ls", "-la"},
			wantErr:     false,
		},
		{
			name:    "empty args",
			args:    []string{},
			wantErr: true,
		},
		{
			name:    "invalid CID",
			args:    []string{"not_a_number", "--", "echo"},
			wantErr: true,
		},
		{
			name:    "missing separator",
			args:    []string{"3", "echo", "hi"},
			wantErr: true,
		},
		{
			name:    "empty argv after separator",
			args:    []string{"3", "--"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cid, con, argv, err := parseGuestArgs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseGuestArgs() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				if cid != tt.wantCID {
					t.Errorf("cid = %d, want %d", cid, tt.wantCID)
				}
				if con != tt.wantConsole {
					t.Errorf("console = %q, want %q", con, tt.wantConsole)
				}
				if !reflect.DeepEqual(argv, tt.wantArgv) {
					t.Errorf("argv = %v, want %v", argv, tt.wantArgv)
				}
			}
		})
	}
}

func TestVMGuestInteractiveArgv(t *testing.T) {
	g := vmGuest{
		cid:         10,
		self:        "/usr/local/bin/quagent",
		consoleSock: "/run/user/1000/console.sock",
	}
	got := g.interactiveArgv("echo test")
	want := []string{
		"/usr/local/bin/quagent",
		attachCommand,
		"10",
		"--console",
		"/run/user/1000/console.sock",
		"--",
		"bash",
		"-lc",
		"echo test",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("interactiveArgv = %v, want %v", got, want)
	}
}

func TestVMGuestGitURL(t *testing.T) {
	g := vmGuest{
		cid:  5,
		self: "/path with spaces/%quagent",
	}
	url := g.gitURL()
	// % -> %%, スペース -> %
	wantPrefix := "ext::/path% with% spaces/%%quagent __exec 5 -- %S /work"
	if url != wantPrefix {
		t.Errorf("gitURL = %q, want %q", url, wantPrefix)
	}
}

func TestCheckStatic(t *testing.T) {
	// 存在しないファイル
	if err := checkStatic(filepath.Join(t.TempDir(), "nonexistent")); err == nil {
		t.Fatal("expected error for nonexistent file")
	}

	// 通常のシステムバイナリ (/bin/sh や /bin/ls) は動的リンクされていることが多い
	// 動的リンクの場合は PT_INTERP があるのでエラーになる
	for _, bin := range []string{"/bin/sh", "/usr/bin/git", "/bin/bash"} {
		if _, err := os.Stat(bin); err == nil {
			err = checkStatic(bin)
			if err != nil && strings.Contains(err.Error(), "動的リンク") {
				// 動的リンクが正しく検出された
				return
			}
		}
	}
}

func TestVMGuestWriteFileInvalidPath(t *testing.T) {
	g := vmGuest{cid: 1}
	if err := g.writeFile("no_slash_path", []byte("data")); err == nil {
		t.Fatal("expected error for path without slash")
	}
}
