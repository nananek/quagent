package contentguard

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestDLPScannerPatterns(t *testing.T) {
	scanner := NewDLPScanner()

	tests := []struct {
		name        string
		input       string
		wantMatch   bool
		wantPattern string
	}{
		{
			name:        "Private Key RSA",
			input:       "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA0...",
			wantMatch:   true,
			wantPattern: "Private Key",
		},
		{
			name:        "Private Key Generic",
			input:       "-----BEGIN PRIVATE KEY-----\nMIIEvgIBADANBgkqhkiG9w0BAQEFAASC...",
			wantMatch:   true,
			wantPattern: "Private Key",
		},
		{
			name:        "AWS Access Key AKIA",
			input:       "aws_access_key_id = " + "AK" + "IAIOSFODNN7EXAMPLE",
			wantMatch:   true,
			wantPattern: "AWS Access Key",
		},
		{
			name:        "AWS Access Key ASIA",
			input:       "AS" + "IAIOSFODNN7EXAMPLE",
			wantMatch:   true,
			wantPattern: "AWS Access Key",
		},
		{
			name:        "GitHub Classic Token",
			input:       "gh" + "p_1234567890abcdefghijklmnopqrstuvwxyzAB",
			wantMatch:   true,
			wantPattern: "GitHub Token",
		},
		{
			name:        "GitHub Fine-grained PAT",
			input:       "github_pat" + "_11AAAAAAA01234567890abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890ab",
			wantMatch:   true,
			wantPattern: "GitHub Token",
		},
		{
			name:        "Google API Key",
			input:       "AI" + "zaSyD-1234567890abcdefghijklmnopqrstU",
			wantMatch:   true,
			wantPattern: "Google API Key",
		},
		{
			name:        "Slack Bot Token",
			input:       "xo" + "xb-123456789012-1234567890123-456789abcdef1234567890ab",
			wantMatch:   true,
			wantPattern: "Slack Token",
		},
		{
			name:        "OpenAI API Key",
			input:       "s" + "k-proj-1234567890abcdefghijklmnopqrstuvwxyz",
			wantMatch:   true,
			wantPattern: "OpenAI/LLM API Key",
		},
		{
			name:        "JWT Token",
			input:       "ey" + "JhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4ifQ.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c",
			wantMatch:   true,
			wantPattern: "JSON Web Token",
		},
		{
			name:      "Clean normal text",
			input:     "Hello, this is a normal request payload with no secret credentials.",
			wantMatch: false,
		},
		{
			name:      "Normal JSON data",
			input:     `{"status": "ok", "count": 42, "user": "alice", "items": ["a", "b"]}`,
			wantMatch: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			matched, pattern, preview := scanner.InspectText(tc.input)
			if matched != tc.wantMatch {
				t.Fatalf("InspectText(%q) matched=%v, want %v", tc.input, matched, tc.wantMatch)
			}
			if tc.wantMatch {
				if pattern != tc.wantPattern {
					t.Errorf("pattern = %q, want %q", pattern, tc.wantPattern)
				}
				if preview == "" {
					t.Errorf("preview is empty for matched pattern")
				}
			}
		})
	}
}

func TestDLPInspectRequest(t *testing.T) {
	scanner := NewDLPScanner()

	// 1. URL に秘密情報が含まれる場合
	req1, _ := http.NewRequest("GET", "https://api.example.com/search?key="+"AI"+"zaSyD-1234567890abcdefghijklmnopqrstU", nil)
	detected, name, preview, err := scanner.InspectRequest(req1)
	if err != nil {
		t.Fatal(err)
	}
	if !detected || !strings.Contains(name, "Google API Key") {
		t.Fatalf("URL inspection failed: detected=%v, name=%s, preview=%s", detected, name, preview)
	}

	// 2. ヘッダに秘密情報が含まれる場合
	req2, _ := http.NewRequest("GET", "https://api.example.com/data", nil)
	req2.Header.Set("X-Custom-Secret", "gh"+"p_1234567890abcdefghijklmnopqrstuvwxyzAB")
	detected, name, preview, err = scanner.InspectRequest(req2)
	if err != nil {
		t.Fatal(err)
	}
	if !detected || !strings.Contains(name, "GitHub Token") {
		t.Fatalf("Header inspection failed: detected=%v, name=%s, preview=%s", detected, name, preview)
	}

	// 3. リクエストボディに秘密情報が含まれる場合とボディ復元
	bodyContent := `{"token": "` + "s" + `k-proj-1234567890abcdefghijklmnopqrstuvwxyz"}`
	req3, _ := http.NewRequest("POST", "https://api.example.com/upload", strings.NewReader(bodyContent))
	detected, name, preview, err = scanner.InspectRequest(req3)
	if err != nil {
		t.Fatal(err)
	}
	if !detected || !strings.Contains(name, "OpenAI/LLM API Key") {
		t.Fatalf("Body inspection failed: detected=%v, name=%s, preview=%s", detected, name, preview)
	}

	// ボディが消費されず再読み込み可能であることを検証
	reRead, err := io.ReadAll(req3.Body)
	if err != nil {
		t.Fatalf("Failed to re-read body: %v", err)
	}
	if string(reRead) != bodyContent {
		t.Fatalf("Re-read body = %q, want %q", string(reRead), bodyContent)
	}

	// 4. 正常なリクエスト
	req4, _ := http.NewRequest("POST", "https://api.example.com/clean", bytes.NewReader([]byte(`{"hello":"world"}`)))
	req4.Header.Set("Content-Type", "application/json")
	detected, _, _, err = scanner.InspectRequest(req4)
	if err != nil {
		t.Fatal(err)
	}
	if detected {
		t.Fatalf("Clean request was falsely detected as DLP violation")
	}

	// 5. req == nil
	detected, _, _, err = scanner.InspectRequest(nil)
	if err != nil || detected {
		t.Fatalf("InspectRequest(nil) failed: %v, %v", detected, err)
	}
}

func TestMaskValue(t *testing.T) {
	if got := MaskValue("short", 10); got != "*****" {
		t.Errorf("MaskValue short = %q, want '*****'", got)
	}
	if got := MaskValue("AIzaSyD-1234567890", 6); got != "AIzaSy****" {
		t.Errorf("MaskValue = %q, want 'AIzaSy****'", got)
	}
}
