package main

import (
	"errors"
	"testing"
)

func TestAskRestartQuit(t *testing.T) {
	o := runOpts{AfterSession: func(cur string, choices []string) (string, bool) {
		return "", true
	}}
	next, discard := askRestart(o, "opencode", true)
	if next != "" || !discard {
		t.Fatalf("終了のはず: %q %v", next, discard)
	}
}

func TestAskRestartNonAgy(t *testing.T) {
	calls := 0
	old := agyWarmup
	agyWarmup = func() error { calls++; return nil }
	defer func() { agyWarmup = old }()
	o := runOpts{AfterSession: func(cur string, choices []string) (string, bool) {
		return "opencode", false
	}}
	next, _ := askRestart(o, "opencode", true)
	if next != "opencode" {
		t.Fatalf("next=%q", next)
	}
	if calls != 0 {
		t.Fatal("agy 以外で warmup した")
	}
}

func TestAskRestartAgyWarmupFailThenReselect(t *testing.T) {
	calls := 0
	old := agyWarmup
	agyWarmup = func() error {
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
	next, _ := askRestart(o, "opencode", true)
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
	agyWarmup = func() error { calls++; return nil }
	defer func() { agyWarmup = old }()
	o := runOpts{AfterSession: func(cur string, choices []string) (string, bool) {
		return "agy", false
	}}
	next, _ := askRestart(o, "opencode", true)
	if next != "agy" || calls != 1 {
		t.Fatalf("next=%q calls=%d", next, calls)
	}
}

func TestAskRestartAgyNoSubscription(t *testing.T) {
	calls := 0
	old := agyWarmup
	agyWarmup = func() error { calls++; return nil }
	defer func() { agyWarmup = old }()
	o := runOpts{AfterSession: func(cur string, choices []string) (string, bool) {
		return "agy", false
	}}
	next, _ := askRestart(o, "opencode", false)
	if next != "agy" || calls != 0 {
		t.Fatalf("サブスク無しで warmup した: %q %d", next, calls)
	}
}
