package hub

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// tmuxBinary は control mode に使う tmux。テストで差し替える。
var tmuxBinary = "tmux"

// controlClient は tmux control mode (tmux -C) でエージェントのペインを中継する。
//
//	%output %<pane> <data>        出力 (data は非可印バイトと \ を \ooo でエスケープ)
//	%begin <ts> <num> <flags> … %end   コマンドの返答
//	%exit                         tmux 側が終了
//
// 入力は send-keys -t <pane> -H <hex...> で生バイトのまま送り、リサイズは
// refresh-client -C で伝える。
type controlClient struct {
	session string
	pane    string

	cmd   *exec.Cmd
	stdin io.WriteCloser

	sink func([]byte)

	cmdMu   sync.Mutex // コマンドは 1 つずつ流す
	replies chan []string

	closeOnce sync.Once
	done      chan struct{}
	waitErr   error
}

// errControlClosed は control client が終了しているときのエラー。
var errControlClosed = errors.New("tmux の control client が終了している")

// startControl は session に control mode で接続し、エージェントのペイン (index 0) を
// 特定する。sink はペインの出力を順に受け取る (nil なら捨てる)。
func startControl(session string, sink func([]byte)) (*controlClient, error) {
	c := &controlClient{
		session: session,
		sink:    sink,
		replies: make(chan []string, 16),
		done:    make(chan struct{}),
	}
	cmd := exec.Command(tmuxBinary, "-C", "attach-session", "-t", session)
	// control mode は端末 (tty) を必要としない。stdout/stderr はパイプで読む。
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("tmux を起動できない: %w", err)
	}
	c.cmd, c.stdin = cmd, in
	go c.read(out)
	go func() {
		c.waitErr = cmd.Wait()
		close(c.done)
	}()

	if err := c.resolvePane(); err != nil {
		c.Close()
		return nil, err
	}
	// control client が最後にサイズを変えたクライアントになっても、共有ウィンドウを
	// 縮めないようにする (スマホ接続でデスクトップの表示が縮むのを防ぐ)。
	_ = c.fire(fmt.Sprintf("set-option -t %s window-size largest", quote(c.session)))
	return c, nil
}

// resolvePane はペイン index 0 (エージェント) の pane id を調べる。
func (c *controlClient) resolvePane() error {
	lines, err := c.run(fmt.Sprintf("list-panes -t %s -F '#{pane_index} #{pane_id}'", quote(c.session)))
	if err != nil {
		return err
	}
	for _, ln := range lines {
		f := strings.Fields(ln)
		if len(f) == 2 && f[0] == "0" {
			c.pane = f[1]
			return nil
		}
	}
	return fmt.Errorf("エージェントのペイン (index 0) が見つからない")
}

// Pane はエージェントの pane id (例 "%0") を返す。
func (c *controlClient) Pane() string { return c.pane }

// Done は tmux 側が終了したときに閉じる。
func (c *controlClient) Done() <-chan struct{} { return c.done }

// read は control mode の行を読み、出力とコマンドの返答を振り分ける。
func (c *controlClient) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	var block []string
	inBlock := false
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "%output "):
			pane, data, ok := parseOutput(line)
			if ok && pane == c.pane && c.sink != nil && len(data) > 0 {
				c.sink(data)
			}
		case strings.HasPrefix(line, "%begin "):
			inBlock, block = true, block[:0]
		case strings.HasPrefix(line, "%end "), strings.HasPrefix(line, "%error"):
			if inBlock {
				c.pushReply(block)
				inBlock = false
			}
		case strings.HasPrefix(line, "%exit"):
			return
		default:
			if inBlock {
				block = append(block, line)
			}
			// それ以外の %通知 (layout-change など) は使わない。
		}
	}
}

// pushReply は返答ブロックを送る。受信側が詰まっていれば捨てる (出力の処理を止めない)。
func (c *controlClient) pushReply(block []string) {
	cp := append([]string(nil), block...)
	select {
	case c.replies <- cp:
	default:
	}
}

// run は cmd を実行し、その返答行を返す。標識 (marker) を付けて他の返答と区別する。
func (c *controlClient) run(cmd string) ([]string, error) {
	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()
	select {
	case <-c.done:
		return nil, errControlClosed
	default:
	}
	// 前のコマンドの残りの返答を捨てる。
	drain(c.replies)
	marker := "quagent-reply-" + randomHex(8)
	full := cmd + " ; display-message -p '" + marker + "'"
	if _, err := io.WriteString(c.stdin, full+"\n"); err != nil {
		return nil, err
	}
	var out []string
	t := time.NewTimer(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return nil, errControlClosed
		case <-t.C:
			return nil, fmt.Errorf("tmux の応答が無い: %s", cmd)
		case block := <-c.replies:
			if containsLine(block, marker) {
				return out, nil
			}
			out = append(out, block...)
		}
	}
}

// fire は返答を待たないコマンドを送る。
func (c *controlClient) fire(cmd string) error {
	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()
	select {
	case <-c.done:
		return errControlClosed
	default:
	}
	_, err := io.WriteString(c.stdin, cmd+"\n")
	return err
}

// maxKeysPerSend は 1 回の send-keys で送るバイト数の上限。
const maxKeysPerSend = 512

// pasteThreshold は send-keys ではなく tmux のバッファ経由 (Paste) に切り替える
// 入力の大きさ。キーストローク (数バイト) は send-keys のままにする。
const pasteThreshold = 256

