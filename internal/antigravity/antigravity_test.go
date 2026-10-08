package antigravity

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMinterCaches(t *testing.T) {
	calls := 0
	m := &Minter{mint: func() (string, error) { calls++; return "tok", nil }}
	got, err := m.Token()
	if err != nil || got != "tok" {
		t.Fatalf("Token() = %q, %v", got, err)
	}
	got, err = m.Token()
	if err != nil || got != "tok" {
		t.Fatalf("Token() = %q, %v", got, err)
	}
	if calls != 1 {
		t.Fatalf("作り直しが %d 回 (使い回していない)", calls)
	}
}

func TestMinterError(t *testing.T) {
	m := &Minter{mint: func() (string, error) { return "", errors.New("boom") }}
	if _, err := m.Token(); err == nil {
		t.Fatal("エラーを返していない")
	}
}

func TestAllowCoversObserved(t *testing.T) {
	// 観測した agy のサブスク経路の呼び出し
	for _, p := range []string{
		"/v1internal:loadCodeAssist",
		"/v1internal:fetchAvailableModels",
		"/v1internal:retrieveUserQuotaSummary",
		"/v1internal:streamGenerateContent",
		"/v1internal:recordTrajectoryAnalytics",
	} {
		found := false
		for _, op := range Allow {
			m, rest, _ := strings.Cut(op, " ")
			if m == "POST" && strings.HasPrefix(p, strings.TrimSuffix(rest, "*")) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Allow に %s が無い", p)
		}
	}
}

func TestUserInfoWith(t *testing.T) {
	srv := httptestServer(`{"email":"a@example.com","picture":"https://lh3.googleusercontent.com/a/XYZ=s96-c"}`)
	defer srv.Close()
	email, picture, err := userInfoWith(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if email != "a@example.com" {
		t.Fatalf("email=%q", email)
	}
	if got := PictureHost(picture); got != "lh3.googleusercontent.com" {
		t.Fatalf("host=%q", got)
	}
	if got := PictureHost(""); got != "" {
		t.Fatalf("空のはず: %q", got)
	}
	if got := PictureHost("://壊"); got != "" {
		t.Fatalf("不正のはず: %q", got)
	}
}

func TestUserInfoWithError(t *testing.T) {
	srv := httptestServerWithStatus(401)
	defer srv.Close()
	if _, _, err := userInfoWith(srv.URL, "tok"); err == nil {
		t.Fatal("401 を通した")
	}
}

func httptestServer(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(body))
	}))
}

func httptestServerWithStatus(code int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
	}))
}

func TestMintWithEndpoint(t *testing.T) {
	// 1. 正常系
	srvSuccess := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		_ = r.ParseForm()
		if r.FormValue("grant_type") != "refresh_token" {
			t.Errorf("grant_type = %q", r.FormValue("grant_type"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"new-acc-tok"}`))
	}))
	defer srvSuccess.Close()

	tok, err := mintWithEndpoint(srvSuccess.URL, credentials{id: "cid", secret: "sec"}, "my-rt")
	if err != nil || tok != "new-acc-tok" {
		t.Fatalf("mintWithEndpoint() = %q, %v; want new-acc-tok", tok, err)
	}

	// 2. エラーレスポンス (OAuthエラー)
	srvOAuthErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"error":"invalid_grant","error_description":"token expired"}`))
	}))
	defer srvOAuthErr.Close()

	if _, err := mintWithEndpoint(srvOAuthErr.URL, credentials{id: "cid", secret: "sec"}, "my-rt"); err == nil {
		t.Fatal("expected error on OAuth error response")
	}

	// 3. 不正なJSON
	srvBadJSON := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`not json`))
	}))
	defer srvBadJSON.Close()

	if _, err := mintWithEndpoint(srvBadJSON.URL, credentials{id: "cid", secret: "sec"}, "my-rt"); err == nil {
		t.Fatal("expected error on invalid JSON")
	}

	// 4. サーバーダウン (接続不可)
	if _, err := mintWithEndpoint("http://127.0.0.1:0", credentials{id: "cid", secret: "sec"}, "my-rt"); err == nil {
		t.Fatal("expected error on connection failure")
	}
}

func TestUniq(t *testing.T) {
	in := []string{"a", "b", "a", "c", "b", "d"}
	got := uniq(in)
	want := []string{"a", "b", "c", "d"}
	if len(got) != len(want) {
		t.Fatalf("uniq(%v) = %v, want %v", in, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("uniq[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	empty := uniq(nil)
	if len(empty) != 0 {
		t.Errorf("uniq(nil) = %v, want empty", empty)
	}
}

func TestTokenFileAndRefreshToken(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	tf := TokenFile()
	if !strings.HasSuffix(tf, filepath.Join(".gemini", "antigravity-cli", "antigravity-oauth-token")) {
		t.Errorf("TokenFile() = %q", tf)
	}

	// 1. ファイルが存在しない
	if _, err := refreshToken(); err == nil {
		t.Fatal("expected error for missing token file")
	}

	// ディレクトリ作成
	if err := os.MkdirAll(filepath.Dir(tf), 0o755); err != nil {
		t.Fatal(err)
	}

	// 2. 不正な JSON
	if err := os.WriteFile(tf, []byte("invalid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := refreshToken(); err == nil {
		t.Fatal("expected error for invalid json")
	}

	// 3. refresh_token が空
	if err := os.WriteFile(tf, []byte(`{"token":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := refreshToken(); err == nil {
		t.Fatal("expected error for empty refresh token")
	}

	// 4. 正常な refresh_token
	if err := os.WriteFile(tf, []byte(`{"token":{"refresh_token":"rt-xyz"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rt, err := refreshToken()
	if err != nil || rt != "rt-xyz" {
		t.Fatalf("refreshToken() = %q, %v; want rt-xyz", rt, err)
	}
}

