package netns

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// TestChildInspectRoundTrip は、子が点検依頼を送り、親の返答で通す/止めるが
// 切り替わることを確かめる。
func TestChildInspectRoundTrip(t *testing.T) {
	pr, pw := io.Pipe()
	c := &child{events: &eventWriter{enc: json.NewEncoder(pw)}, inspectWait: map[int]chan inspectResult{}}
	done := make(chan error, 1)
	go func() { done <- c.inspect(InspectRequest{Method: "GET", Host: "example.com", Path: "/p"}) }()

	var ev Event
	if err := json.NewDecoder(pr).Decode(&ev); err != nil {
		t.Fatal(err)
	}
	if ev.Inspect == nil || ev.Inspect.Host != "example.com" || ev.Inspect.Path != "/p" {
		t.Fatalf("点検依頼 = %+v", ev.Inspect)
	}
	if ev.InspectID == 0 {
		t.Fatal("点検の ID が無い")
	}
	c.deliverInspect(ev.InspectID, true, "")
	if err := <-done; err != nil {
		t.Fatalf("通す判定なのに止まった: %v", err)
	}
}

func TestChildInspectDenied(t *testing.T) {
	pr, pw := io.Pipe()
	c := &child{events: &eventWriter{enc: json.NewEncoder(pw)}, inspectWait: map[int]chan inspectResult{}}
	done := make(chan error, 1)
	go func() { done <- c.inspect(InspectRequest{Method: "POST", Host: "example.com"}) }()

	var ev Event
	if err := json.NewDecoder(pr).Decode(&ev); err != nil {
		t.Fatal(err)
	}
	c.deliverInspect(ev.InspectID, false, "秘密の持ち出し")
	err := <-done
	if err == nil || err.Error() != "秘密の持ち出し" {
		t.Fatalf("止める判定 = %v", err)
	}
}

// TestChildFailInspects は親との接続が切れたら、待っている点検が止める側で戻ることを
// 確かめる (接続を握ったままにしない)。
func TestChildFailInspects(t *testing.T) {
	pr, pw := io.Pipe()
	c := &child{events: &eventWriter{enc: json.NewEncoder(pw)}, inspectWait: map[int]chan inspectResult{}}
	done := make(chan error, 1)
	go func() { done <- c.inspect(InspectRequest{Method: "GET", Host: "example.com"}) }()

	var ev Event
	if err := json.NewDecoder(pr).Decode(&ev); err != nil {
		t.Fatal(err)
	}
	c.failInspects()
	if err := <-done; err == nil {
		t.Fatal("親が消えたのに点検が通った")
	}
}

func TestRemoveHopHeaders(t *testing.T) {
	h := http.Header{
		"Connection":       {"keep-alive", "X-Custom"},
		"X-Custom":         {"drop"},
		"X-Keep":           {"keep"},
		"Proxy-Connection": {"keep-alive"},
		"Te":               {"trailers"},
	}
	removeHopHeaders(h)
	for _, name := range []string{"Connection", "X-Custom", "Proxy-Connection", "Te"} {
		if h.Get(name) != "" {
			t.Fatalf("%s が残っている: %v", name, h)
		}
	}
	if h.Get("X-Keep") != "keep" {
		t.Fatalf("残すべきヘッダが消えた: %v", h)
	}
}

func TestHasToken(t *testing.T) {
	h := http.Header{"Connection": {"Upgrade, keep-alive"}}
	if !hasToken(h, "Connection", "upgrade") {
		t.Fatal("upgrade を見つけられない")
	}
	if hasToken(h, "Connection", "close") {
		t.Fatal("close が無いのに見つけた")
	}
}

func TestIsUpgrade(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://example.com/", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	if !isUpgrade(req) {
		t.Fatal("WebSocket の昇格を検出できない")
	}
	req.Header.Del("Upgrade")
	if isUpgrade(req) {
		t.Fatal("Upgrade が無いのに昇格と判定した")
	}
}
