package main

import (
	"errors"
	"testing"

	"github.com/nananek/quagent/internal/antigravity"
	"github.com/nananek/quagent/internal/codex"
)

func TestAskRestartQuit(t *testing.T) {
	o := runOpts{AfterSession: func(cur string, choices []string) (string, bool) {
		return "", true
	}}
	next, discard := askRestart(o, "opencode", true, antigravity.RefreshSource{}, true, codex.RefreshSource{})
	if next != "" || !discard {
		t.Fatalf("終了のはず: %q %v", next, discard)
	}
}

func TestAskRestartNonSubscription(t *testing.T) {
	callsAgy := 0
	oldAgy := agyWarmup
	agyWarmup = func(antigravity.RefreshSource) error { callsAgy++; return nil }
	defer func() { agyWarmup = oldAgy }()

	callsCodex := 0
	oldCodex := codexWarmup
	codexWarmup = func(codex.RefreshSource) error { callsCodex++; return nil }
	defer func() { codexWarmup = oldCodex }()

	o := runOpts{AfterSession: func(cur string, choices []string) (string, bool) {
		return "opencode", false
	}}
	next, _ := askRestart(o, "opencode", true, antigravity.RefreshSource{}, true, codex.RefreshSource{})
	if next != "opencode" {
		t.Fatalf("next=%q", next)
	}
	if callsAgy != 0 || callsCodex != 0 {
		t.Fatal("opencode で warmup した")
	}
}

func TestAskRestartAgyWarmupFailThenReselect(t *testing.T) {
	calls := 0
	old := agyWarmup
	agyWarmup = func(antigravity.RefreshSource) error {
		calls++
		if calls == 1 {
			return errors.New("boom")
		}
		return nil
	}
	defer func() { agyWarmup = old }()
	answers := []string{"agy", "opencode"}
	o := runOpts{AfterSession: func(cur string, choices []string) (string, bool) {
		a := answers[0]
		answers = answers[1:]
		return a, false
	}}
	next, _ := askRestart(o, "opencode", true, antigravity.RefreshSource{}, false, codex.RefreshSource{})
	if next != "opencode" {
		t.Fatalf("選び直しのはず: %q", next)
	}
	if calls != 1 {
		t.Fatalf("warmup が %d 回 (失敗時に選び直しのはず)", calls)
	}
}

func TestAskRestartAgyWarmupOK(t *testing.T) {
	calls := 0
	old := agyWarmup
	agyWarmup = func(antigravity.RefreshSource) error { calls++; return nil }
	defer func() { agyWarmup = old }()
	o := runOpts{AfterSession: func(cur string, choices []string) (string, bool) {
		return "agy", false
	}}
	next, _ := askRestart(o, "opencode", true, antigravity.RefreshSource{}, false, codex.RefreshSource{})
	if next != "agy" || calls != 1 {
		t.Fatalf("next=%q calls=%d", next, calls)
	}
}

func TestAskRestartAgyNoSubscription(t *testing.T) {
	calls := 0
	old := agyWarmup
	agyWarmup = func(antigravity.RefreshSource) error { calls++; return nil }
	defer func() { agyWarmup = old }()
	o := runOpts{AfterSession: func(cur string, choices []string) (string, bool) {
		return "agy", false
	}}
	next, _ := askRestart(o, "opencode", false, antigravity.RefreshSource{}, false, codex.RefreshSource{})
	if next != "agy" || calls != 0 {
		t.Fatalf("サブスク無しで warmup した: %q %d", next, calls)
	}
}

func TestAskRestartCodexWarmupFailThenReselect(t *testing.T) {
	calls := 0
	old := codexWarmup
	codexWarmup = func(codex.RefreshSource) error {
		calls++
		if calls == 1 {
			return errors.New("boom")
		}
		return nil
	}
	defer func() { codexWarmup = old }()
	answers := []string{"codex", "claude"}
	o := runOpts{AfterSession: func(cur string, choices []string) (string, bool) {
		a := answers[0]
		answers = answers[1:]
		return a, false
	}}
	next, _ := askRestart(o, "opencode", false, antigravity.RefreshSource{}, true, codex.RefreshSource{})
	if next != "claude" {
		t.Fatalf("選び直しのはず: %q", next)
	}
	if calls != 1 {
		t.Fatalf("warmup が %d 回 (失敗時に選び直しのはず)", calls)
	}
}

func TestAskRestartCodexWarmupOK(t *testing.T) {
	calls := 0
	old := codexWarmup
	codexWarmup = func(codex.RefreshSource) error { calls++; return nil }
	defer func() { codexWarmup = old }()
	o := runOpts{AfterSession: func(cur string, choices []string) (string, bool) {
		return "codex", false
	}}
	next, _ := askRestart(o, "opencode", false, antigravity.RefreshSource{}, true, codex.RefreshSource{})
	if next != "codex" || calls != 1 {
		t.Fatalf("next=%q calls=%d", next, calls)
	}
}

func TestAskRestartCodexNoSubscription(t *testing.T) {
	calls := 0
	old := codexWarmup
	codexWarmup = func(codex.RefreshSource) error { calls++; return nil }
	defer func() { codexWarmup = old }()
	o := runOpts{AfterSession: func(cur string, choices []string) (string, bool) {
		return "codex", false
	}}
	next, _ := askRestart(o, "opencode", false, antigravity.RefreshSource{}, false, codex.RefreshSource{})
	if next != "codex" || calls != 0 {
		t.Fatalf("サブスク無しで warmup した: %q %d", next, calls)
	}
}
