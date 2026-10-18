package guest

import (
	"encoding/base64"
	"io"
	"strings"
)

// OSC 52 (端末経由のクリップボード操作) の扱いの上限。
const (
	// maxOSC は 1 つの OSC シーケンスとして溜める上限。超えたら中身を捨てる
	// (終わらない OSC でメモリを使い潰されないように)。
	maxOSC = 128 << 10
	// MaxClipboard はクリップボードに書き込ませる中身の上限 (デコード後)。
	MaxClipboard = 64 << 10
)

// ClipboardEvent は VM 側が出した OSC 52 の要求。
type ClipboardEvent struct {
	Data     []byte // 書き込みたい中身 (Query / TooLarge / Invalid のときは空)
	Query    bool   // クリップボードの読み出し要求 (常に拒否する)
	TooLarge bool
	Invalid  bool
}

// oscFilter は端末への出力から OSC 52 を抜き取り、onClip に渡す io.Writer。
// それ以外の出力はそのまま (OSC 52 以外の OSC も) out に流す。tmux や host の
// 端末に OSC 52 が届かないので、VM が勝手にクリップボードを書き換えたり読み出したり
// できない。
//
// colors が nil でなければ、端末の色問い合わせ (OSC 10/11/12) にも自分で答える。
// tmux はクライアントの接続状況によって問い合わせの転送先を変える (実端末が付いて
// いればその端末、離れればハブのクライアント) ため、答える主体が揺れて配色が
// 変わってしまう。ここで一元的に答えて、明暗を安定させる。応答は端末ではなく
// inject (エージェントの入力) へ流す。
//
// colors が無い (run が端末の配色を判定できなかった) ときは themeFile を読む。
// hub がブラウザの配色を書き込むので、素通しして tmux の答えに委ねるより安定する。
type oscFilter struct {
	out    io.Writer
	onClip func(ClipboardEvent)
	colors *TerminalColors
	// themeFile は hub がブラウザの配色を書き込むファイル。colors (run が検出した
	// 配色) が無いときだけ読む。これで run 側の検出が失敗しても、答える主体が tmux
	// クライアントの接続状況で揺れない (デタッチで黒く転ぶのを防ぐ)。
	themeFile string
	inject    func([]byte)

	state    int
	osc      []byte
	overflow bool
}

const (
	stNormal = iota
	stEsc    // ESC を見た
	stOSC    // OSC の中
	stOSCEsc // OSC の中で ESC を見た (ST = ESC \ の途中かもしれない)
)

// StripClipboard は w へ流す出力から OSC 52 を抜き取って捨てる io.Writer を返す。
func StripClipboard(w io.Writer) io.Writer { return newOSCFilter(w, nil) }

func newOSCFilter(out io.Writer, onClip func(ClipboardEvent)) *oscFilter {
	return &oscFilter{out: out, onClip: onClip}
}

func (f *oscFilter) Write(p []byte) (int, error) {
	var pass []byte
	for _, b := range p {
		switch f.state {
		case stNormal:
			if b == 0x1b {
				f.state = stEsc
			} else {
				pass = append(pass, b)
			}
		case stEsc:
			switch b {
			case ']':
				f.state, f.osc, f.overflow = stOSC, f.osc[:0], false
			case 0x1b:
				pass = append(pass, 0x1b)
			default:
				pass = append(pass, 0x1b, b)
				f.state = stNormal
			}
		case stOSC:
			switch b {
			case 0x07: // BEL で終わり
				pass = f.finish(pass, []byte{0x07})
			case 0x1b:
				f.state = stOSCEsc
			default:
				f.add(b)
			}
		case stOSCEsc:
			if b == '\\' { // ST で終わり
				pass = f.finish(pass, []byte{0x1b, '\\'})
			} else {
				f.add(0x1b)
				f.add(b)
				f.state = stOSC
			}
		}
	}
	if len(pass) > 0 {
		if _, err := f.out.Write(pass); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// colorScheme は色問い合わせに答える配色を返す。run が検出した配色 (colors) を優先し、
// 無ければテーマファイル (hub が書くブラウザの配色) を使う。どちらも無ければ nil。
func (f *oscFilter) colorScheme() *TerminalColors {
	if f.colors != nil {
		return f.colors
	}
	return TerminalColorsForScheme(ThemeFileScheme(f.themeFile))
}

func (f *oscFilter) add(b byte) {
	if len(f.osc) >= maxOSC {
		f.overflow = true
		return
	}
	f.osc = append(f.osc, b)
}

// finish は OSC を閉じる。OSC 52 なら抜き取り、それ以外はそのまま流す。
func (f *oscFilter) finish(pass []byte, term []byte) []byte {
	f.state = stNormal
	body := string(f.osc)
	if strings.HasPrefix(body, "52;") {
		f.clipboard(body)
		return pass
	}
	if colors := f.colorScheme(); colors != nil {
		if reply, ok := colors.replyColorQuery(body); ok {
			if f.inject != nil {
				f.inject(reply)
			}
			return pass // 色問い合わせは端末へ流さず、ここで答える
		}
	}
	if f.overflow {
		return pass // 途中を捨てたものは流さない
	}
	pass = append(pass, 0x1b, ']')
	pass = append(pass, f.osc...)
	return append(pass, term...)
}

func (f *oscFilter) clipboard(body string) {
	if f.onClip == nil {
		return
	}
	if f.overflow {
		f.onClip(ClipboardEvent{TooLarge: true})
		return
	}
	// 52;<選択>;<base64 か ?>
	parts := strings.SplitN(body, ";", 3)
	if len(parts) != 3 {
		f.onClip(ClipboardEvent{Invalid: true})
		return
	}
	if parts[2] == "?" {
		f.onClip(ClipboardEvent{Query: true})
		return
	}
	data, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		f.onClip(ClipboardEvent{Invalid: true})
		return
	}
	if len(data) > MaxClipboard {
		f.onClip(ClipboardEvent{TooLarge: true})
		return
	}
	f.onClip(ClipboardEvent{Data: data})
}
