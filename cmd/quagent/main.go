// quagent — プロジェクトごとに使い捨て qemu VM を立て、egress を host 側で
// 制限したうえでコーディングエージェントを動かす。
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nananek/quagent/internal/console"
	"github.com/nananek/quagent/internal/guest"
	"github.com/nananek/quagent/internal/image"
	"github.com/nananek/quagent/internal/netns"
	"github.com/nananek/quagent/internal/tui"
)

const usage = `usage:
  quagent                                      TUI (VM の起動設定とベースイメージの管理)
  quagent image recipes                        使えるレシピ (OS) の一覧
  quagent image build [--refresh] [RECIPE]     ベースイメージを焼く (既定: debian)
  quagent image ls                             焼いたベースイメージの一覧
  quagent image rm IMAGE                       ベースイメージを消す
  quagent run [--repo DIR] [--image RECIPE] [--cpus N] [--mem MiB] [--allow "d1 d2"] [--ssh] [--mount-tmp]
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
		l, err := tui.Run()
		if errors.Is(err, tui.ErrQuit) {
			return nil
		}
		if err != nil {
			return err
		}
		return run(runOpts{Repo: l.Repo, Recipe: l.Recipe, CPUs: l.CPUs, MemMiB: l.MemMiB,
			MountTmp: l.MountTmp, SSH: l.SSH, Interactive: true})
	}
	switch args[0] {
	case netns.ChildCommand:
		if len(args) != 2 {
			return fmt.Errorf("%s: spec のパスが必要", netns.ChildCommand)
		}
		return netns.RunChild(args[1])
	case guestCommand:
		return guest.Serve()
	case execCommand:
		return cmdExec(args[1:])
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
		_ = fs.Parse(args[1:])
		name := image.DefaultRecipe
		if fs.NArg() > 0 {
			name = fs.Arg(0)
		}
		r, err := image.FindRecipe(name)
		if err != nil {
			return err
		}
		img, err := image.Build(r, image.BuildOpts{CPUs: *cpus, MemMiB: *mem, Refresh: *refresh}, os.Stderr)
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
	mountTmp := fs.Bool("mount-tmp", false, "repo の .tmp を VM の /work/.tmp に読み書き可能でマウントする")
	useSSH := fs.Bool("ssh", false, "人が ssh で VM に入れるようにする (quagent 自身の操作は vsock)")
	allow := fs.String("allow", "", "egress を許すドメイン (空白区切り)。LLM API は認証プロキシ経由なので不要")
	_ = fs.Parse(args)
	return run(runOpts{
		Repo:        *repo,
		Recipe:      *recipe,
		CPUs:        *cpus,
		MemMiB:      *mem,
		Allow:       strings.Fields(*allow),
		Interactive: isTerminal(os.Stdin),
		SSH:         *useSSH,
		MountTmp:    *mountTmp,
	})
}

// consoleCommand は承認コンソール UI を動かす隠しサブコマンド名。
const consoleCommand = "__console"

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
