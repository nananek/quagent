// Package tui は起動時の TUI (VM の起動設定とベースイメージの管理)。
package tui

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"

	"github.com/nananek/quagent/internal/access"
	"github.com/nananek/quagent/internal/image"
	"github.com/nananek/quagent/internal/paths"
)

// Launch は TUI で決めた起動設定。
type Launch struct {
	Repo   string `json:"repo"`
	Recipe string `json:"recipe"`
	CPUs   int    `json:"cpus"`
	MemMiB int    `json:"mem_mib"`
	// MountTmp は repo の .tmp を VM にマウントする。SSH は人が ssh で入れるようにする。
	MountTmp bool   `json:"mount_tmp"`
	SSH      bool   `json:"ssh"`
	Agent    string `json:"agent"`
}

// ErrQuit は TUI で終了を選んだ (または中断した)。
var ErrQuit = errors.New("quit")

func lastPath() string { return filepath.Join(paths.StateDir(), "last.json") }

func loadLast() Launch {
	l := Launch{Recipe: image.DefaultRecipe, CPUs: 4, MemMiB: 8192}
	if b, err := os.ReadFile(lastPath()); err == nil {
		_ = json.Unmarshal(b, &l)
	}
	return l
}

func saveLast(l Launch) {
	if err := os.MkdirAll(filepath.Dir(lastPath()), 0o755); err != nil {
		return
	}
	if b, err := json.MarshalIndent(l, "", "  "); err == nil {
		_ = os.WriteFile(lastPath(), b, 0o644)
	}
}

// Run はメインメニューを出し、起動が選ばれたらその設定を返す。agents は選べるエージェント。
func Run(agents []string) (Launch, error) {
	for {
		var choice string
		err := huh.NewForm(huh.NewGroup(
			huh.NewSelect[string]().Title("quagent").Options(
				huh.NewOption("VM を起動", "start"),
				huh.NewOption("ベースイメージの管理", "images"),
				huh.NewOption("「以後確認しない」ドメインの管理", "always"),
				huh.NewOption("終了", "quit"),
			).Value(&choice),
		)).Run()
		if err != nil || choice == "quit" {
			return Launch{}, ErrQuit
		}
		switch choice {
		case "start":
			l, err := startForm(agents)
			if errors.Is(err, errBack) {
				continue
			}
			return l, err
		case "images":
			if err := imagesMenu(); err != nil && !errors.Is(err, errBack) {
				return Launch{}, err
			}
		case "always":
			if err := alwaysMenu(); err != nil && !errors.Is(err, errBack) {
				return Launch{}, err
			}
		}
	}
}

var errBack = errors.New("back")

func gitTop(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func recipeOptions() ([]huh.Option[string], error) {
	rs, err := image.Recipes()
	if err != nil {
		return nil, err
	}
	var opts []huh.Option[string]
	for _, r := range rs {
		label := r.Name + " — " + r.Description
		if img, err := image.Latest(r.Name); err == nil {
			label += "  [" + img.Built.Format("2006-01-02") + " に作成]"
		} else {
			label += "  [未作成: 起動前に焼く]"
		}
		opts = append(opts, huh.NewOption(label, r.Name))
	}
	return opts, nil
}

func startForm(agents []string) (Launch, error) {
	l := loadLast()
	if l.Agent == "" {
		l.Agent = "opencode"
	}
	var agentOpts []huh.Option[string]
	for _, a := range agents {
		agentOpts = append(agentOpts, huh.NewOption(a, a))
	}
	if cwd, err := os.Getwd(); err == nil {
		if top := gitTop(cwd); top != "" {
			l.Repo = top
		}
	}
	opts, err := recipeOptions()
	if err != nil {
		return Launch{}, err
	}
	cpus, mem := strconv.Itoa(l.CPUs), strconv.Itoa(l.MemMiB)
	positive := func(s string) error {
		if n, err := strconv.Atoi(s); err != nil || n <= 0 {
			return fmt.Errorf("正の整数を入れる")
		}
		return nil
	}
	var extras []string
	if l.MountTmp {
		extras = append(extras, "tmp")
	}
	if l.SSH {
		extras = append(extras, "ssh")
	}
	ok := true
	err = huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("対象 repo").Description("VM の /work にコピーする git repo").
			Value(&l.Repo).Validate(func(s string) error {
			if gitTop(s) == "" {
				return fmt.Errorf("git repo ではない")
			}
			return nil
		}),
		huh.NewSelect[string]().Title("ベースイメージ").Options(opts...).Value(&l.Recipe),
		huh.NewSelect[string]().Title("エージェント").Options(agentOpts...).Value(&l.Agent),
		huh.NewInput().Title("CPU").Value(&cpus).Validate(positive),
		huh.NewInput().Title("メモリ (MiB)").Value(&mem).Validate(positive),
		huh.NewMultiSelect[string]().Title("オプション").Options(
			huh.NewOption("repo の .tmp を VM の /work/.tmp にマウント (読み書き可)", "tmp"),
			huh.NewOption("ssh で入れるようにする", "ssh"),
		).Value(&extras),
		huh.NewConfirm().Title("この設定で起動する?").Affirmative("起動").Negative("戻る").Value(&ok),
	)).Run()
	if err != nil {
		return Launch{}, ErrQuit
	}
	if !ok {
		return Launch{}, errBack
	}
	l.Repo = gitTop(l.Repo)
	l.MountTmp, l.SSH = slices.Contains(extras, "tmp"), slices.Contains(extras, "ssh")
	l.CPUs, _ = strconv.Atoi(cpus)
	l.MemMiB, _ = strconv.Atoi(mem)
	saveLast(l)

	if _, err := image.Latest(l.Recipe); err != nil {
		build := true
		if err := huh.NewForm(huh.NewGroup(huh.NewConfirm().
			Title(l.Recipe + " のベースイメージがまだ無い。今から焼く? (数分かかる)").
			Affirmative("焼く").Negative("戻る").Value(&build))).Run(); err != nil || !build {
			return Launch{}, errBack
		}
		if err := buildImage(l.Recipe, false); err != nil {
			return Launch{}, err
		}
	}
	return l, nil
}

