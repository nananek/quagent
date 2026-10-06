package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nananek/quagent/internal/paths"
)

// 起動画面のベースイメージは Gentoo/Arch/Debian の順で、焼いてあるものは色を
// 付けず、未ビルドのものはグレー (端末の色) にする。
func TestRecipeOptions(t *testing.T) {
	t.Setenv("CLICOLOR_FORCE", "1") // 擬似端末でなくても色を出す
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	// gentoo だけ焼いてあることにする。
	dir := paths.ImagesDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "base-gentoo-20261007-010203.qcow2"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts, err := recipeOptions()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, o := range opts {
		names = append(names, o.Value)
	}
	if got := strings.Join(names, ","); got != "gentoo,arch,debian" {
		t.Fatalf("並び: got %q", got)
	}
	if strings.Contains(opts[0].Key, "\x1b[") {
		t.Fatalf("作成済みに色が付いている: %q", opts[0].Key)
	}
	for _, o := range opts[1:] {
		if !strings.Contains(o.Key, "\x1b[") || !strings.Contains(o.Key, "未作成") {
			t.Fatalf("未作成がグレーでない: %q", o.Key)
		}
	}
}
