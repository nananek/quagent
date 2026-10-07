// quagent — プロジェクトごとに使い捨て qemu VM を立て、egress を host 側で
// 制限したうえでコーディングエージェントを動かす。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nananek/quagent/internal/access"
	"github.com/nananek/quagent/internal/config"
	"github.com/nananek/quagent/internal/console"
	"github.com/nananek/quagent/internal/guard"
	"github.com/nananek/quagent/internal/guest"
	"github.com/nananek/quagent/internal/hostsvc"
	"github.com/nananek/quagent/internal/image"
	"github.com/nananek/quagent/internal/netns"
	"github.com/nananek/quagent/internal/sandbox"
	"github.com/nananek/quagent/internal/tui"
)

const usage = `usage:
  quagent                                      TUI (VM の起動設定とベースイメージの管理)
  quagent image recipes                        使えるレシピ (OS) の一覧
  quagent image build [--refresh|--incremental] [RECIPE]  ベースイメージを焼く/差分更新する (既定: debian)
  quagent image ls                             焼いたベースイメージの一覧
  quagent image rm IMAGE                       ベースイメージを消す
  quagent always ls | rm DOMAIN...              「以後確認しない」ドメインの一覧・取り消し
  quagent guard check [TEXT]                    設定したローカル LLM でリクエストの中身を点検してみる
  quagent run [--repo DIR] [--image RECIPE] [--cpus N] [--mem MiB] [--agent opencode|claude|agy] [--allow "d1 d2"] [--ssh] [--mount-tmp] [--nested-virt] [--local-head] [--pr-approval]
                                               VM を起動し、tmux でエージェントと承認コンソールを開く
`

func main() {
	if err := dispatch(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "quagent: %v\n", err)
		os.Exit(1)
	}
}

func dispatch(args []string) error {
	if len(args) == 0 {
		if !isTerminal(os.Stdin) {
			fmt.Fprint(os.Stderr, usage)
			os.Exit(2)
		}
		l, err := tui.Run(agentNames())
		if errors.Is(err, tui.ErrQuit) {
			return nil
		}
		if err != nil {
			return err
		}
		return run(runOpts{Repo: l.Repo, Recipe: l.Recipe, CPUs: l.CPUs, MemMiB: l.MemMiB,
			MountTmp: l.MountTmp, SSH: l.SSH, NestedVirt: l.NestedVirt, LocalHead: l.LocalHead, Agent: l.Agent, PRApproval: l.PRApproval, Interactive: true,
			AfterSession: afterSession})
	}
	switch args[0] {
	case netns.ChildCommand:
		if len(args) != 2 {
			return fmt.Errorf("%s: spec のパスが必要", netns.ChildCommand)
		}
		return netns.RunChild(args[1])
	case guestCommand:
		if len(args) != 2 {
			return fmt.Errorf("%s: host の窓口のポートが必要", guestCommand)
		}
		port, err := strconv.ParseUint(args[1], 10, 32)
		if err != nil {
			return fmt.Errorf("%s: ポートが不正: %q", guestCommand, args[1])
		}
		return guest.Serve(hostsvc.GuestPort, uint32(port))
	case execCommand:
		return cmdExec(args[1:])
	case sandbox.LauncherCommand:
		return sandbox.Run(args[1:])
	case attachCommand:
		return cmdAttach(args[1:])
	case consoleCommand:
		if len(args) != 2 {
			return fmt.Errorf("%s: socket のパスが必要", consoleCommand)
		}
		return console.RunClient(args[1])
	case "image":
		return cmdImage(args[1:])
	case "run":
		return cmdRun(args[1:])
	case "always":
		return cmdAlways(args[1:])
	case "guard":
		return cmdGuard(args[1:])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	return nil
}

func cmdImage(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("image: recipes / build / ls / rm を指定する")
	}
	switch args[0] {
	case "recipes":
		rs, err := image.Recipes()
		if err != nil {
			return err
		}
		for _, r := range rs {
			fmt.Printf("%-10s %s (%s)\n", r.Name, r.Description, r.Source)
		}
		return nil
	case "build":
		fs := flag.NewFlagSet("image build", flag.ExitOnError)
		cpus := fs.Int("cpus", 4, "焼き込み VM の CPU 数")
		mem := fs.Int("mem", 4096, "焼き込み VM のメモリ (MiB)")
		refresh := fs.Bool("refresh", false, "クラウドイメージを取り直す")
		incremental := fs.Bool("incremental", false, "前回のイメージから差分更新する (カーネルは更新があるときだけ作り直す)")
		_ = fs.Parse(args[1:])
		name := image.DefaultRecipe
		if fs.NArg() > 0 {
			name = fs.Arg(0)
		}
		if *refresh && *incremental {
			return fmt.Errorf("--refresh と --incremental は同時に使えない")
		}
		r, err := image.FindRecipe(name)
		if err != nil {
			return err
		}
		img, err := image.Build(r, image.BuildOpts{CPUs: *cpus, MemMiB: *mem, Refresh: *refresh, Incremental: *incremental}, os.Stderr)
		if err != nil {
			return err
		}
		fmt.Println(img.Path)
		return nil
	case "ls":
		imgs, err := image.List("")
		if err != nil {
			return err
		}
		for _, img := range imgs {
			fmt.Printf("%-10s %s  %5.1f GiB  %s\n", img.Recipe, img.Built.Format("2006-01-02 15:04"),
				float64(img.Size)/(1<<30), img.Path)
		}
		return nil
	case "rm":
		if len(args) != 2 {
			return fmt.Errorf("image rm: イメージのパスを 1 つ指定する")
		}
		imgs, err := image.List("")
		if err != nil {
			return err
		}
		for _, img := range imgs {
			if img.Path == args[1] || filepath.Base(img.Path) == args[1] {
				return image.Remove(img)
			}
		}
		return fmt.Errorf("そのイメージは無い: %s", args[1])
	default:
		return fmt.Errorf("image: 不明なサブコマンド %q", args[0])
	}
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	repo := fs.String("repo", "", "VM に渡す git repo (既定: cwd の git toplevel)")
	cpus := fs.Int("cpus", 4, "VM の CPU 数")
	mem := fs.Int("mem", 8192, "VM のメモリ (MiB)")
	recipe := fs.String("image", image.DefaultRecipe, "使うベースイメージのレシピ (quagent image recipes)")
	agent := fs.String("agent", DefaultAgent, "VM 内で動かすエージェント ("+strings.Join(agentNames(), " / ")+")")
	mountTmp := fs.Bool("mount-tmp", false, "repo の .tmp と VM の /work/.tmp (64 MiB) を受け渡す (起動時にコピー、終了時に回収)")
	localHead := fs.Bool("local-head", false, "checkout 中のブランチをローカルの先頭 (未 push のコミットを含む) で渡す。既定は upstream の先頭")
	nested := fs.Bool("nested-virt", false, "VM の中で KVM を使えるようにする (VM の中で VM を動かすとき)")
	useSSH := fs.Bool("ssh", false, "人が ssh で VM に入れるようにする (quagent 自身の操作は vsock)")
	prApproval := fs.Bool("pr-approval", false, "PR の作成を承認コンソールで確認してから push する")
	allow := fs.String("allow", "", "egress を許すドメイン (空白区切り)。LLM API は認証プロキシ経由なので不要")
	_ = fs.Parse(args)
	interactive := isTerminal(os.Stdin)
	o := runOpts{
		Repo:        *repo,
		Recipe:      *recipe,
		CPUs:        *cpus,
		MemMiB:      *mem,
		Allow:       strings.Fields(*allow),
		Interactive: interactive,
		SSH:         *useSSH,
		MountTmp:    *mountTmp,
		NestedVirt:  *nested,
		LocalHead:   *localHead,
		Agent:       *agent,
		PRApproval:  *prApproval,
	}
	if interactive {
		o.AfterSession = afterSession
	}
	return run(o)
}

