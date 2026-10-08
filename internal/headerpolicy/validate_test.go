package headerpolicy

import (
	"net/http"
	"testing"
)

func TestValidateIfModifiedSince(t *testing.T) {
	valid := []string{
		"Wed, 21 Oct 2015 07:28:00 GMT",
		"Sunday, 06-Nov-94 08:49:37 GMT",
		"Sun Nov  6 08:49:37 1994",
	}
	for _, v := range valid {
		if !validateIfModifiedSince(v) {
			t.Errorf("validateIfModifiedSince(%q) = false, want true", v)
		}
	}

	invalid := []string{
		"",
		"not a date",
		"2026-10-08",
		"Wed, 21 Oct 2015 07:28:00 GMT; secret=exfil",
		"VGhpcyBpcyBhIHNlY3JldCBtZXNzYWdlIHRvIGV4ZmlsdHJhdGU=",
	}
	for _, v := range invalid {
		if validateIfModifiedSince(v) {
			t.Errorf("validateIfModifiedSince(%q) = true, want false", v)
		}
	}
}

func TestValidateContentLength(t *testing.T) {
	valid := []string{"0", "1", "12345", "18446744073709551615"}
	for _, v := range valid {
		if !validateContentLength(v) {
			t.Errorf("validateContentLength(%q) = false, want true", v)
		}
	}

	invalid := []string{
		"",
		"-1",
		"+10",
		"12.5",
		"100 ",
		" 100",
		"100; secret",
		"123456789012345678901", // > 20 digits
	}
	for _, v := range invalid {
		if validateContentLength(v) {
			t.Errorf("validateContentLength(%q) = true, want false", v)
		}
	}
}

func TestValidateRange(t *testing.T) {
	valid := []string{
		"bytes=0-499",
		"bytes=500-",
		"bytes=-500",
		"bytes=0-0,-1",
		"bytes=500-600, 601-999",
	}
	for _, v := range valid {
		if !validateRange(v) {
			t.Errorf("validateRange(%q) = false, want true", v)
		}
	}

	invalid := []string{
		"",
		"bytes=",
		"bytes=-",
		"bytes=0-100; leak=secret",
		"bytes=abc-def",
		"bytes=100-50, ",
		"items=0-100",
		"pages=1-5",
	}
	for _, v := range invalid {
		if validateRange(v) {
			t.Errorf("validateRange(%q) = true, want false", v)
		}
	}
}

func TestValidateAcceptEncoding(t *testing.T) {
	valid := []string{
		"gzip",
		"gzip, deflate, br",
		"gzip, deflate, br, zstd",
		"gzip;q=1.0, *;q=0.5",
		"gzip; q=0.8, identity",
	}
	for _, v := range valid {
		if !validateAcceptEncoding(v) {
			t.Errorf("validateAcceptEncoding(%q) = false, want true", v)
		}
	}

	invalid := []string{
		"",
		"secret-encoding",
		"gzip, custom_exfil",
		"gzip; leak=secret",
		"gzip; q=2.0",
		"gzip; q=bad",
		"gzip, , br",
	}
	for _, v := range invalid {
		if validateAcceptEncoding(v) {
			t.Errorf("validateAcceptEncoding(%q) = true, want false", v)
		}
	}
}

func TestValidateContentType(t *testing.T) {
	valid := []string{
		"application/json",
		"text/plain; charset=utf-8",
		"text/html; charset=ISO-8859-1",
		"multipart/form-data; boundary=---------------------------974767299852498929531610575",
		"application/octet-stream",
		"application/vnd.docker.distribution.manifest.v2+json",
	}
	for _, v := range valid {
		if !validateContentType(v) {
			t.Errorf("validateContentType(%q) = false, want true", v)
		}
	}

	invalid := []string{
		"",
		"invalid-no-slash",
		"application/json; secret=stolen_data",
		"application/json; charset=utf-8; leak=true",
		"multipart/form-data; boundary=too_long_" + string(make([]byte, 100)),
		"text/plain; charset=invalid_charset_name_that_is_way_too_long_for_a_standard_charset",
	}
	for _, v := range invalid {
		if validateContentType(v) {
			t.Errorf("validateContentType(%q) = true, want false", v)
		}
	}
}

func TestValidateAccept(t *testing.T) {
	valid := []string{
		"*/*",
		"application/json",
		"text/html, application/xhtml+xml, application/xml;q=0.9, */*;q=0.8",
		"application/json; charset=utf-8",
		"application/vnd.github.v3+json",
	}
	for _, v := range valid {
		if !validateAccept(v) {
			t.Errorf("validateAccept(%q) = false, want true", v)
		}
	}

	invalid := []string{
		"",
		"no-slash-media",
		"*/*; leak=stolen",
		"application/json; custom_param=secret",
		"text/html, application/json; q=invalid",
	}
	for _, v := range invalid {
		if validateAccept(v) {
			t.Errorf("validateAccept(%q) = true, want false", v)
		}
	}
}

func TestApplyDropsInvalidHeaderValues(t *testing.T) {
	p := &Policy{Enabled: true}
	h := http.Header{}
	// 正当なヘッダ
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Range", "bytes=0-1024")
	h.Set("If-Modified-Since", "Wed, 21 Oct 2015 07:28:00 GMT")
	// ヘッダ名は許可されているが値が不正 (エクスフィルトレーションの試み)
	h.Set("Content-Length", "100; secret=stolen")
	h.Set("Accept-Encoding", "gzip, custom_exfil_channel")

	dropped := p.Rules("example.com").Apply(h)

	// 正当なものは残る
	for _, k := range []string{"Accept", "Content-Type", "Range", "If-Modified-Since"} {
		if h.Get(k) == "" {
			t.Errorf("%s が落ちている", k)
		}
	}
	// 値が不正なものは落とされる
	for _, k := range []string{"Content-Length", "Accept-Encoding"} {
		if h.Get(k) != "" {
			t.Errorf("%s が残っている", k)
		}
	}

	// drop リストに含まれていること
	hasDropped := func(name string) bool {
		for _, d := range dropped {
			if d == name {
				return true
			}
		}
		return false
	}
	if !hasDropped("Content-Length") || !hasDropped("Accept-Encoding") {
		t.Fatalf("dropped に不正ヘッダが含まれていない: %v", dropped)
	}
}
