package codex

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMinterCaches(t *testing.T) {
	calls := 0
	m := &Minter{
		mint: func() (string, string, error) {
			calls++
			return "tok", "acc123", nil
		},
	}
	got, err := m.Token()
	if err != nil || got != "tok" {
		t.Fatalf("Token() = %q, %v", got, err)
	}
	if acc := m.AccountID(); acc != "acc123" {
		t.Fatalf("AccountID() = %q, want acc123", acc)
	}
	got, err = m.Token()
	if err != nil || got != "tok" {
		t.Fatalf("Token() = %q, %v", got, err)
	}
	if calls != 1 {
		t.Fatalf("作り直しが %d 回 (使い回していない)", calls)
	}
}

func TestAllowCoversObserved(t *testing.T) {
	for _, p := range []string{
		"/backend-api/codex/responses",
		"/backend-api/conversation",
		"/v1/chat/completions",
		"/responses",
		"/models",
	} {
		found := false
		for _, op := range Allow {
			m, rest, _ := strings.Cut(op, " ")
			if m == "POST" || m == "GET" {
				prefix := strings.TrimSuffix(rest, "*")
				if strings.HasPrefix(p, prefix) {
					found = true
					break
				}
			}
		}
		if !found {
			t.Errorf("Allow に %s が無い", p)
		}
	}
}

func TestExtractAuth(t *testing.T) {
	// 1. 標準的な tokens オブジェクト
	json1 := `{
		"auth_mode": "chatgpt",
		"tokens": {
			"access_token": "acc1",
			"refresh_token": "ref1",
			"account_id": "acc_id_1",
			"client_id": "client_1"
		}
	}`
	info1, err := extractAuth(json1)
	if err != nil {
		t.Fatalf("extractAuth(json1) error: %v", err)
	}
	if info1.AccessToken != "acc1" || info1.RefreshToken != "ref1" || info1.AccountID != "acc_id_1" || info1.ClientID != "client_1" {
		t.Fatalf("unexpected info1: %+v", info1)
	}

	// 2. トップレベルにフィールドがある場合
	json2 := `{
		"access_token": "acc2",
		"refresh_token": "ref2",
		"account_id": "acc_id_2"
	}`
	info2, err := extractAuth(json2)
	if err != nil {
		t.Fatalf("extractAuth(json2) error: %v", err)
	}
	if info2.AccessToken != "acc2" || info2.RefreshToken != "ref2" || info2.AccountID != "acc_id_2" {
		t.Fatalf("unexpected info2: %+v", info2)
	}

	// 3. 素の文字列
	info3, err := extractAuth("plain-token")
	if err != nil {
		t.Fatalf("extractAuth(plain) error: %v", err)
	}
	if info3.AccessToken != "plain-token" {
		t.Fatalf("unexpected info3: %+v", info3)
	}

	// 4. 空のJSON
	_, err = extractAuth(`{"other":"val"}`)
	if err == nil {
		t.Fatal("expected error on empty tokens")
	}
}

func TestParseJWTExpiry(t *testing.T) {
	expTime := time.Now().Add(1 * time.Hour).Truncate(time.Second)
	payload, _ := json.Marshal(map[string]any{"exp": expTime.Unix()})
	b64Payload := base64.RawURLEncoding.EncodeToString(payload)
	fakeJWT := fmt.Sprintf("header.%s.sig", b64Payload)

	got := ParseJWTExpiry(fakeJWT)
	if got.Unix() != expTime.Unix() {
		t.Fatalf("ParseJWTExpiry() = %v, want %v", got, expTime)
	}

	// 不正なトークン
	if gotBad := ParseJWTExpiry("not-jwt"); !gotBad.IsZero() {
		t.Fatalf("expected zero time for invalid jwt, got %v", gotBad)
	}
}

func TestMintWithEndpoint(t *testing.T) {
	srvSuccess := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		_ = r.ParseForm()
		if r.FormValue("grant_type") != "refresh_token" {
			t.Errorf("grant_type = %q", r.FormValue("grant_type"))
		}
		if r.FormValue("client_id") != "test-client" {
			t.Errorf("client_id = %q", r.FormValue("client_id"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"new-token"}`))
	}))
	defer srvSuccess.Close()

	tok, err := mintWithEndpoint(srvSuccess.URL, "test-client", "ref-tok")
	if err != nil || tok != "new-token" {
		t.Fatalf("mintWithEndpoint() = %q, %v; want new-token", tok, err)
	}

	// エラー応答
	srvErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"error":"invalid_grant","error_description":"expired"}`))
	}))
	defer srvErr.Close()

	if _, err := mintWithEndpoint(srvErr.URL, "test-client", "ref-tok"); err == nil {
		t.Fatal("expected error on OAuth error response")
	}
}

func TestRefreshSource(t *testing.T) {
	// Env
	t.Setenv("TEST_CODEX_TOKEN", "my-env-token")
	srcEnv := RefreshSource{Env: "TEST_CODEX_TOKEN"}
	if !srcEnv.Configured() {
		t.Fatal("expected Configured() to be true")
	}
	out, err := srcEnv.fetch()
	if err != nil || out != "my-env-token" {
		t.Fatalf("fetch() = %q, %v; want my-env-token", out, err)
	}

	// File
	dir := t.TempDir()
	path := filepath.Join(dir, "token.txt")
	_ = os.WriteFile(path, []byte("my-file-token\n"), 0o600)
	srcFile := RefreshSource{File: path}
	out, err = srcFile.fetch()
	if err != nil || out != "my-file-token" {
		t.Fatalf("fetch() = %q, %v; want my-file-token", out, err)
	}

	// Command
	srcCmd := RefreshSource{Command: []string{"echo", "my-cmd-token"}}
	out, err = srcCmd.fetch()
	if err != nil || out != "my-cmd-token" {
		t.Fatalf("fetch() = %q, %v; want my-cmd-token", out, err)
	}
}