// bracketedPasteStart は xterm.js が貼り付けに付けるブラケットペーストの開始
// マーカー (ESC [ 2 0 0 ~)。
const bracketedPasteStart = "\x1b[200~"

// isPaste はブラウザからの入力を貼り付けとみなすかを返す。xterm.js は貼り付けを
// 1 回の onData で送るので、大きさかブラケットペーストのマーカーで判別できる。
func isPaste(data []byte) bool {
	return len(data) >= pasteThreshold || bytes.Contains(data, []byte(bracketedPasteStart))
}

// Send はブラウザからの入力を生バイトのままペインへ送る。キーストローク向け。
func (c *controlClient) Send(data []byte) error {
	for off := 0; off < len(data); off += maxKeysPerSend {
		chunk := data[off:min(off+maxKeysPerSend, len(data))]
		hexes := make([]string, 0, len(chunk))
		for _, b := range chunk {
			hexes = append(hexes, hex.EncodeToString([]byte{b}))
		}
		if err := c.fire(fmt.Sprintf("send-keys -t %s -H %s", c.pane, strings.Join(hexes, " "))); err != nil {
			return err
		}
	}
	return nil
}

// Paste はまとまった入力を tmux のバッファ経由でペインへ一括して送る。
//
// send-keys は 512 バイトごとに 1 コマンドを流し、その各バイトをキー入力として
// 1 つずつ注入するため、長い貼り付けほど遅くなる (実測で paste-buffer の 2〜5 倍)。
// paste-buffer はバッファの中身をまとめてペインへ書くため、貼り付けをそのまま速く
// 流せる。ブラウザが付けたブラケットペーストのマーカーはそのまま通し、tmux 側では
// 付け直さない (-p なし)。
func (c *controlClient) Paste(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	// tmux はファイルからしかバッファへ読み込めない (control mode の stdin は
	// コマンド列に使う)。0600 の一時ファイルに書いて load-buffer に読ませる。
	f, err := os.CreateTemp("", "quagent-paste-*")
	if err != nil {
		return err
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	name := "quagent-paste-" + randomHex(8)
	// load-buffer / paste-buffer は成功時に何も出力しない。返答が空でなければ
	// tmux のエラーなので、その内容をエラーとして返す。
	if out, err := c.run(fmt.Sprintf("load-buffer -b %s %s", quote(name), quote(path))); err != nil {
		return err
	} else if len(out) > 0 {
		return fmt.Errorf("load-buffer に失敗した: %s", strings.Join(out, "; "))
	}
	// -r で改行をそのまま渡し、-d で使い終わったバッファを消す。
	out, err := c.run(fmt.Sprintf("paste-buffer -d -r -b %s -t %s", quote(name), quote(c.pane)))
	if err != nil || len(out) > 0 {
		// 失敗するとバッファが残るので消しておく。
		_, _ = c.run(fmt.Sprintf("delete-buffer -b %s", quote(name)))
	}
	if err != nil {
		return err
	}
	if len(out) > 0 {
		return fmt.Errorf("paste-buffer に失敗した: %s", strings.Join(out, "; "))
	}
	return nil
}

// Resize は control クライアントのサイズを伝える (バリデーション付き)。
func (c *controlClient) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	if cols > maxTermSize {
		cols = maxTermSize
	}
	if rows > maxTermSize {
		rows = maxTermSize
	}
	return c.fire(fmt.Sprintf("refresh-client -C %dx%d", cols, rows))
}

// Capture はペインの現在の画面を (色付きで) 取り出す。ブラウザが繋いだ直後の
// 初期表示に使う。
func (c *controlClient) Capture() ([]byte, error) {
	lines, err := c.run(fmt.Sprintf("capture-pane -p -e -t %s", c.pane))
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, nil
	}
	return []byte(strings.Join(lines, "\r\n") + "\r\n"), nil
}

// Close は control client を終了する。
func (c *controlClient) Close() {
	c.closeOnce.Do(func() {
		if c.stdin != nil {
			_ = c.stdin.Close()
		}
		select {
		case <-c.done:
		case <-time.After(2 * time.Second):
			if c.cmd != nil && c.cmd.Process != nil {
				_ = c.cmd.Process.Kill()
			}
		}
	})
}

// parseOutput は "%output %<pane-id> <data>" を分解し、data のエスケープを戻す。
func parseOutput(line string) (pane string, data []byte, ok bool) {
	rest, found := strings.CutPrefix(line, "%output ")
	if !found {
		return "", nil, false
	}
	i := strings.IndexByte(rest, ' ')
	if i < 0 {
		return "", nil, false
	}
	return rest[:i], decodeControl([]byte(rest[i+1:])), true
}

// decodeControl は tmux control mode のエスケープ (\ooo の 3 桁八進) を戻す。
// UTF-8 などの高位バイトはエスケープされないため、そのまま通す。
func decodeControl(s []byte) []byte {
	if bytes.IndexByte(s, '\\') < 0 {
		return s
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			a, b, c := s[i+1], s[i+2], s[i+3]
			if isOctal(a) && isOctal(b) && isOctal(c) {
				out = append(out, (a-'0')<<6|(b-'0')<<3|(c-'0'))
				i += 3
				continue
			}
		}
		out = append(out, s[i])
	}
	return out
}

func isOctal(b byte) bool { return b >= '0' && b <= '7' }

func containsLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

func drain[T any](ch chan T) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// quote は tmux のコマンド引数として安全なよう単一引用符で囲む。
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
