// quagent — プロジェクトごとに使い捨て qemu VM を立て、egress を host 側で
// 制限したうえでコーディングエージェントを動かす。
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/nananek/quagent/internal/console"
	"github.com/nananek/quagent/internal/image"
	"github.com/nananek/quagent/internal/netns"
)

const usage = `usage:
  quagent image build [--cpus N] [--mem MiB]   ベースイメージを焼く
  quagent image ls                             焼いたベースイメージの一覧
  quagent run [--repo DIR] [--cpus N] [--mem MiB] [--allow "d1 d2"]
                                               VM を起動して /work に repo を置き ssh する
`

func main() {
	if err := dispatch(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "quagent: %v\n", err)
		os.Exit(1)
	}
}

func dispatch(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch args[0] {
	case netns.ChildCommand:
		if len(args) != 2 {
			return fmt.Errorf("%s: spec のパスが必要", netns.ChildCommand)
		}
		return netns.RunChild(args[1])
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
		return fmt.Errorf("image: build か ls を指定する")
	}
	switch args[0] {
	case "build":
		fs := flag.NewFlagSet("image build", flag.ExitOnError)
		cpus := fs.Int("cpus", 4, "焼き込み VM の CPU 数")
		mem := fs.Int("mem", 4096, "焼き込み VM のメモリ (MiB)")
		_ = fs.Parse(args[1:])
		path, err := image.Build(*cpus, *mem, os.Stderr)
		if err != nil {
			return err
		}
		fmt.Println(path)
		return nil
	case "ls":
		latest, err := image.Latest()
		if err != nil {
			return err
		}
		fmt.Println(latest)
		return nil
	default:
		return fmt.Errorf("image: 不明なサブコマンド %q", args[0])
	}
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	repo := fs.String("repo", "", "VM に渡す git repo (既定: cwd の git toplevel)")
	cpus := fs.Int("cpus", 4, "VM の CPU 数")
	mem := fs.Int("mem", 8192, "VM のメモリ (MiB)")
	allow := fs.String("allow", "", "egress を許すドメイン (空白区切り)。LLM API は認証プロキシ経由なので不要")
	_ = fs.Parse(args)
	return run(runOpts{
		Repo:        *repo,
		CPUs:        *cpus,
		MemMiB:      *mem,
		Allow:       strings.Fields(*allow),
		Interactive: isTerminal(os.Stdin),
	})
}

// consoleCommand は承認コンソール UI を動かす隠しサブコマンド名。
const consoleCommand = "__console"

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
