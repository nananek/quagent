package antigravity

import (
	"errors"
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
