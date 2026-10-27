package proxycache

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/nananek/quagent/internal/access"
	"github.com/nananek/quagent/internal/console"
	"github.com/nananek/quagent/internal/guest"
)

// roundTripperFunc は http.RoundTripper を関数で実装するヘルパー。
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestGoProxyCaching(t *testing.T) {
	tmpDir := t.TempDir()
	storage, err := NewStorage(tmpDir, 1)
	if err != nil {
		t.Fatal(err)
	}

	var upstreamCalls int
	client := &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			upstreamCalls++
			if strings.HasSuffix(r.URL.Path, "/@v/v1.0.0.mod") {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("module example.com/pkg\n\ngo 1.24\n")),
					Header:     http.Header{"Content-Type": []string{"text/plain"}},
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Body:       io.NopCloser(strings.NewReader("not found")),
			}, nil
		}),
	}

	srv := NewServer(storage, nil, nil)
	srv.client = client
	var approvedEcosystems []string
	srv.SetAskBatch(func(info console.BatchDownloadInfo) (access.Kind, error) {
		approvedEcosystems = append(approvedEcosystems, info.Ecosystem)
		return access.Once, nil
	})

	// 1. 初回リクエスト (キャッシュミス) -> 承認 & 上流ダウンロード
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/go/example.com/pkg/@v/v1.0.0.mod", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "module example.com/pkg") {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
	if upstreamCalls != 1 {
		t.Fatalf("expected 1 upstream call, got %d", upstreamCalls)
	}
	if len(approvedEcosystems) != 1 || approvedEcosystems[0] != "go" {
		t.Fatalf("expected go approval, got %v", approvedEcosystems)
	}

	// 2. 2回目リクエスト (キャッシュヒット) -> 上流呼び出しなし
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/go/example.com/pkg/@v/v1.0.0.mod", nil)
	srv.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec2.Code)
	}
	if upstreamCalls != 1 {
		t.Fatalf("expected still 1 upstream call (cached), got %d", upstreamCalls)
	}
}

func TestCargoSparseIndexAndCrates(t *testing.T) {
	tmpDir := t.TempDir()
	storage, err := NewStorage(tmpDir, 1)
	if err != nil {
		t.Fatal(err)
	}

	client := &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/config.json" {
				raw := `{"dl":"https://static.crates.io/crates/{crate}/{crate}-{version}.crate","api":"https://crates.io"}`
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(raw)),
					Header:     http.Header{"Content-Type": []string{"application/json"}},
				}, nil
			} else if strings.HasPrefix(r.URL.Path, "/crates/") {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("fake-crate-tarball")),
					Header:     http.Header{"Content-Type": []string{"application/x-tar"}},
				}, nil
			}
			return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("not found"))}, nil
		}),
	}

	srv := NewServer(storage, nil, nil)
	srv.client = client
	srv.SetAskBatch(func(info console.BatchDownloadInfo) (access.Kind, error) {
		return access.Once, nil
	})

	// 1. config.json の dl 書き換え確認
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/cargo/index/config.json", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("config.json expected 200, got %d", rec.Code)
	}
	var cfg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	dl, _ := cfg["dl"].(string)
	if !strings.HasPrefix(dl, "http://"+guest.GuestProxyCacheAddr+"/cargo/crates/") {
		t.Fatalf("expected dl rewritten to %s, got %s", guest.GuestProxyCacheAddr, dl)
	}

	// 2. crates ダウンロード & キャッシュ確認
	recCrate := httptest.NewRecorder()
	reqCrate := httptest.NewRequest(http.MethodGet, "/cargo/crates/serde/serde-1.0.197.crate", nil)
	srv.ServeHTTP(recCrate, reqCrate)

	if recCrate.Code != http.StatusOK {
		t.Fatalf("crate expected 200, got %d", recCrate.Code)
	}
	if recCrate.Body.String() != "fake-crate-tarball" {
		t.Fatalf("unexpected crate body: %s", recCrate.Body.String())
	}
}

