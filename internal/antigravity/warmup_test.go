package antigravity

import (
	"context"
	"strings"
	"testing"
)

func TestCheckLoginNoAgy(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if err := CheckLogin(); err == nil || !strings.Contains(err.Error(), "agy が見つからない") {
		t.Fatalf("agy 無しを検出していない: %v", err)
	}
}

func TestWarmupNoAgy(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if err := Warmup(context.Background()); err == nil || !strings.Contains(err.Error(), "agy が見つからない") {
		t.Fatalf("agy 無しを検出していない: %v", err)
	}
}

func TestWarmupNilContextNoAgy(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if err := Warmup(nil); err == nil {
		t.Fatal("エラーを返していない")
	}
}
