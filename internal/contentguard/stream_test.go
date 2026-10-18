package contentguard

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// 検査上限を超えた位置の秘密は、転送中の走査で検知して中断する。
func TestInspectRequestStreamsBodyBeyondCap(t *testing.T) {
	payload := strings.Repeat("-", MaxInspectBodySize+100) + "AK" + "IAIOSFODNN7EXAMPLE"
	req, err := http.NewRequest("POST", "https://example.com/upload", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(payload))

	scanner := NewDLPScanner()
	detected, _, _, err := scanner.InspectRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if detected {
		t.Fatal("先頭の上限内で検知した (秘密は上限の後ろにあるはず)")
	}
	if _, err := io.ReadAll(req.Body); err == nil {
		t.Fatal("転送中に秘密を検知しても中断しなかった")
	}
	v, ok := req.Body.(BodyViolation)
	if !ok {
		t.Fatal("本文が BodyViolation を実装していない")
	}
	if pattern, _, hit := v.Violation(); !hit || pattern == "" {
		t.Fatalf("違反が記録されていない (pattern=%q hit=%v)", pattern, hit)
	}
}

// 上限を超えるクリーンな本文は切り詰めずに全部読める (転送を妨げない)。
func TestInspectRequestStreamsCleanLargeBody(t *testing.T) {
	payload := strings.Repeat("a", MaxInspectBodySize+4096)
	req, err := http.NewRequest("POST", "https://example.com/upload", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(payload))

	if detected, _, _, err := NewDLPScanner().InspectRequest(req); err != nil || detected {
		t.Fatalf("クリーンな本文で detected=%v err=%v", detected, err)
	}
	got, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != payload {
		t.Fatalf("本文長 = %d, want %d", len(got), len(payload))
	}
}

// チャンク境界をまたぐ秘密も見逃さない。
func TestInspectRequestDetectsSecretSpanningChunks(t *testing.T) {
	secret := "AK" + "IAIOSFODNN7EXAMPLE"
	// 上限をちょうど跨ぐ位置に置き、先頭チャンクと後続チャンクに分割させる。
	pad := strings.Repeat("-", MaxInspectBodySize-4)
	payload := pad + secret
	req, err := http.NewRequest("POST", "https://example.com/upload", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(payload))
	if detected, _, _, err := NewDLPScanner().InspectRequest(req); err != nil || detected {
		t.Fatalf("先頭検査で detected=%v err=%v (want false)", detected, err)
	}
	if _, err := io.ReadAll(req.Body); err == nil {
		t.Fatal("境界をまたぐ秘密を検知しなかった")
	}
}