func TestNPMMetadataAndTarball(t *testing.T) {
	tmpDir := t.TempDir()
	storage, err := NewStorage(tmpDir, 1)
	if err != nil {
		t.Fatal(err)
	}

	client := &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, ".tgz") {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("npm-tarball-bytes")),
					Header:     http.Header{"Content-Type": []string{"application/octet-stream"}},
				}, nil
			}
			// packument
			raw := `{"name":"express","versions":{"4.18.2":{"dist":{"tarball":"https://registry.npmjs.org/express/-/express-4.18.2.tgz"}}}}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(raw)),
				Header:     http.Header{"Content-Type": []string{"application/json"}},
			}, nil
		}),
	}

	srv := NewServer(storage, nil, nil)
	srv.client = client
	srv.SetAskBatch(func(info console.BatchDownloadInfo) (access.Kind, error) {
		return access.Once, nil
	})

	// 1. packument の tarball URL 書き換え確認
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/npm/express", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("npm packument expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "http://"+guest.GuestProxyCacheAddr+"/npm/express/-/express-4.18.2.tgz") {
		t.Fatalf("expected tarball rewritten, got: %s", rec.Body.String())
	}

	// 2. tarball ダウンロード確認
	recTgz := httptest.NewRecorder()
	reqTgz := httptest.NewRequest(http.MethodGet, "/npm/express/-/express-4.18.2.tgz", nil)
	srv.ServeHTTP(recTgz, reqTgz)

	if recTgz.Code != http.StatusOK {
		t.Fatalf("tarball expected 200, got %d", recTgz.Code)
	}
	if recTgz.Body.String() != "npm-tarball-bytes" {
		t.Fatalf("unexpected tarball body: %s", recTgz.Body.String())
	}
}

func TestPyPISimpleAndPackages(t *testing.T) {
	tmpDir := t.TempDir()
	storage, err := NewStorage(tmpDir, 1)
	if err != nil {
		t.Fatal(err)
	}

	client := &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasPrefix(r.URL.Path, "/packages/") {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("wheel-binary-bytes")),
					Header:     http.Header{"Content-Type": []string{"application/x-wheel+zip"}},
				}, nil
			}
			// simple html
			raw := `<html><body><a href="https://files.pythonhosted.org/packages/ab/cd/requests-2.31.0-py3-none-any.whl#sha256=123">requests-2.31.0-py3-none-any.whl</a></body></html>`
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(raw)),
				Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
			}, nil
		}),
	}

	srv := NewServer(storage, nil, nil)
	srv.client = client
	srv.SetAskBatch(func(info console.BatchDownloadInfo) (access.Kind, error) {
		return access.Once, nil
	})

	// 1. Simple API のリンク書き換え確認
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/pypi/simple/requests/", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("pypi simple expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "http://"+guest.GuestProxyCacheAddr+"/pypi/packages/ab/cd/requests-2.31.0-py3-none-any.whl") {
		t.Fatalf("expected simple links rewritten, got: %s", rec.Body.String())
	}

	// 2. wheel パッケージダウンロード確認
	recWhl := httptest.NewRecorder()
	reqWhl := httptest.NewRequest(http.MethodGet, "/pypi/packages/ab/cd/requests-2.31.0-py3-none-any.whl", nil)
	srv.ServeHTTP(recWhl, reqWhl)

	if recWhl.Code != http.StatusOK {
		t.Fatalf("wheel expected 200, got %d", recWhl.Code)
	}
	if recWhl.Body.String() != "wheel-binary-bytes" {
		t.Fatalf("unexpected wheel body: %s", recWhl.Body.String())
	}

	// 3. PEP 658 .metadata の承認バイパスと Content-Type 確認
	batchCalled := false
	srv.SetAskBatch(func(info console.BatchDownloadInfo) (access.Kind, error) {
		batchCalled = true
		return access.Once, nil
	})
	recMeta := httptest.NewRecorder()
	reqMeta := httptest.NewRequest(http.MethodGet, "/pypi/packages/ab/cd/requests-2.31.0-py3-none-any.whl.metadata", nil)
	srv.ServeHTTP(recMeta, reqMeta)

	if recMeta.Code != http.StatusOK {
		t.Fatalf("metadata expected 200, got %d", recMeta.Code)
	}
	if batchCalled {
		t.Fatal("expected .metadata download to bypass batch approval, but AskBatch was called")
	}
	if recMeta.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("expected text/plain; charset=utf-8, got %s", recMeta.Header().Get("Content-Type"))
	}
}

func TestParsePyPIPackageFilename(t *testing.T) {
	tests := []struct {
		filename    string
		wantPkg     string
		wantVersion string
	}{
		{"jupyterlab-4.6.4-py3-none-any.whl", "jupyterlab", "4.6.4"},
		{"jupyterlab-4.6.4-py3-none-any.whl.metadata", "jupyterlab", "4.6.4"},
		{"jupyter_core-5.7.2-py3-none-any.whl", "jupyter_core", "5.7.2"},
		{"jupyter-core-5.7.2.tar.gz", "jupyter-core", "5.7.2"},
		{"requests-2.31.0.tar.gz", "requests", "2.31.0"},
	}
	for _, tc := range tests {
		pkg, ver := parsePyPIPackageFilename(tc.filename)
		if pkg != tc.wantPkg || ver != tc.wantVersion {
			t.Errorf("parsePyPIPackageFilename(%q) = (%q, %q), want (%q, %q)",
				tc.filename, pkg, ver, tc.wantPkg, tc.wantVersion)
		}
	}
}

// TestMetadataConsentPrecedesUpstream は、メタデータ取得の許可が上流リクエストより
// 先に求められることを確かめる。許可なしに上流へ出る回帰を防ぐ。
func TestMetadataConsentPrecedesUpstream(t *testing.T) {
	storage, err := NewStorage(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var events []string
	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		events = append(events, "upstream")
		mu.Unlock()
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"name":"express","versions":{},"dist-tags":{}}`)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	})}

	srv := NewServer(storage, nil, nil)
	srv.client = client
	srv.SetAskBatch(func(info console.BatchDownloadInfo) (access.Kind, error) {
		mu.Lock()
		if info.Metadata {
			events = append(events, "metadata-ask")
		} else {
			events = append(events, "package-ask")
		}
		mu.Unlock()
		return access.Once, nil
	})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/npm/express", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) < 1 || events[0] != "metadata-ask" {
		t.Fatalf("メタデータの許可より先に上流へ出た: %v", events)
	}
}
