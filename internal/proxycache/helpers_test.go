package proxycache

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

// TestHasPkgFileRejectsDirectory は、ディレクトリをキャッシュファイルとして
// 扱わないことを確かめる (io.ReadAll が EISDIR になり、空の 200 を返してしまう)。
func TestHasPkgFileRejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	storage, err := NewStorage(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "cargo", "index"), 0o755); err != nil {
		t.Fatal(err)
	}
	if has, _ := storage.HasPkgFile("cargo", "index"); has {
		t.Fatal("ディレクトリをキャッシュヒット扱いした")
	}
	if _, _, err := storage.OpenPkgFile("cargo", "index"); err == nil {
		t.Fatal("ディレクトリを開けてしまった")
	}
}

// TestCargoConfigJSONDoesNotCacheError は、上流のエラー応答を 200 として
// キャッシュしないことを確かめる。
func TestCargoConfigJSONDoesNotCacheError(t *testing.T) {
	dir := t.TempDir()
	storage, err := NewStorage(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(storage, nil, nil)
	srv.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusInternalServerError, Body: io.NopCloser(strings.NewReader("boom"))}, nil
	})}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/cargo/index/config.json", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500", rec.Code)
	}
	if _, err := storage.GetPkgFile("cargo", "index/config.json"); err == nil {
		t.Fatal("エラー応答がキャッシュされた")
	}
}

// TestCargoConfigJSONFallsBackToCacheOnError は、上流がエラーのときキャッシュが
// あればそれを返すことを確かめる。
func TestCargoConfigJSONFallsBackToCacheOnError(t *testing.T) {
	dir := t.TempDir()
	storage, err := NewStorage(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.SavePkgFile("cargo", "index/config.json", strings.NewReader(`{"dl":"cached"}`)); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(storage, nil, nil)
	srv.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadGateway, Body: io.NopCloser(strings.NewReader("boom"))}, nil
	})}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/cargo/index/config.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (キャッシュ)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cached") {
		t.Fatalf("body = %q, want cached", rec.Body.String())
	}
}
