// Package paths はホスト側の保存先 (XDG) を決める。
package paths

import (
	"os"
	"path/filepath"
)

func xdg(env, fallback string) string {
	if v := os.Getenv(env); v != "" {
		return filepath.Join(v, "quagent")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, fallback, "quagent")
}

// DataDir はベースイメージなど長期保持するものの置き場。
func DataDir() string { return xdg("XDG_DATA_HOME", ".local/share") }

// CacheDir はダウンロードしたクラウドイメージの置き場。
func CacheDir() string { return xdg("XDG_CACHE_HOME", ".cache") }

// StateDir は実行中 VM の作業ディレクトリの置き場 (終了時に消す)。
func StateDir() string { return xdg("XDG_STATE_HOME", ".local/state") }

// ImagesDir は焼いたベースイメージの置き場。
func ImagesDir() string { return filepath.Join(DataDir(), "images") }

// RunsDir は run ごとの作業ディレクトリの親。
func RunsDir() string { return filepath.Join(StateDir(), "runs") }

// LogsDir は run 終了後も残す host 側ログの置き場。
func LogsDir() string { return filepath.Join(StateDir(), "logs") }

// ConfigFile はユーザー設定ファイル (~/.config/quagent/config.json)。
func ConfigFile() string { return filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), "config.json") }
