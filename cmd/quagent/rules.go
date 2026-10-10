package main

import (
	"fmt"
)

// guestRulesContent は使い捨て VM 上で動く各コーディングエージェントに共通で
// 遵守させる基本制約と行動規範。
const guestRulesContent = `# quagent VM 実行環境の制約と運用ルール

本環境は、外向き通信をホスト側で厳格に遮断した使い捨て QEMU VM 上で動作しています。
エージェントは以下の原則を厳守して作業を行ってください。

## 1. 使い捨てVMと知識・成果物の永続化（メモリーへの依存禁止）
- **完全使い捨て環境**: セッション終了時に VM は破棄されます。成果物として残るのは「Git のプルリクエスト (PR) として提出したコミット」およびホストと同期される .tmp/ 配下のファイルのみです。
- **メモリー機能の利用禁止**: Claude Code の auto-memory (~/.claude.json や CLAUDE.md 内メモリー等) などのセッション記憶に知識を溜め込まないでください。VM 破棄時にすべて消失します。
- **プロジェクト知識の集約**: プロジェクトに必要な知識、ワークフロー、手順は、メモリーではなくリポジトリの skill (skills/<名前>/SKILL.md 等) やリポジトリ内のドキュメントとして作成し、コミット・PR化してください。
- **グローバル規範の提案**: プロジェクトを問わずユーザーが共通して要求する行動規範やコーディング規約は、グローバルスキル (ホスト側 ~/.config/quagent/skills) への追加をユーザーに提案してください。

## 2. コミットの署名（一時 SSH 鍵の必須利用）
- **署名の必須**: コミットは必ず ~/.ssh/quagent-mark の一時 SSH 署名鍵 (Git 設定で commit.gpgsign=true として構成済み) で署名してください。--no-gpg-sign などで署名を省略してはいけません。
- **ホスト側での再署名**: ホスト側の quagent は「この一時鍵で正しく署名されたコミット」のみを検証し、利用者の正規の鍵で再署名して GitHub へ提出します。一時鍵で署名されていないコミットは PR 化されません。

## 3. 常に再現可能な検証手順（マシン固有事情の完全排除）
- **rootless Docker の原則利用**: 動作検証やビルド・テスト環境の構築には、原則として rootless Docker を使用してください。
- **マシン固有事情を作らない**: ゲスト OS に ad-hoc にパッケージを追加したり、手動でマシン固有の設定を行ったりして「この VM 上でしか動かない」状態を一切作らないでください。
- **再現性の担保**: CI や別の使い捨て VM、他の開発者の環境でも全く同じ手順で再現・実行できる検証手順 (Dockerfile、docker compose、スクリプトなど) を常に用いてください。
`

// linkRulesScript は VM 内の ~/.quagent/rules.md を各エージェント
// (opencode, claude, agy, codex) のグローバル指示・ルール探索パスへシンボリックリンクするシェルスクリプト。
func linkRulesScript() string {
	return `mkdir -p ~/.config/opencode ~/.claude ~/.gemini/config/rules ~/.gemini/antigravity-cli/rules ~/.codex
ln -sfn ~/.quagent/rules.md ~/.config/opencode/AGENTS.md
ln -sfn ~/.quagent/rules.md ~/.claude/CLAUDE.md
ln -sfn ~/.quagent/rules.md ~/.gemini/config/GEMINI.md
ln -sfn ~/.quagent/rules.md ~/.gemini/antigravity-cli/GEMINI.md
ln -sfn ~/.quagent/rules.md ~/.gemini/config/rules/quagent.md
ln -sfn ~/.quagent/rules.md ~/.gemini/antigravity-cli/rules/quagent.md
ln -sfn ~/.quagent/rules.md ~/.codex/instructions.md
ln -sfn ~/.quagent/rules.md ~/.codex/CODEX.md`
}

// setupRules は VM 内の ~/.quagent/rules.md に共通ルールを書き込み、
// 各エージェントの探索パスへシンボリックリンクを展開する。
func setupRules(g vmGuest) error {
	if err := g.writeFile("~/.quagent/rules.md", []byte(guestRulesContent)); err != nil {
		return fmt.Errorf("共通ルールを VM へ配置できない: %w", err)
	}
	if outBytes, err := g.sh(linkRulesScript(), nil); err != nil {
		return fmt.Errorf("共通ルールのシンボリックリンク作成に失敗: %v: %s", err, outBytes)
	}
	logf("共通ルールを VM へ配置した")
	return nil
}
