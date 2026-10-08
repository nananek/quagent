package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nananek/quagent/internal/paths"
)

// 起動画面のベースイメージは Gentoo/Arch の順で、焼いてあるものは色を
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
	if got := strings.Join(names, ","); got != "gentoo,arch" {
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

func TestEscapeMarkdown(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`plain text`, `plain text`},
		{`*bold* and _italic_`, `\*bold\* and \_italic\_`},
		{"`code` and <tag>", "\\`code\\` and \\<tag>"},
		{`path\with\backslash`, `path\\with\\backslash`},
	}
	for _, c := range cases {
		if got := escapeMarkdown(c.in); got != c.want {
			t.Errorf("escapeMarkdown(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPositiveInt(t *testing.T) {
	if err := positiveInt("1"); err != nil {
		t.Errorf("positiveInt(1) error: %v", err)
	}
	if err := positiveInt("4096"); err != nil {
		t.Errorf("positiveInt(4096) error: %v", err)
	}

	for _, bad := range []string{"0", "-1", "abc", "", "1.5"} {
		if err := positiveInt(bad); err == nil {
			t.Errorf("positiveInt(%q) accepted, want error", bad)
		}
	}
}

func TestBuildSettingsPersistence(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	// デフォルト値
	sDefault := loadBuildSettings()
	if sDefault.CPUs != 4 || sDefault.MemMiB != 4096 {
		t.Errorf("default build settings: %+v", sDefault)
	}

	// 保存して再読み込み
	saved := BuildSettings{CPUs: 8, MemMiB: 16384}
	saveBuildSettings(saved)

	loaded := loadBuildSettings()
	if loaded != saved {
		t.Errorf("loaded build settings: %+v, want %+v", loaded, saved)
	}
}

func TestLastLaunchPersistence(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	// デフォルト値
	lDefault := loadLast()
	if lDefault.CPUs != 4 || lDefault.MemMiB != 8192 || !lDefault.PRApproval {
		t.Errorf("default launch: %+v", lDefault)
	}

	// 保存して再読み込み
	saved := Launch{
		Recipe:     "arch",
		CPUs:       12,
		MemMiB:     32768,
		Agent:      "claude",
		PRApproval: false,
	}
	saveLast(saved)

	loaded := loadLast()
	if loaded.Recipe != saved.Recipe || loaded.CPUs != saved.CPUs || loaded.MemMiB != saved.MemMiB || loaded.Agent != saved.Agent || loaded.PRApproval != saved.PRApproval {
		t.Errorf("loaded launch: %+v, want %+v", loaded, saved)
	}
}

func TestTheme(t *testing.T) {
	th := theme()
	if th == nil {
		t.Fatal("theme() returned nil")
	}
}

func TestGitTop(t *testing.T) {
	// git リポジトリ内: 一時ディレクトリを git init して確認する。
	// 以前は "/work" 決め打ちだったため CI (/home/runner/work/...) で失敗した。
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v: %s", repo, err, out)
	}
	top := gitTop(repo)
	if top == "" {
		t.Fatal("expected gitTop to return non-empty repository root for git repo")
	}
	// サブディレクトリからもトップレベルに解決される。
	sub := filepath.Join(repo, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := gitTop(sub); got != top {
		t.Errorf("gitTop(sub) = %q, want %q", got, top)
	}

	// git 管理外のディレクトリ
	plain := t.TempDir()
	if got := gitTop(plain); got != "" {
		t.Errorf("expected empty string for non-git dir, got %q", got)
	}

	// 存在しないディレクトリ
	nonExistent := filepath.Join(t.TempDir(), "nonexistent")
	if got := gitTop(nonExistent); got != "" {
		t.Errorf("expected empty string for nonexistent dir, got %q", got)
	}
}

func TestHasAnyImage(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	// 初期状態: イメージなし
	if hasAnyImage() {
		t.Error("expected hasAnyImage = false when no images exist")
	}

	// 1つ作成
	dir := paths.ImagesDir()
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "base-gentoo-20261008-120000.qcow2"), []byte("data"), 0o644)
	if !hasAnyImage() {
		t.Error("expected hasAnyImage = true when an image exists")
	}
}
