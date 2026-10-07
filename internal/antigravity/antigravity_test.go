package antigravity

import (
	"errors"
	"net/http"
	"net/http/httptest"
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
