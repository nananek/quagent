package proxycache

import (
	"net/http/httptest"
	"testing"
)

func TestFormatSize(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.00 KiB"},
		{1536, "1.50 KiB"},
		{1024 * 1024, "1.00 MiB"},
		{3 * 1024 * 1024 * 1024, "3.00 GiB"},
	}
	for _, c := range cases {
		if got := FormatSize(c.in); got != c.want {
			t.Errorf("FormatSize(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeDockerHubRepo(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"alpine", "library/alpine"},
		{"library/alpine", "library/alpine"},
		{"owner/repo", "owner/repo"},
	}
	for _, c := range cases {
		if got := normalizeDockerHubRepo(c.in); got != c.want {
			t.Errorf("normalizeDockerHubRepo(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSetGoContentType(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"example.com/mod/@v/v1.0.0.zip", "application/zip"},
		{"example.com/mod/@v/v1.0.0.info", "application/json; charset=utf-8"},
		{"example.com/mod/@latest", "application/json; charset=utf-8"},
		{"example.com/mod/@v/list", "text/plain; charset=utf-8"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		setGoContentType(rec, c.path)
		if got := rec.Header().Get("Content-Type"); got != c.want {
			t.Errorf("setGoContentType(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestSetPyPIContentType(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"pkg-1.0-py3-none-any.whl", "application/x-wheel+zip"},
		{"pkg-1.0.tar.gz", "application/gzip"},
		{"pkg-1.0.zip", "application/zip"},
		{"pkg-1.0.metadata", "text/plain; charset=utf-8"},
		{"pkg-1.0.unknown", "application/octet-stream"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		setPyPIContentType(rec, c.name)
		if got := rec.Header().Get("Content-Type"); got != c.want {
			t.Errorf("setPyPIContentType(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}
