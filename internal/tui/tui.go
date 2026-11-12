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

	"github.com/nananek/quagent/internal/access"
	"github.com/nananek/quagent/internal/image"
	"github.com/nananek/quagent/internal/paths"
	"github.com/nananek/quagent/internal/proxycache"
)

// Launch は TUI で決めた起動設定。
type Launch struct {
	Repo   string `json:"repo"`
	Recipe string `json:"recipe"`
	CPUs   int    `json:"cpus"`
	MemMiB int    `json:"mem_mib"`
	// MountTmp は repo の .tmp と VM の /work/.tmp を受け渡す。
	MountTmp bool `json:"mount_tmp"`
	// Copy は git の履歴ではなく、Repo のディレクトリをそのまま /work へコピーする。
	Copy bool `json:"copy"`
	// WorkSize は Copy のときの /work ディスク容量 (空なら自動)。
	WorkSize string `json:"work_size"`
	// NestedVirt は VM の中で KVM を使えるようにする。
	NestedVirt bool `json:"nested_virt"`
	// LocalHead はブランチをローカルの先頭 (未 push のコミットを含む) で渡す。
	LocalHead bool   `json:"local_head"`
	Agent     string `json:"agent"`
	// PRApproval は PR の作成を承認コンソールで確認してから push する。
	PRApproval bool `json:"pr_approval"`
	// Hub は Web 遠隔操作 (quagent hub) にこのセッションを登録する。
	Hub bool `json:"hub"`
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
	l := Launch{Recipe: image.DefaultRecipe, CPUs: 4, MemMiB: 8192, PRApproval: true}
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
		f := newForm("ベースイメージがまだ無い")
		f.setNote("VM を起動するには、先にベースイメージを焼く必要がある。\n次に開く管理画面で焼いてから、起動する。")
		if st, err := f.run(); err != nil || st != formSubmitted {
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
		f := newForm("quagent")
		f.addSelect("", "", &choice,
			option{"VM を起動", "start", false},
			option{"ベースイメージの管理", "images", false},
			option{"キャッシュの全クリア", "cache", false},
			option{"「以後確認しない」ドメインの管理", "always", false},
			option{"終了", "quit", false},
		)
		st, err := f.run()
		if err != nil {
			return Launch{}, err
		}
		if st != formSubmitted || choice == "quit" {
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
		case "cache":
			if err := cleanCacheMenu(); err != nil && !errors.Is(err, errBack) {
				return Launch{}, err
			}
		case "always":
			if err := alwaysMenu(); err != nil && !errors.Is(err, errBack) {
				return Launch{}, err
			}
		}
	}
}

func cleanCacheMenu() error {
	stats, err := proxycache.InspectCache(paths.ProxyCacheDir())
	if err != nil {
		return err
	}
	var totalBytes int64 = stats.TotalBlobBytes
	for _, es := range stats.Packages {
		totalBytes += es.TotalBytes
	}
	desc := fmt.Sprintf("現在のキャッシュ: Docker %d レイヤー, 合計 %s", stats.TotalBlobs, proxycache.FormatSize(totalBytes))
	if totalBytes == 0 {
		f := newForm("キャッシュ")
		f.setNote("キャッシュは空です。")
		_, _ = f.run()
		return nil
	}

	var confirm bool
	f := newForm("キャッシュ")
	f.addConfirm("キャッシュを全削除しますか?", desc, &confirm, "削除", "戻る")
	st, err := f.run()
	if err != nil || st != formSubmitted || !confirm {
		return nil
	}

	if err := proxycache.CleanCache(paths.ProxyCacheDir()); err != nil {
		return fmt.Errorf("キャッシュ削除失敗: %w", err)
	}
	done := newForm("完了")
	done.setNote("キャッシュを全削除しました。")
	_, _ = done.run()
	return nil
}

