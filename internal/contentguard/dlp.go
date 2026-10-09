// Package contentguard はデータ持ち出し防止 (DLP: Data Loss Prevention) およびコンテンツ検査を行う。
//
// 許可ドメイン宛ての通信であっても、リクエストヘッダ、URL クエリ、リクエストボディに
// 含まれる秘密鍵、API キー、トークンなどの機密情報漏洩を検知・阻止する。
package contentguard

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// Pattern は検査対象の機密情報パターン。
type Pattern struct {
	Name    string
	Regex   *regexp.Regexp
	MaskLen int
}

var defaultPatterns = []Pattern{
	{
		Name:    "Private Key",
		Regex:   regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |DSA )?PRIVATE KEY-----`),
		MaskLen: 20,
	},
	{
		Name:    "AWS Access Key",
		Regex:   regexp.MustCompile(`\b(?:A3T[A-Z0-9]|AKIA|AGPA|AIDA|AROA|AIPA|ANPA|ANVA|ASIA)[A-Z0-9]{16}\b`),
		MaskLen: 8,
	},
	{
		Name:    "GitHub Token",
		Regex:   regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9_]{36,255}\b|\bgithub_pat_[A-Za-z0-9_]{22,255}\b`),
		MaskLen: 15,
	},
	{
		Name:    "Google API Key",
		Regex:   regexp.MustCompile(`\bAIza[0-9A-Za-z-_]{35}\b`),
		MaskLen: 8,
	},
	{
		Name:    "Slack Token",
		Regex:   regexp.MustCompile(`\bxox[baprs]-[0-9A-Za-z]{10,48}\b`),
		MaskLen: 9,
	},
	{
		Name:    "OpenAI/LLM API Key",
		Regex:   regexp.MustCompile(`\bsk-[a-zA-Z0-9_-]{20,}\b`),
		MaskLen: 6,
	},
	{
		Name:    "JSON Web Token",
		Regex:   regexp.MustCompile(`\beyJ[a-zA-Z0-9_-]{10,}\.eyJ[a-zA-Z0-9_-]{10,}\.[a-zA-Z0-9_-]{10,}\b`),
		MaskLen: 12,
	},
}

// MaxInspectBodySize はリクエストボディを検査する上限サイズ (1 MiB)。
const MaxInspectBodySize = 1 << 20

// DLPScanner は機密情報パターンの検査器。
type DLPScanner struct {
	patterns []Pattern
}

// NewDLPScanner は既定の機密情報パターンを持つ DLPScanner を返す。
func NewDLPScanner() *DLPScanner {
	return &DLPScanner{patterns: defaultPatterns}
}

// MaskValue は検出された機密情報のプレビューを安全にマスクする。
func MaskValue(val string, visiblePrefix int) string {
	runes := []rune(val)
	if len(runes) <= visiblePrefix {
		return strings.Repeat("*", len(runes))
	}
	return string(runes[:visiblePrefix]) + "****"
}

// InspectText は文字列 s から機密情報パターンを検出する。
func (s *DLPScanner) InspectText(text string) (detected bool, patternName string, maskedPreview string) {
	for _, p := range s.patterns {
		if loc := p.Regex.FindString(text); loc != "" {
			return true, p.Name, MaskValue(loc, p.MaskLen)
		}
	}
	return false, "", ""
}

// InspectRequest は HTTP リクエストの URL、ヘッダ、ボディを検査する。
// 本文を読み取った場合も req.Body を復元するため、後続のプロキシ転送には影響しない。
func (s *DLPScanner) InspectRequest(req *http.Request) (detected bool, patternName string, maskedPreview string, err error) {
	if req == nil {
		return false, "", "", nil
	}

	// 1. URL パスとクエリの検査
	if req.URL != nil {
		if matched, name, preview := s.InspectText(req.URL.String()); matched {
			return true, name, preview, nil
		}
	}

	// 2. ヘッダの検査
	for k, vs := range req.Header {
		for _, v := range vs {
			if matched, name, preview := s.InspectText(v); matched {
				return true, fmt.Sprintf("%s in header %s", name, k), preview, nil
			}
		}
	}

	// 3. リクエストボディの検査
	if req.Body != nil {
		bodyBytes, err := io.ReadAll(io.LimitReader(req.Body, MaxInspectBodySize+1))
		if err != nil {
			return false, "", "", err
		}
		// 元の Body を閉じて、再読み込み可能なバッファで差し替える
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		if len(bodyBytes) > 0 {
			if matched, name, preview := s.InspectText(string(bodyBytes)); matched {
				return true, fmt.Sprintf("%s in request body", name), preview, nil
			}
		}
	}

	return false, "", "", nil
}
