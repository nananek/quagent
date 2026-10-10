package main

import (
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/nananek/quagent/internal/antigravity"
	"github.com/nananek/quagent/internal/authproxy"
	"github.com/nananek/quagent/internal/hostsvc"
)

// TestAgySubscriptionE2E は本物の agy + 本物のサブスク上流でプロキシ経路を検証する。
// host の agy ログインと外向き通信が要るので、QUAGENT_E2E_AGY=1 のときだけ動く。
func TestAgySubscriptionE2E(t *testing.T) {
	if os.Getenv("QUAGENT_E2E_AGY") == "" {
		t.Skip("QUAGENT_E2E_AGY=1 のときだけ")
	}
	if _, err := exec.LookPath("agy"); err != nil {
		t.Skip("agy が無い")
	}
	minter := antigravity.NewMinter(antigravity.RefreshSource{})
	seed, err := minter.Token()
	if err != nil {
		t.Fatalf("mint できない: %v", err)
	}
	logger := log.New(os.Stderr, "", log.Ltime)
	svc, err := hostsvc.New(2)
	if err != nil {
		t.Fatal(err)
	}
	// 本番 (run.go) と同じく seed を追加の合言葉にする。hostsvc の認証も検証する。
	svc.ExtraTokens = []string{seed}
	mux := svc.Mux
	secret := func() (string, error) {
		tok, err := minter.Token()
		return "Bearer " + tok, err
	}
	if err := authproxy.RegisterDynamic(mux, antigravity.ProviderID, antigravity.Upstream,
		"Authorization", secret, antigravity.Allow, logger, nil, nil); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go http.Serve(ln, svc.Handler())
	base := "http://" + ln.Addr().String() + "/llm/" + antigravity.ProviderID

	home := t.TempDir()
	writeJSON := func(path, s string) {
		t.Helper()
		if err := os.MkdirAll(home+"/.gemini/antigravity-cli/cache", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(home+path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// 起動直後の 1 回だけ guest から直接 Google に行く (ユーザー情報の確認) ので、
	// 本物を入れる (setupAgySubscription と同じ)。
	realTok := `{"token":{"access_token":"` + seed + `","token_type":"Bearer","refresh_token":"quagent","expiry":"2035-01-01T00:00:00+09:00"},"auth_method":"consumer","id_token":"quagent"}`
	writeJSON("/.gemini/antigravity-cli/antigravity-oauth-token", realTok)
	writeJSON("/.gemini/antigravity-cli/cache/onboarding.json",
		`{"consumerOnboardingComplete": true, "enterpriseOnboardingComplete": false, "onboardingComplete": true}`)
	writeJSON("/.gemini/antigravity-cli/settings.json", `{"trustedWorkspaces": ["/work"]}`)

	cmd := exec.Command("agy", "-p", "Reply with exactly: E2E_OK", "--print-timeout", "55s")
	cmd.Env = append(os.Environ(), "HOME="+home, "CLOUD_CODE_URL="+base, "DISABLE_AUTOUPDATER=1")
	cmd.Dir = home
	out, err := cmd.CombinedOutput()
	t.Logf("agy: %s", out)
	if err != nil {
		t.Fatalf("agy が失敗: %v", err)
	}
	if got := string(out); !strings.Contains(got, "E2E_OK") {
		t.Fatalf("E2E_OK が無い: %q", got)
	}
}
