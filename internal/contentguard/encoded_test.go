package contentguard

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"testing"
)

// TestInspectRequestRejectsEncodedBody は、Content-Encoding で符号化された本文を
// 平文に戻さずに通さないことを確かめる。圧縮した本文は走査できないため、そのまま
// 転送すると秘密情報の持ち出し経路になる。
func TestInspectRequestRejectsEncodedBody(t *testing.T) {
	scanner := NewDLPScanner()

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte("just a log line\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest("POST", "https://allowed.example.com/upload", bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Encoding", "gzip")

	detected, _, _, err := scanner.InspectRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !detected {
		t.Fatal("符号化された本文を検査せずに通した (持ち出し経路)")
	}

	// identity は平文と同じ扱い (拒否しない)。
	plain, _ := http.NewRequest("POST", "https://allowed.example.com/upload", bytes.NewReader([]byte("clean")))
	plain.Header.Set("Content-Encoding", "identity")
	if detected, _, _, err := scanner.InspectRequest(plain); err != nil || detected {
		t.Fatalf("identity のクリーンな本文を拒否した: detected=%v err=%v", detected, err)
	}
}