// afterSession はエージェントのセッションが終わったあとに TUI で次の操作を尋ねる。
// 起動し直すならエージェント名を、終了なら空を返す (終了のときだけログの扱いも尋ねる)。
func afterSession(current string, choices []string) (next string, discardLogs bool) {
	c := tui.AfterSession(choices, current)
	if c.Restart {
		return c.Agent, false
	}
	return "", c.DiscardLogs
}

func cmdAlways(args []string) error {
	if len(args) == 0 || args[0] == "ls" {
		list, err := access.LoadAlways(access.AlwaysPath())
		if err != nil {
			return err
		}
		for _, d := range list {
			fmt.Println(d)
		}
		return nil
	}
	if args[0] == "rm" && len(args) > 1 {
		removed, err := access.RemoveAlways(access.AlwaysPath(), args[1:])
		if err != nil {
			return err
		}
		if len(removed) == 0 {
			return fmt.Errorf("一覧に無い: %s", strings.Join(args[1:], " "))
		}
		fmt.Println("取り消した:", strings.Join(removed, " "), "(動いている VM には次の起動から効く)")
		return nil
	}
	return fmt.Errorf("always: ls か rm DOMAIN... を指定する")
}

// cmdGuard は設定したローカル LLM に、その場でリクエストを 1 件点検させる。
// Ollama などが動いているか、モデルが判定を返せるかを確かめるのに使う。
func cmdGuard(args []string) error {
	if len(args) == 0 || args[0] != "check" {
		return fmt.Errorf("guard: check [TEXT] を指定する")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	g, err := guard.New(cfg.Guard, log.New(os.Stderr, "", 0))
	if err != nil {
		return err
	}
	text := strings.Join(args[1:], " ")
	if text == "" {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, guard.MaxInspect))
		if err != nil {
			return err
		}
		text = string(b)
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("guard check: 点検する TEXT か標準入力が必要")
	}
	req := guard.Request{
		Provider: "cli", Method: http.MethodPost, Host: "example.com", Path: "/",
		Headers: http.Header{"User-Agent": {"quagent-guard-check"}},
		Body:    []byte(text),
	}
	v, err := g.Inspect(context.Background(), req)
	if err != nil {
		return fmt.Errorf("点検できない (Ollama などは動いている?): %w", err)
	}
	fmt.Printf("%s: %s\n", v.Action, v.Reason)
	if v.Evidence != "" {
		fmt.Printf("evidence: %s\n", v.Evidence)
	}
	if len(v.Categories) > 0 {
		fmt.Println("categories:", strings.Join(v.Categories, ", "))
	}
	return nil
}

// consoleCommand は承認コンソール UI を動かす隠しサブコマンド名。
const consoleCommand = "__console"

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
