package headerpolicy

import (
	"mime"
	"net/http"
	"strconv"
	"strings"
)

// headerValidators は標準で許可するヘッダごとの構文検証関数。
var headerValidators = map[string]func(string) bool{
	"Accept":            validateAccept,
	"Accept-Encoding":   validateAcceptEncoding,
	"Content-Length":    validateContentLength,
	"Content-Type":      validateContentType,
	"Range":             validateRange,
	"If-Modified-Since": validateIfModifiedSince,
}

// validateHeaderValues は canonicalKey に対する全値が妥当か検証する。
func validateHeaderValues(canonicalKey string, values []string) bool {
	if len(values) == 0 {
		return false
	}
	vfn := headerValidators[canonicalKey]
	for _, v := range values {
		if !validateGenericHeaderValue(v) {
			return false
		}
		if vfn != nil && !vfn(v) {
			return false
		}
	}
	return true
}

// validateGenericHeaderValue は任意のヘッダ値に対する基本的な安全性を確認する。
// 制御文字 (CR/LF などによる Header Splitting / Smuggling) を防ぎ、長さを抑える。
func validateGenericHeaderValue(v string) bool {
	if len(v) > 4096 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if (c < 0x20 || c > 0x7E) && c != 0x09 {
			return false
		}
	}
	return true
}

// validateIfModifiedSince は RFC 1123 / RFC 850 / ANSI C の標準 HTTP-date 形式を検証する。
func validateIfModifiedSince(v string) bool {
	v = strings.TrimSpace(v)
	if len(v) == 0 || len(v) > 64 {
		return false
	}
	_, err := http.ParseTime(v)
	return err == nil
}

// validateContentLength は非負の十進整数 (0〜uint64 最大) のみを許す (空白不可)。
func validateContentLength(v string) bool {
	if len(v) == 0 || len(v) > 20 {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return false
		}
	}
	return true
}

// validateRange は bytes=start-end (複数範囲可) の形式を検証する。
func validateRange(v string) bool {
	v = strings.TrimSpace(v)
	if len(v) == 0 || len(v) > 256 {
		return false
	}
	if !strings.HasPrefix(v, "bytes=") {
		return false
	}
	ranges := strings.Split(v[len("bytes="):], ",")
	if len(ranges) == 0 {
		return false
	}
	for _, r := range ranges {
		r = strings.TrimSpace(r)
		dash := strings.IndexByte(r, '-')
		if dash == -1 {
			return false
		}
		start := r[:dash]
		end := r[dash+1:]
		if start == "" && end == "" {
			return false
		}
		if start != "" && !isDigits(start, 20) {
			return false
		}
		if end != "" && !isDigits(end, 20) {
			return false
		}
	}
	return true
}

func isDigits(s string, maxLen int) bool {
	if len(s) == 0 || len(s) > maxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

var allowedEncodings = map[string]bool{
	"gzip":     true,
	"deflate":  true,
	"br":       true,
	"brotli":   true,
	"zstd":     true,
	"compress": true,
	"identity": true,
	"*":        true,
}

// validateAcceptEncoding は既知のアルゴリズム名と品質値 (q=) のみを許す。
func validateAcceptEncoding(v string) bool {
	v = strings.TrimSpace(v)
	if len(v) == 0 || len(v) > 256 {
		return false
	}
	tokens := strings.Split(v, ",")
	if len(tokens) == 0 {
		return false
	}
	for _, tok := range tokens {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			return false
		}
		parts := strings.Split(tok, ";")
		coding := strings.ToLower(strings.TrimSpace(parts[0]))
		if !allowedEncodings[coding] {
			return false
		}
		if len(parts) > 2 {
			return false
		}
		if len(parts) == 2 {
			qPart := strings.TrimSpace(parts[1])
			if !validateQValue(qPart) {
				return false
			}
		}
	}
	return true
}

func validateQValue(s string) bool {
	if !strings.HasPrefix(s, "q=") {
		return false
	}
	val := s[2:]
	if len(val) == 0 || len(val) > 5 {
		return false
	}
	f, err := strconv.ParseFloat(val, 64)
	return err == nil && f >= 0.0 && f <= 1.0
}

// validateContentType は MIME メディアタイプ (type/subtype) と安全なパラメータ (charset, boundary) を検証する。
func validateContentType(v string) bool {
	v = strings.TrimSpace(v)
	if len(v) == 0 || len(v) > 256 {
		return false
	}
	mediatype, params, err := mime.ParseMediaType(v)
	if err != nil {
		return false
	}
	if !validateMediaType(mediatype) {
		return false
	}
	for key, val := range params {
		switch strings.ToLower(key) {
		case "charset":
			if !validateToken(val, 32) {
				return false
			}
		case "boundary":
			if !validateBoundary(val) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// validateAccept はカンマ区切りのメディアレンジと安全なパラメータ (q, charset, v, version, level) を検証する。
func validateAccept(v string) bool {
	v = strings.TrimSpace(v)
	if len(v) == 0 || len(v) > 512 {
		return false
	}
	parts := strings.Split(v, ",")
	if len(parts) == 0 {
		return false
	}
	for _, item := range parts {
		item = strings.TrimSpace(item)
		if item == "" {
			return false
		}
		mt, params, err := mime.ParseMediaType(item)
		if err != nil {
			return false
		}
		if !validateMediaType(mt) {
			return false
		}
		for key, val := range params {
			switch strings.ToLower(key) {
			case "q":
				if !validateQValue("q=" + val) {
					return false
				}
			case "charset", "v", "version", "level":
				if !validateToken(val, 32) {
					return false
				}
			default:
				return false
			}
		}
	}
	return true
}

func validateMediaType(mt string) bool {
	parts := strings.Split(mt, "/")
	if len(parts) != 2 {
		return false
	}
	return validateMimeToken(parts[0]) && validateMimeToken(parts[1])
}

func validateMimeToken(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '+' || c == '.' || c == '_' || c == '*' {
			continue
		}
		return false
	}
	return true
}

func validateToken(s string, maxLen int) bool {
	if len(s) == 0 || len(s) > maxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}

func validateBoundary(s string) bool {
	if len(s) == 0 || len(s) > 70 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '+' || c == '=' || c == ':' {
			continue
		}
		return false
	}
	return true
}
