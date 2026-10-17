package guest

import "strings"

// TerminalColors は端末が OSC 10/11/12 (前景・背景・カーソル色の問い合わせ) に
// 返す色。TUI はこれを使って自分の配色を端末の明暗に合わせる。値は xterm.js と同じ
// "rgb:RRRR/GGGG/BBBB" (各チャンネル 16 bit) で表す。
type TerminalColors struct {
	Foreground string
	Background string
	Cursor     string
}

// quagent hub のブラウザ UI (internal/hub/assets/app.css の :root[data-theme]) と
// 同じ配色。ライトは白磁色の Shirasagi 配色。
var (
	colorsLight = &TerminalColors{
		Foreground: "rgb:2424/3a3a/5858", // #243a58
		Background: "rgb:f9f9/f8f8/f5f5", // #f9f8f5
		Cursor:     "rgb:9d9d/3e3e/6868", // #9d3e68
	}
	colorsDark = &TerminalColors{
		Foreground: "rgb:e6e6/e6e6/e6e6", // #e6e6e6
		Background: "rgb:1111/1313/1a1a", // #11131a
		Cursor:     "rgb:e6e6/e6e6/e6e6",
	}
)

// TerminalColorsForScheme は "light" / "dark" の配色を返す。どちらでもなければ nil
// を返し、色問い合わせは素通しする (端末や tmux クライアントに任せる)。
func TerminalColorsForScheme(scheme string) *TerminalColors {
	switch scheme {
	case "light":
		return colorsLight
	case "dark":
		return colorsDark
	}
	return nil
}

// replyColorQuery は OSC の本文 (例 "11;?") が色の問い合わせなら応答バイト列を返す。
// 問い合わせでなければ ok=false を返し、呼び出し側は素通しする。
func (c *TerminalColors) replyColorQuery(body string) (reply []byte, ok bool) {
	parts := strings.Split(body, ";")
	for i := 0; i+1 < len(parts); i += 2 {
		code, val := parts[i], parts[i+1]
		if val != "?" {
			return nil, false // 色の設定 (問い合わせ以外) は素通しする
		}
		var color string
		switch code {
		case "10":
			color = c.Foreground
		case "11":
			color = c.Background
		case "12":
			color = c.Cursor
		default:
			return nil, false
		}
		reply = append(reply, []byte("\x1b]"+code+";"+color+"\x1b\\")...)
	}
	if len(reply) == 0 {
		return nil, false
	}
	return reply, true
}
