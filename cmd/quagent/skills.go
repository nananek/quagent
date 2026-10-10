package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// maxSkillsIn は VM へ渡すホスト側スキルの合計サイズ上限 (16 MiB)。
const maxSkillsIn = 16 << 20

// archiveSkills は hostSkillsDir 配下のスキル群を tar アーカイブにまとめる。
// ディレクトリが存在しないか通常ファイルが無い場合は count = 0, buf = nil, err = nil を返す。
// 通常ファイルとディレクトリのみを含め、サイズ上限 (maxSkillsIn) を超えるとエラーを返す。
func archiveSkills(dir string) (*bytes.Buffer, int, error) {
	st, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	if !st.IsDir() {
		return nil, 0, fmt.Errorf("%s はディレクトリではない", dir)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	var total int64
	var count int

	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			return tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeDir,
				Name:     filepath.ToSlash(rel) + "/",
				Mode:     0o755,
				ModTime:  info.ModTime(),
			})
		case info.Mode().IsRegular():
			if total+info.Size() > maxSkillsIn {
				return fmt.Errorf("%s が大きすぎる (VM へ渡すスキルは合計 %d MiB まで)", dir, maxSkillsIn>>20)
			}
			total += info.Size()
			count++
			mode := int64(0o644)
			if info.Mode()&0o111 != 0 {
				mode = 0o755
			}
			if err := tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeReg,
				Name:     filepath.ToSlash(rel),
				Mode:     mode,
				Size:     info.Size(),
				ModTime:  info.ModTime(),
			}); err != nil {
				return err
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.CopyN(tw, f, info.Size())
			return err
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	if err := tw.Close(); err != nil {
		return nil, 0, err
	}
	if count == 0 {
		return nil, 0, nil
	}
	return &buf, count, nil
}

// linkSkillsScript は VM 内で展開された ~/.quagent/skills の各スキルを
// 各エージェント (opencode, claude, agy, codex) の探索パスへシンボリックリンクするシェルスクリプト。
func linkSkillsScript() string {
	return `mkdir -p ~/.config/opencode/skills ~/.claude/skills ~/.gemini/config/skills ~/.gemini/antigravity-cli/skills ~/.codex/skills
for d in ~/.quagent/skills/*; do
  [ -d "$d" ] || continue
  name=$(basename "$d")
  ln -sfn "$d" ~/.config/opencode/skills/"$name"
  ln -sfn "$d" ~/.claude/skills/"$name"
  ln -sfn "$d" ~/.gemini/config/skills/"$name"
  ln -sfn "$d" ~/.gemini/antigravity-cli/skills/"$name"
  ln -sfn "$d" ~/.codex/skills/"$name"
done`
}

// copySkills は host の共通スキルディレクトリから VM の ~/.quagent/skills へ
// スキル群を安全に転送し、各エージェントのスキル探索パスへシンボリックリンクを展開する。
func copySkills(g vmGuest, dir string) error {
	buf, count, err := archiveSkills(dir)
	if err != nil {
		return err
	}
	if count == 0 {
		return nil
	}

	var out bytes.Buffer
	if err := g.stream("mkdir -p ~/.quagent/skills && tar -x -C ~/.quagent/skills --no-same-owner --no-same-permissions", buf, &out, &out); err != nil {
		return fmt.Errorf("共通スキルを VM へ展開できない: %v: %s", err, out.String())
	}

	if outBytes, err := g.sh(linkSkillsScript(), nil); err != nil {
		return fmt.Errorf("共通スキルのシンボリックリンク作成に失敗: %v: %s", err, outBytes)
	}

	logf("共通スキルを VM へ配置した (%d 件, %s)", count, dir)
	return nil
}
