package contentguard

import (
	"errors"
	"io"
)

// streamOverlap はチャンク境界をまたぐ秘密を見逃さないために持ち越す長さ。
// 既定パターンは最長でも 255 文字程度なので、これだけあれば十分。
const streamOverlap = 4096

// errBodyViolation は本文の走査で秘密を見つけ、転送を中断するときのエラー。
var errBodyViolation = errors.New("contentguard: 本文から機密情報を検知した")

// BodyViolation は転送中に検知した違反を報告できる本文。DLP は検査上限を超える
// 本文を「転送しながら」走査するため、検知の有無はこの口で確かめる。
type BodyViolation interface {
	Violation() (pattern, preview string, ok bool)
}

// streamScanner は本文をチャンクごとに走査する。直前の streamOverlap バイトを
// 持ち越して、境界をまたぐ秘密も見逃さない。
type streamScanner struct {
	patterns []Pattern
	tail     []byte
	pattern  string
	preview  string
	found    bool
}

// scan はチャンクを走査し、見つかれば found を立てて true を返す。
func (s *streamScanner) scan(p []byte) bool {
	if s.found {
		return true
	}
	// 容量を len+len で見積もると、値が大きいときにオーバーフローしうる。
	// 伸長は append に任せ、make には単一の長さだけ渡す。
	buf := make([]byte, 0, len(s.tail))
	buf = append(buf, s.tail...)
	buf = append(buf, p...)
	for _, pat := range s.patterns {
		if loc := pat.Regex.FindString(string(buf)); loc != "" {
			s.pattern, s.preview, s.found = pat.Name, MaskValue(loc, pat.MaskLen), true
			break
		}
	}
	if len(buf) > streamOverlap {
		s.tail = append([]byte(nil), buf[len(buf)-streamOverlap:]...)
	} else {
		s.tail = append([]byte(nil), buf...)
	}
	return s.found
}

// scanBody は本文を転送しながら走査する ReadCloser。違反を見つけると Read が
// エラーを返し、転送 (req.Write / RoundTrip) を中断させる。読んだ本文は
// そのまま返すので、クリーンな本文は切り詰められない。
type scanBody struct {
	rc      io.ReadCloser
	scanner streamScanner
}

func (b *scanBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 && b.scanner.scan(p[:n]) {
		return 0, errBodyViolation
	}
	return n, err
}

func (b *scanBody) Close() error { return b.rc.Close() }

func (b *scanBody) Violation() (string, string, bool) {
	if b.scanner.found {
		return b.scanner.pattern, b.scanner.preview, true
	}
	return "", "", false
}

// guardBody は rc を、転送しながら走査する本文で包む。
func guardBody(rc io.ReadCloser, patterns []Pattern) io.ReadCloser {
	return &scanBody{rc: rc, scanner: streamScanner{patterns: patterns}}
}