// AfterChoice はエージェントのセッションが終わったあとに選んだ次の操作。
type AfterChoice struct {
	// Restart は同じ VM でエージェント Agent を起動し直す。false なら終了する。
	Restart bool
	Agent   string
	// DiscardLogs はこの VM のログ (host.log など) を残さない (終了のときだけ意味を持つ)。
	DiscardLogs bool
}

// agentRestartOptions は再起動先のエージェント候補に「(再起動をやめて) 戻る」を足した選択肢を返す。
func agentRestartOptions(agents []string) []option {
	var opts []option
	for _, a := range agents {
		opts = append(opts, option{label: a, value: a})
	}
	opts = append(opts, option{label: "(再起動をやめて) 戻る", value: "back"})
	return opts
}

// initialAgentChoice は現在または直前のエージェント名から、選択初期値を決める。
func initialAgentChoice(agents []string, current, picked string) string {
	if picked != "" && picked != "back" && slices.Contains(agents, picked) {
		return picked
	}
	if slices.Contains(agents, current) {
		return current
	}
	if len(agents) > 0 {
		return agents[0]
	}
	return ""
}

// AfterSession はセッション終了後に、エージェントを選び直して再起動するか終了するかを尋ね、
// 終了のときだけログを残すかを尋ねる。current は直前まで動かしていたエージェント (選択の初期値)。
// 再起動のエージェント選択では「(再起動をやめて) 戻る」や Esc で次の操作の選択に戻れる。
// 中断 (Ctrl-C) されたときは、従来どおり「終了・ログを残す」として扱う。
func AfterSession(agents []string, current string) AfterChoice {
	action, agent, logs := "restart", current, "keep"
	agentOpts := agentRestartOptions(agents)
	for {
		f := newForm("エージェントが終了した")
		f.setNote("再起動は同じ VM のまま行う。終了すると VM は破棄される (VM の中の作業は残らない)。")
		f.noBack = true // Esc で VM を捨てないようにする (選ぶのは「次の操作」だけ)
		f.addSelect("次の操作", "", &action,
			option{"エージェントを選び直して再起動 (同じ VM のまま)", "restart", false},
			option{"終了", "quit", false},
		)
		st, err := f.run()
		if err != nil || st != formSubmitted {
			return AfterChoice{}
		}
		if action == "quit" {
			break
		}

		picked := initialAgentChoice(agents, current, agent)
		pick := newForm("")
		pick.addSelect("エージェント", "", &picked, agentOpts...)
		st, err = pick.run()
		if err != nil {
			return AfterChoice{}
		}
		if st == formBack || picked == "back" {
			// 「(再起動をやめて) 戻る」または Esc が押されたら、再起動をやめて次の操作の選択に戻る。
			continue
		}

		return AfterChoice{Restart: true, Agent: picked}
	}

	f := newForm("")
	f.addSelect("この VM のログ", "host.log などの host 側の記録 (VM の中のものではない)", &logs,
		option{"残す", "keep", false},
		option{"残さない", "discard", false},
	)
	st, err := f.run()
	if err != nil || st != formSubmitted {
		return AfterChoice{}
	}
	return AfterChoice{Restart: false, DiscardLogs: logs == "discard"}
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

// defaultTarget は起動フォームの「渡し方」と「対象ディレクトリ」の初期値を決める。
// TUI は対象のディレクトリで quagent を実行する前提なので、前回の設定ではなく
// カレントディレクトリから決める。git 管理下なら toplevel を「git の履歴ごと」、
// そうでなければそのディレクトリを「そのままコピー」の初期値にする。
// カレントを取れないとき (cwd が空) だけ、前回の値に戻す。
func defaultTarget(cwd string, saved Launch) (repo string, copyMode bool) {
	if cwd == "" {
		return saved.Repo, saved.Copy
	}
	if top := gitTop(cwd); top != "" {
		return top, false
	}
	return cwd, true
}

// recipeOptions は焼けるレシピの選択肢を返す。焼いていないレシピは目立たせない。
func recipeOptions() ([]option, error) {
	rs, err := image.Recipes()
	if err != nil {
		return nil, err
	}
	var opts []option
	for _, r := range rs {
		label := r.Name + " — " + r.Description
		if img, err := image.Latest(r.Name); err == nil {
			label += "  [" + img.Built.Format("2006-01-02") + " に作成]"
			opts = append(opts, option{label: label, value: r.Name})
		} else {
			// 未ビルドのディストロは起動できないので、グレーにして見て分かるようにする
			// (選ぶと管理画面へ誘導する)。
			label += "  [未作成: 管理画面で焼く]"
			opts = append(opts, option{label: label, value: r.Name, dim: true})
		}
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
	var agentOpts []option
	for _, a := range agents {
		agentOpts = append(agentOpts, option{label: a, value: a})
	}
	if cwd, err := os.Getwd(); err == nil {
		l.Repo, l.Copy = defaultTarget(cwd, l)
	}
	opts, err := recipeOptions()
	if err != nil {
		return Launch{}, err
	}
	mode := "git"
	if l.Copy {
		mode = "copy"
	}
	cpus, mem := strconv.Itoa(l.CPUs), strconv.Itoa(l.MemMiB)
	workSize := l.WorkSize
	var extras []string
	if l.MountTmp {
		extras = append(extras, "tmp")
	}
	if l.NestedVirt {
		extras = append(extras, "nested")
	}
	if l.LocalHead {
		extras = append(extras, "local")
	}
	if l.Hub {
		extras = append(extras, "hub")
	}
	var prApproval []string
	if l.PRApproval {
		prApproval = append(prApproval, "pr")
	}
	ok := true

	f := newForm("VM の起動")
	f.addSelect("渡し方", "git の履歴を渡すか、データを含むディレクトリをそのまま渡すか", &mode,
		option{"git の履歴ごと (未コミットの変更は渡さない)", "git", false},
		option{"ディレクトリをそのままコピー (DB やアップロードも含む)", "copy", false},
	)
	f.addInput("対象ディレクトリ", "VM の /work に渡すディレクトリ", &l.Repo, func(s string) error {
		fi, err := os.Stat(s)
		if err != nil || !fi.IsDir() {
			return fmt.Errorf("ディレクトリではない")
		}
		if mode == "git" && gitTop(s) == "" {
			return fmt.Errorf("git repo ではない。そのままコピーするなら「渡し方」を切り替える")
		}
		return nil
	})
	f.addMulti("PR の作成", "エージェントの create_pull_request を承認コンソールで確認してから push する", &prApproval,
		option{"PR を承認制にする", "pr", false},
	)
	f.addSelect("ベースイメージ", "", &l.Recipe, opts...)
	f.addSelect("エージェント", "", &l.Agent, agentOpts...)
	f.addInput("CPU", "", &cpus, positiveInt)
	f.addInput("メモリ (MiB)", "", &mem, positiveInt)
	f.addInput("作業ディスク容量", "「そのままコピー」のときの /work の容量 (例: 20G)。空欄ならコピー元から自動", &workSize, nil)
	f.addMulti("オプション", "", &extras,
		option{"repo の .tmp と VM の /work/.tmp を受け渡す (終了時に回収)", "tmp", false},
		option{"VM の中で KVM を使えるようにする (入れ子の仮想化)", "nested", false},
		option{"ブランチをローカルの先頭で渡す (未 push のコミットも渡る)", "local", false},
		option{"Web 遠隔操作 (quagent hub) に登録する", "hub", false},
	)
	f.addConfirm("この設定で起動する?", "", &ok, "起動", "戻る")

	st, err := f.run()
	if err != nil {
		return Launch{}, err
	}
	switch st {
	case formAborted:
		return Launch{}, ErrQuit
	case formBack:
		return Launch{}, errBack
	}
	if !ok {
		return Launch{}, errBack
	}

	l.Copy = mode == "copy"
	l.WorkSize = strings.TrimSpace(workSize)
	if l.Copy {
		if abs, err := filepath.Abs(l.Repo); err == nil {
			l.Repo = abs
		}
	} else {
		l.Repo = gitTop(l.Repo)
		if l.Repo == "" {
			fmt.Println("git repo ではない。そのままコピーするなら「渡し方」を切り替える。")
			pause()
			return Launch{}, errBack
		}
	}
	l.MountTmp = slices.Contains(extras, "tmp")
	l.NestedVirt = slices.Contains(extras, "nested")
	l.LocalHead = slices.Contains(extras, "local")
	l.PRApproval = slices.Contains(prApproval, "pr")
	l.Hub = slices.Contains(extras, "hub")
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

		var opts []option
		for _, r := range rs {
			opts = append(opts,
				option{label: r.Name + " を焼く / 更新する", value: "build:" + r.Name})
		}
		opts = append(opts,
			option{label: "イメージを個別に消す (最新も消せる)", value: "delete"},
			option{label: "古いイメージを消す (各 OS の最新だけ残す)", value: "prune"},
			option{label: "戻る", value: "back"},
		)
		var choice string
		f := newForm("ベースイメージ")
		f.setNote(sb.String())
		f.addSelect("", "", &choice, opts...)
		st, err := f.run()
		if err != nil || st != formSubmitted || choice == "back" {
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
	var opts []option
	for _, img := range imgs {
		label := fmt.Sprintf("%s  %s  %.1f GiB", img.Recipe,
			img.Built.Format("2006-01-02 15:04"), float64(img.Size)/(1<<30))
		if !seen[img.Recipe] {
			label += "  (最新)"
			seen[img.Recipe] = true
		}
		opts = append(opts, option{label: label, value: img.Path})
	}
	var picked []string
	f := newForm("ベースイメージを消す")
	f.addMulti("消すイメージ (space で選択、enter で確定)",
		"各 OS の最新も消せる。起動に使う最新を消すと、次は焼き直しが要る", &picked, opts...)
	st, err := f.run()
	if err != nil || st != formSubmitted || len(picked) == 0 {
		return errBack
	}
	ok := false
	confirm := newForm("")
	confirm.addConfirm(fmt.Sprintf("%d 個のイメージを消す? (元に戻せない)", len(picked)), "", &ok, "消す", "戻る")
	st, err = confirm.run()
	if err != nil || st != formSubmitted || !ok {
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
	// 焼き込み VM の CPU・メモリを決める。既存のイメージは使わず、常にレシピから
	// 新しく焼き直す。
	s := loadBuildSettings()
	cpus, mem := strconv.Itoa(s.CPUs), strconv.Itoa(s.MemMiB)
	ok := true
	f := newForm(name + " を焼く")
	f.addInput("CPU (焼き込み VM)", "ホストの範囲で。多いほどビルドが速い", &cpus, positiveInt)
	f.addInput("メモリ MiB (焼き込み VM)", "", &mem, positiveInt)
	f.addConfirm(name+" を焼く?", "", &ok, "焼く", "戻る")
	st, err := f.run()
	if err != nil || st != formSubmitted || !ok {
		return nil
	}
	s.CPUs, _ = strconv.Atoi(cpus)
	s.MemMiB, _ = strconv.Atoi(mem)
	saveBuildSettings(s)
	img, err := image.Build(r, image.BuildOpts{
		CPUs: s.CPUs, MemMiB: s.MemMiB, Refresh: true,
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
	var opts []option
	for _, d := range list {
		opts = append(opts, option{label: d, value: d})
	}
	var picked []string
	f := newForm("「以後確認しない」ドメイン")
	f.addMulti("取り消すドメイン (space で選択、enter で確定)",
		"全プロジェクト共通。取り消すと次の起動から再び確認される", &picked, opts...)
	st, err := f.run()
	if err != nil || st != formSubmitted || len(picked) == 0 {
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