func imagesMenu() error {
	for {
		rs, err := image.Recipes()
		if err != nil {
			return err
		}
		var sb strings.Builder
		for _, r := range rs {
			imgs, _ := image.List(r.Name)
			fmt.Fprintf(&sb, "%s — %s\n", r.Name, r.Description)
			if r.Source != "builtin" {
				fmt.Fprintf(&sb, "  レシピ: %s\n", r.Source)
			}
			if len(imgs) == 0 {
				sb.WriteString("  (未作成)\n")
			}
			for i, img := range imgs {
				mark := "   "
				if i == 0 {
					mark = " ▶ "
				}
				fmt.Fprintf(&sb, " %s%s  %.1f GiB\n", mark, img.Built.Format("2006-01-02 15:04"), float64(img.Size)/(1<<30))
			}
		}
		sb.WriteString("\n▶ が起動に使われる (各 OS の最新)。\n利用者のレシピは " + image.RecipesDir() + "/<名前>/ に置く。")

		var opts []huh.Option[string]
		for _, r := range rs {
			opts = append(opts,
				huh.NewOption(r.Name+" を焼く / 更新する (OS の更新を取り込む)", "build:"+r.Name))
		}
		opts = append(opts,
			huh.NewOption("古いイメージを消す (各 OS の最新だけ残す)", "prune"),
			huh.NewOption("戻る", "back"))
		var choice string
		if err := huh.NewForm(huh.NewGroup(
			huh.NewNote().Title("ベースイメージ").Description(escapeMarkdown(sb.String())),
			huh.NewSelect[string]().Options(opts...).Value(&choice),
		)).Run(); err != nil || choice == "back" {
			return errBack
		}
		switch {
		case strings.HasPrefix(choice, "build:"):
			if err := buildImage(strings.TrimPrefix(choice, "build:"), true); err != nil {
				fmt.Fprintln(os.Stderr, "失敗:", err)
			}
			pause()
		case choice == "prune":
			n := 0
			for _, r := range rs {
				imgs, _ := image.List(r.Name)
				for _, img := range imgs[min(1, len(imgs)):] {
					if err := image.Remove(img); err != nil {
						fmt.Fprintln(os.Stderr, "削除に失敗:", err)
						continue
					}
					n++
				}
			}
			fmt.Printf("%d 個消した\n", n)
			pause()
		}
	}
}

func buildImage(name string, refresh bool) error {
	r, err := image.FindRecipe(name)
	if err != nil {
		return err
	}
	img, err := image.Build(r, image.BuildOpts{CPUs: 4, MemMiB: 4096, Refresh: refresh}, os.Stdout)
	if err != nil {
		return err
	}
	fmt.Println("できた:", img.Path)
	return nil
}

func pause() {
	fmt.Print("Enter で戻る")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

// escapeMarkdown は Note の説明文 (markdown として描画される) の特殊文字を無効にする。
func escapeMarkdown(s string) string {
	return strings.NewReplacer(`\`, `\\`, "*", `\*`, "_", `\_`, "`", "\\`", "<", `\<`).Replace(s)
}

// alwaysMenu は「以後確認しない」ドメインを一覧し、選んだものを取り消す。
func alwaysMenu() error {
	list, err := access.LoadAlways(access.AlwaysPath())
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("「以後確認しない」ドメインは無い")
		pause()
		return errBack
	}
	var opts []huh.Option[string]
	for _, d := range list {
		opts = append(opts, huh.NewOption(d, d))
	}
	var picked []string
	if err := huh.NewForm(huh.NewGroup(
		huh.NewMultiSelect[string]().Title("取り消すドメイン (space で選択、enter で確定)").
			Description("全プロジェクト共通。取り消すと次の起動から再び確認される").
			Options(opts...).Value(&picked),
	)).Run(); err != nil || len(picked) == 0 {
		return errBack
	}
	removed, err := access.RemoveAlways(access.AlwaysPath(), picked)
	if err != nil {
		return err
	}
	fmt.Println("取り消した:", strings.Join(removed, " "))
	pause()
	return errBack
}
