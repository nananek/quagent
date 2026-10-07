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
	"github.com/charmbracelet/lipgloss"

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
	// MountTmp は repo の .tmp と VM の /work/.tmp を受け渡す。SSH は人が ssh で入れるようにする。
	MountTmp bool `json:"mount_tmp"`
	SSH      bool `json:"ssh"`
	// NestedVirt は VM の中で KVM を使えるようにする。
	NestedVirt bool `json:"nested_virt"`
	// LocalHead はブランチをローカルの先頭 (未 push のコミットを含む) で渡す。
	LocalHead bool   `json:"local_head"`
	Agent     string `json:"agent"`
	// PRApproval は PR の作成を承認コンソールで確認してから push する。
	PRApproval bool `json:"pr_approval"`
}

// ErrQuit は TUI で終了を選んだ (または中断した)。
var ErrQuit = errors.New("quit")

func lastPath() string { return filepath.Join(paths.StateDir(), "last.json") }

// BuildSettings は焼き込み VM の設定 (TUI の管理画面で決める)。
type BuildSettings struct {
	CPUs   int `json:"cpus"`
	MemMiB int `json:"mem_mib"`
}

func buildSettingsPath() string { return filepath.Join(paths.StateDir(), "build.json") }

func loadBuildSettings() BuildSettings {
	s := BuildSettings{CPUs: 4, MemMiB: 4096}
	if b, err := os.ReadFile(buildSettingsPath()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func saveBuildSettings(s BuildSettings) {
	if err := os.MkdirAll(filepath.Dir(buildSettingsPath()), 0o755); err != nil {
		return
	}
	if b, err := json.MarshalIndent(s, "", "  "); err == nil {
		_ = os.WriteFile(buildSettingsPath(), b, 0o644)
	}
}

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
	// 初回 (まだ 1 つも焼いていない) は起動先が無いので、焼き込みへ誘導する。
	if !hasAnyImage() {
		if err := newForm(huh.NewGroup(huh.NewNote().
			Title("ベースイメージがまだ無い").
			Description("VM を起動するには、先にベースイメージを焼く必要がある。\n次に開く管理画面で焼いてから、起動する。"))).Run(); err != nil {
			return Launch{}, ErrQuit
		}
		if err := imagesMenu(); err != nil && !errors.Is(err, errBack) {
			return Launch{}, err
		}
		if !hasAnyImage() {
			return Launch{}, ErrQuit
		}
	}
	for {
		var choice string
		err := newForm(huh.NewGroup(
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
			if errors.Is(err, errNoImage) {
				if e := imagesMenu(); e != nil && !errors.Is(e, errBack) {
					return Launch{}, e
				}
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

// AfterChoice はエージェントのセッションが終わったあとに選んだ次の操作。
type AfterChoice struct {
	// Restart は新しい VM でエージェント Agent を起動し直す。false なら終了する。
	Restart bool
	Agent   string
	// DiscardLogs はこの VM のログ (host.log など) を残さない。
	DiscardLogs bool
}

// AfterSession はセッション終了後に、エージェントを選び直して再起動するか終了するか、
// ログを残すかを尋ねる。current は直前まで動かしていたエージェント (選択の初期値)。
// 中断 (Ctrl-C) されたときは、従来どおり「終了・ログを残す」として扱う。
func AfterSession(agents []string, current string) AfterChoice {
	action, agent, logs := "quit", current, "keep"
	var agentOpts []huh.Option[string]
	for _, a := range agents {
		agentOpts = append(agentOpts, huh.NewOption(a, a))
	}
	err := newForm(
		huh.NewGroup(
			huh.NewNote().Title("エージェントが終了した").
				Description("VM は、ここでの選択のあとに破棄される (VM の中の作業は残らない)。"),
			huh.NewSelect[string]().Title("次の操作").Options(
				huh.NewOption("エージェントを選び直して再起動 (新しい VM を起動する)", "restart"),
				huh.NewOption("終了", "quit"),
			).Value(&action),
		),
		huh.NewGroup(
			huh.NewSelect[string]().Title("エージェント").Options(agentOpts...).Value(&agent),
		).WithHideFunc(func() bool { return action != "restart" }),
		huh.NewGroup(
			huh.NewSelect[string]().Title("この VM のログ").
				Description("host.log などの host 側の記録 (VM の中のものではない)").
				Options(
					huh.NewOption("残す", "keep"),
					huh.NewOption("残さない", "discard"),
				).Value(&logs),
		),
	).Run()
	if err != nil {
		return AfterChoice{}
	}
	return AfterChoice{Restart: action == "restart", Agent: agent, DiscardLogs: logs == "discard"}
}

var errBack = errors.New("back")

// errNoImage は選んだレシピのベースイメージがまだ無い (管理画面へ誘導する)。
var errNoImage = errors.New("no image")

// hasAnyImage は焼いたベースイメージが 1 つ以上あるかを返す (初回起動の判定用)。
func hasAnyImage() bool {
	imgs, err := image.List("")
	return err == nil && len(imgs) > 0
}

func gitTop(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// unbuiltStyle は、まだ焼いていないレシピの行をグレーにする。16 色パレットの
// 8 (明るい黒) なので、明るい背景でも暗い背景でも控えめな灰色になる。
var unbuiltStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

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
			// 未ビルドのディストロは起動できないので、グレーにして見て分かるようにする
			// (選ぶと管理画面へ誘導する)。
			label = unbuiltStyle.Render(label + "  [未作成: 管理画面で焼く]")
		}
		opts = append(opts, huh.NewOption(label, r.Name))
	}
	return opts, nil
}

// positiveInt はフォームの入力が正の整数かを確かめる。
func positiveInt(s string) error {
	if n, err := strconv.Atoi(s); err != nil || n <= 0 {
		return fmt.Errorf("正の整数を入れる")
	}
	return nil
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
	var extras []string
	if l.MountTmp {
		extras = append(extras, "tmp")
	}
	if l.SSH {
		extras = append(extras, "ssh")
	}
	if l.NestedVirt {
		extras = append(extras, "nested")
	}
	if l.LocalHead {
		extras = append(extras, "local")
	}
	var prApproval []string
	if l.PRApproval {
		prApproval = append(prApproval, "pr")
	}
	ok := true
	err = newForm(huh.NewGroup(
		huh.NewInput().Title("対象 repo").Description("VM の /work にコピーする git repo").
			Value(&l.Repo).Validate(func(s string) error {
			if gitTop(s) == "" {
				return fmt.Errorf("git repo ではない")
			}
			return nil
		}),
		huh.NewMultiSelect[string]().Title("PR の作成").
			Description("エージェントの create_pull_request を承認コンソールで確認してから push する").
			Options(huh.NewOption("PR を承認制にする", "pr")).
			Value(&prApproval),
		huh.NewSelect[string]().Title("ベースイメージ").Options(opts...).Value(&l.Recipe),
		huh.NewSelect[string]().Title("エージェント").Options(agentOpts...).Value(&l.Agent),
		huh.NewInput().Title("CPU").Value(&cpus).Validate(positiveInt),
		huh.NewInput().Title("メモリ (MiB)").Value(&mem).Validate(positiveInt),
		huh.NewMultiSelect[string]().Title("オプション").Options(
			huh.NewOption("repo の .tmp と VM の /work/.tmp を受け渡す (終了時に回収)", "tmp"),
			huh.NewOption("ssh で入れるようにする", "ssh"),
			huh.NewOption("VM の中で KVM を使えるようにする (入れ子の仮想化)", "nested"),
			huh.NewOption("ブランチをローカルの先頭で渡す (未 push のコミットも渡る)", "local"),
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
	l.NestedVirt = slices.Contains(extras, "nested")
	l.LocalHead = slices.Contains(extras, "local")
	l.PRApproval = slices.Contains(prApproval, "pr")
	l.CPUs, _ = strconv.Atoi(cpus)
	l.MemMiB, _ = strconv.Atoi(mem)
	saveLast(l)

	// 焼き込みは起動画面では行わない (管理画面から)。まだ無ければそこへ誘導する。
	if _, err := image.Latest(l.Recipe); err != nil {
		fmt.Printf("%s のベースイメージがまだ無い。「ベースイメージの管理」で焼いてから起動する。\n", l.Recipe)
		pause()
		return Launch{}, errNoImage
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
			if r.Details != "" {
				fmt.Fprintf(&sb, "  %s\n", r.Details)
			}
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
				huh.NewOption(r.Name+" を焼く / 更新する", "build:"+r.Name))
		}
		opts = append(opts,
			huh.NewOption("イメージを個別に消す (最新も消せる)", "delete"),
			huh.NewOption("古いイメージを消す (各 OS の最新だけ残す)", "prune"),
			huh.NewOption("戻る", "back"))
		var choice string
		if err := newForm(huh.NewGroup(
			huh.NewNote().Title("ベースイメージ").Description(escapeMarkdown(sb.String())),
			huh.NewSelect[string]().Options(opts...).Value(&choice),
		)).Run(); err != nil || choice == "back" {
			return errBack
		}
		switch {
		case strings.HasPrefix(choice, "build:"):
			if err := buildImage(strings.TrimPrefix(choice, "build:")); err != nil {
				fmt.Fprintln(os.Stderr, "失敗:", err)
			}
			pause()
		case choice == "delete":
			if err := deleteImagesMenu(); err != nil && !errors.Is(err, errBack) {
				return err
			}
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

// deleteImagesMenu は焼いたイメージを一覧し、選んだものを消す (各 OS の最新も消せる)。
func deleteImagesMenu() error {
	imgs, err := image.List("")
	if err != nil {
		return err
	}
	if len(imgs) == 0 {
		fmt.Println("消せるイメージが無い")
		pause()
		return errBack
	}
	seen := map[string]bool{}
	var opts []huh.Option[string]
	for _, img := range imgs {
		label := fmt.Sprintf("%s  %s  %.1f GiB", img.Recipe,
			img.Built.Format("2006-01-02 15:04"), float64(img.Size)/(1<<30))
		if !seen[img.Recipe] {
			label += "  (最新)"
			seen[img.Recipe] = true
		}
		opts = append(opts, huh.NewOption(label, img.Path))
	}
	var picked []string
	if err := newForm(huh.NewGroup(
		huh.NewMultiSelect[string]().Title("消すイメージ (space で選択、enter で確定)").
			Description("各 OS の最新も消せる。起動に使う最新を消すと、次は焼き直しが要る").
			Options(opts...).Value(&picked),
	)).Run(); err != nil || len(picked) == 0 {
		return errBack
	}
	ok := false
	if err := newForm(huh.NewGroup(huh.NewConfirm().
		Title(fmt.Sprintf("%d 個のイメージを消す? (元に戻せない)", len(picked))).
		Affirmative("消す").Negative("戻る").Value(&ok))).Run(); err != nil || !ok {
		return errBack
	}
	n := 0
	for _, p := range picked {
		for _, img := range imgs {
			if img.Path == p {
				if err := image.Remove(img); err != nil {
					fmt.Fprintln(os.Stderr, "削除に失敗:", err)
					continue
				}
				n++
			}
		}
	}
	fmt.Printf("%d 個消した\n", n)
	pause()
	return errBack
}

func buildImage(name string) error {
	r, err := image.FindRecipe(name)
	if err != nil {
		return err
	}
	// 焼き方 (差分更新 / 焼き直し) と、焼き込み VM の CPU・メモリを決める。
	// 前回のイメージがあれば差分更新を既定にする (カーネルの作り直しを避ける)。
	mode := "fresh"
	var fields []huh.Field
	if _, err := image.Latest(name); err == nil {
		mode = "incremental"
		fields = append(fields, huh.NewSelect[string]().Title("焼き方").Options(
			huh.NewOption("差分更新 (前回のイメージから。カーネルは更新があるときだけ)", "incremental"),
			huh.NewOption("焼き直し (クラウドイメージから)", "fresh"),
		).Value(&mode))
	}
	s := loadBuildSettings()
	cpus, mem := strconv.Itoa(s.CPUs), strconv.Itoa(s.MemMiB)
	ok := true
	fields = append(fields,
		huh.NewInput().Title("CPU (焼き込み VM)").Description("ホストの範囲で。多いほどカーネルの作り直しが速い").
			Value(&cpus).Validate(positiveInt),
		huh.NewInput().Title("メモリ MiB (焼き込み VM)").Value(&mem).Validate(positiveInt),
		huh.NewConfirm().Title(name+" を焼く / 更新する?").Affirmative("焼く").Negative("戻る").Value(&ok),
	)
	if err := newForm(huh.NewGroup(fields...)).Run(); err != nil || !ok {
		return nil
	}
	s.CPUs, _ = strconv.Atoi(cpus)
	s.MemMiB, _ = strconv.Atoi(mem)
	saveBuildSettings(s)
	img, err := image.Build(r, image.BuildOpts{
		CPUs: s.CPUs, MemMiB: s.MemMiB,
		Refresh: mode == "fresh", Incremental: mode == "incremental",
	}, os.Stdout)
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
	if err := newForm(huh.NewGroup(
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

// newForm は端末の 16 色パレットに従う配色のフォームを作る。実際の色は利用者の
// 端末の配色で決まるので、明るい背景でも暗い背景でも読める。
func newForm(groups ...*huh.Group) *huh.Form {
	return huh.NewForm(groups...).WithTheme(theme())
}

// theme は ThemeBase16 から、明るい背景で読めない色を外したもの。
// 白 (7) の文字は端末の既定の文字色に、黄 (3) の印は青 (4) にする。
func theme() *huh.Theme {
	t := huh.ThemeBase16()
	blue := lipgloss.Color("4")
	for _, f := range []*huh.FieldStyles{&t.Focused, &t.Blurred} {
		f.Option = f.Option.UnsetForeground()
		f.UnselectedOption = f.UnselectedOption.UnsetForeground()
		f.TextInput.Text = f.TextInput.Text.UnsetForeground()
		f.SelectSelector = f.SelectSelector.Foreground(blue)
		f.MultiSelectSelector = f.MultiSelectSelector.Foreground(blue)
		f.NextIndicator = f.NextIndicator.Foreground(blue)
		f.PrevIndicator = f.PrevIndicator.Foreground(blue)
		f.TextInput.Prompt = f.TextInput.Prompt.Foreground(blue)
	}
	return t
}
