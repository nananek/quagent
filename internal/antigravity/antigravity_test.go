package antigravity

import (
	"context"
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
	if _, err := refreshTokenFrom(RefreshSource{}); err == nil {
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
	if _, err := refreshTokenFrom(RefreshSource{}); err == nil {
		t.Fatal("expected error for invalid json")
	}

	// 3. refresh_token が空
	if err := os.WriteFile(tf, []byte(`{"token":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := refreshTokenFrom(RefreshSource{}); err == nil {
		t.Fatal("expected error for empty refresh token")
	}

	// 4. 正常な refresh_token
	if err := os.WriteFile(tf, []byte(`{"token":{"refresh_token":"rt-xyz"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rt, err := refreshTokenFrom(RefreshSource{})
	if err != nil || rt != "rt-xyz" {
		t.Fatalf("refreshToken() = %q, %v; want rt-xyz", rt, err)
	}
}

func TestFindCredentials(t *testing.T) {
	// 1. agy が PATH にない
	t.Run("NoAgy", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if _, err := findCredentials(); err == nil {
			t.Fatal("expected error when agy is not found")
		}
	})

	// 2. agy があるがクレデンシャルを含まない
	t.Run("NoCredentialsInBinary", func(t *testing.T) {
		binDir := t.TempDir()
		agyPath := filepath.Join(binDir, "agy")
		if err := os.WriteFile(agyPath, []byte("#!/bin/sh\necho hello\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", binDir)
		if _, err := findCredentials(); err == nil {
			t.Fatal("expected error when credentials are not found")
		}
	})

	// 3. agy からクレデンシャルを正しく抽出できる
	t.Run("ValidCredentials", func(t *testing.T) {
		binDir := t.TempDir()
		agyPath := filepath.Join(binDir, "agy")
		dummyContent := "junk " +
			"123456789-abcdef.apps.googleusercontent.com" +
			" other " +
			"GOCSPX-secret_12345678" +
			" end"
		if err := os.WriteFile(agyPath, []byte(dummyContent), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", binDir)
		creds, err := findCredentials()
		if err != nil {
			t.Fatalf("findCredentials() failed: %v", err)
		}
		if len(creds) == 0 {
			t.Fatal("expected credentials, got none")
		}
		found := false
		for _, c := range creds {
			if c.id == "123456789-abcdef.apps.googleusercontent.com" && c.secret == "GOCSPX-secret_12345678" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected credential pair not found in %v", creds)
		}
	})
}

func TestCheckLogin(t *testing.T) {
	binDir := t.TempDir()
	agyPath := filepath.Join(binDir, "agy")
	if err := os.WriteFile(agyPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	// トークンが無い場合
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	if err := CheckLogin(RefreshSource{}); err == nil {
		t.Fatal("expected error without token file")
	}

	// トークンがある場合
	tokenPath := filepath.Join(homeDir, ".gemini", "antigravity-cli", "antigravity-oauth-token")
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte(`{"token":{"refresh_token":"rt-ok"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckLogin(RefreshSource{}); err != nil {
		t.Fatalf("CheckLogin() failed: %v", err)
	}
}

func TestWarmup(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	tokenPath := filepath.Join(homeDir, ".gemini", "antigravity-cli", "antigravity-oauth-token")
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte(`{"token":{"refresh_token":"rt-ok"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. 成功ケース
	t.Run("Success", func(t *testing.T) {
		binDir := t.TempDir()
		agyPath := filepath.Join(binDir, "agy")
		script := "#!/bin/sh\nif [ \"$1\" = \"models\" ]; then exit 0; fi\nexit 1\n"
		if err := os.WriteFile(agyPath, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", binDir)
		if err := Warmup(context.Background(), RefreshSource{}); err != nil {
			t.Fatalf("Warmup() failed: %v", err)
		}
	})

	// 2. 失敗ケース (エラー出力あり)
	t.Run("FailureOutput", func(t *testing.T) {
		binDir := t.TempDir()
		agyPath := filepath.Join(binDir, "agy")
		script := "#!/bin/sh\necho 'auth failed' >&2\nexit 1\n"
		if err := os.WriteFile(agyPath, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", binDir)
		if err := Warmup(context.Background(), RefreshSource{}); err == nil || !strings.Contains(err.Error(), "auth failed") {
			t.Fatalf("expected error with auth failed, got %v", err)
		}
	})

	// 3. 失敗ケース (エラー出力なし)
	t.Run("FailureEmptyOutput", func(t *testing.T) {
		binDir := t.TempDir()
		agyPath := filepath.Join(binDir, "agy")
		script := "#!/bin/sh\nexit 1\n"
		if err := os.WriteFile(agyPath, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", binDir)
		if err := Warmup(context.Background(), RefreshSource{}); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestNewMinter(t *testing.T) {
	m := NewMinter(RefreshSource{})
	if m == nil || m.mint == nil {
		t.Fatal("NewMinter() returned invalid instance")
	}
}

func TestMintError(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // トークンファイルが無い環境
	if _, err := Mint(RefreshSource{}); err == nil {
		t.Fatal("expected error from Mint when no token exists")
	}
}

func TestRefreshSourceConfigured(t *testing.T) {
	if (RefreshSource{}).Configured() {
		t.Error("empty source should not be configured")
	}
	for _, src := range []RefreshSource{
		{Env: "X"},
		{File: "/tmp/x"},
		{Command: []string{"echo", "x"}},
	} {
		if !src.Configured() {
			t.Errorf("%+v should be configured", src)
		}
	}
}

func TestRefreshSourceFetch(t *testing.T) {
	// 1. 何も指定しない
	if _, err := (RefreshSource{}).fetch(); err == nil {
		t.Error("expected error for empty source")
	}

	// 2. 環境変数
	t.Setenv("TEST_QUAGENT_AGY_RT", "  rt-env  ")
	out, err := (RefreshSource{Env: "TEST_QUAGENT_AGY_RT"}).fetch()
	if err != nil || out != "rt-env" {
		t.Errorf("fetch(env) = %q, %v; want rt-env", out, err)
	}
	if _, err := (RefreshSource{Env: "TEST_QUAGENT_AGY_RT_MISSING"}).fetch(); err == nil {
		t.Error("expected error for missing env var")
	}
	t.Setenv("TEST_QUAGENT_AGY_RT_EMPTY", "  \n")
	if _, err := (RefreshSource{Env: "TEST_QUAGENT_AGY_RT_EMPTY"}).fetch(); err == nil {
		t.Error("expected error for empty env var")
	}

	// 3. ファイル (~/ 展開を含む)
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	rtFile := filepath.Join(tmp, "rt.json")
	if err := os.WriteFile(rtFile, []byte(`{"token":{"refresh_token":"rt-file"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = (RefreshSource{File: "~/rt.json"}).fetch()
	if err != nil || out != `{"token":{"refresh_token":"rt-file"}}` {
		t.Errorf("fetch(file ~/) = %q, %v", out, err)
	}
	if _, err := (RefreshSource{File: "~/no-such-file"}).fetch(); err == nil {
		t.Error("expected error for missing file")
	}

	// 4. コマンド
	out, err = (RefreshSource{Command: []string{"echo", "rt-cmd"}}).fetch()
	if err != nil || out != "rt-cmd" {
		t.Errorf("fetch(command) = %q, %v; want rt-cmd", out, err)
	}
	if _, err := (RefreshSource{Command: []string{"false"}}).fetch(); err == nil {
		t.Error("expected error for failing command")
	}
}

func TestExtractRefreshToken(t *testing.T) {
	// 1. トークンファイルと同じ JSON
	rt, err := extractRefreshToken(`{"token":{"access_token":"a","refresh_token":"rt-json"},"auth_method":"consumer"}`)
	if err != nil || rt != "rt-json" {
		t.Errorf("extract(json) = %q, %v; want rt-json", rt, err)
	}

	// 2. 素の refresh_token
	rt, err = extractRefreshToken("rt-raw")
	if err != nil || rt != "rt-raw" {
		t.Errorf("extract(raw) = %q, %v; want rt-raw", rt, err)
	}

	// 3. JSON だが refresh_token が無い
	if _, err := extractRefreshToken(`{"token":{}}`); err == nil {
		t.Error("expected error for json without refresh token")
	}
}

func TestRefreshTokenFromSource(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // ファイル側は使わない

	// 1. JSON 出力のコマンド
	rt, err := refreshTokenFrom(RefreshSource{Command: []string{"echo", `{"token":{"refresh_token":"rt-src-json"}}`}})
	if err != nil || rt != "rt-src-json" {
		t.Errorf("refreshTokenFrom(json cmd) = %q, %v; want rt-src-json", rt, err)
	}

	// 2. 素の refresh_token の環境変数
	t.Setenv("TEST_QUAGENT_AGY_RT_RAW", "rt-src-raw")
	rt, err = refreshTokenFrom(RefreshSource{Env: "TEST_QUAGENT_AGY_RT_RAW"})
	if err != nil || rt != "rt-src-raw" {
		t.Errorf("refreshTokenFrom(raw env) = %q, %v; want rt-src-raw", rt, err)
	}

	// 3. 失敗する取り出し方
	if _, err := refreshTokenFrom(RefreshSource{Env: "TEST_QUAGENT_AGY_RT_MISSING"}); err == nil {
		t.Error("expected error for missing env var")
	}
}
