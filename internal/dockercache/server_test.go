package dockercache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nananek/quagent/internal/console"
)

type mockUpstream struct {
	name            string
	manifestResp    *http.Response
	manifestErr     error
	blobResp        *http.Response
	blobErr         error
	gotManifestRepo string
	gotManifestRef  string
	gotBlobDigest   string
}

func (m *mockUpstream) Name() string { return m.name }
func (m *mockUpstream) GetManifest(ctx context.Context, repository, reference string, headers http.Header) (*http.Response, error) {
	m.gotManifestRepo = repository
	m.gotManifestRef = reference
	return m.manifestResp, m.manifestErr
}
func (m *mockUpstream) GetBlob(ctx context.Context, repository, digest string) (*http.Response, error) {
	m.gotBlobDigest = digest
	return m.blobResp, m.blobErr
}

func TestParsePath(t *testing.T) {
	tests := []struct {
		path         string
		wantRegistry string
		wantRepo     string
		wantAction   string
		wantRef      string
		wantOK       bool
	}{
		{
			path:         "/v2/library/golang/manifests/1.24",
			wantRegistry: "docker.io",
			wantRepo:     "library/golang",
			wantAction:   "manifests",
			wantRef:      "1.24",
			wantOK:       true,
		},
		{
			path:         "/v2/ghcr.io/astral-sh/uv/manifests/latest",
			wantRegistry: "ghcr.io",
			wantRepo:     "astral-sh/uv",
			wantAction:   "manifests",
			wantRef:      "latest",
			wantOK:       true,
		},
		{
			path:         "/v2/docker.io/library/alpine/blobs/sha256:1234",
			wantRegistry: "docker.io",
			wantRepo:     "library/alpine",
			wantAction:   "blobs",
			wantRef:      "sha256:1234",
			wantOK:       true,
		},
		{
			path:         "/v2/invalid",
			wantOK:       false,
		},
	}

	for _, tt := range tests {
		reg, repo, act, ref, ok := parsePath(tt.path)
		if ok != tt.wantOK {
			t.Errorf("parsePath(%q) ok = %v, want %v", tt.path, ok, tt.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if reg != tt.wantRegistry || repo != tt.wantRepo || act != tt.wantAction || ref != tt.wantRef {
			t.Errorf("parsePath(%q) = (%s, %s, %s, %s), want (%s, %s, %s, %s)",
				tt.path, reg, repo, act, ref, tt.wantRegistry, tt.wantRepo, tt.wantAction, tt.wantRef)
		}
	}
}

func TestServerApprovalAndCaching(t *testing.T) {
	tmpDir := t.TempDir()
	storage, err := NewStorage(tmpDir, 1)
	if err != nil {
		t.Fatal(err)
	}

	blobContent := []byte("hello layer world")
	hasher := sha256.New()
	hasher.Write(blobContent)
	digest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))

	mockHub := &mockUpstream{
		name: "docker.io",
		manifestResp: &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type":          []string{"application/vnd.docker.distribution.manifest.v2+json"},
				"Docker-Content-Digest": []string{"sha256:manifest123"},
			},
			Body: io.NopCloser(bytes.NewReader([]byte(`{"schemaVersion": 2}`))),
		},
		blobResp: &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(blobContent)),
		},
	}

	var approvalCalled bool
	var approvalAllow bool

	srv := NewServer(storage, mockHub, nil)
	srv.AskApproval = func(info console.DockerImageInfo) error {
		approvalCalled = true
		if !approvalAllow {
			return errors.New("ユーザーにより拒否されました")
		}
		return nil
	}

	// 1. キャッシュミスで拒否された場合
	approvalCalled = false
	approvalAllow = false

	req := httptest.NewRequest("GET", "/v2/library/alpine/manifests/latest", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if !approvalCalled {
		t.Errorf("AskApproval が呼ばれませんでした")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d (Forbidden)", rec.Code, http.StatusForbidden)
	}

	// 2. キャッシュミスで許可された場合
	approvalCalled = false
	approvalAllow = true

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if !approvalCalled {
		t.Errorf("AskApproval が呼ばれませんでした")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (OK)", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), `"schemaVersion": 2`) {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}

	// 3. キャッシュヒット (承認不要)
	approvalCalled = false

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if approvalCalled {
		t.Errorf("キャッシュヒット時に AskApproval が呼ばれてしまいました")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (OK)", rec.Code, http.StatusOK)
	}

	// 4. blob の取得
	blobReq := httptest.NewRequest("GET", "/v2/library/alpine/blobs/"+digest, nil)
	blobRec := httptest.NewRecorder()
	srv.ServeHTTP(blobRec, blobReq)

	if blobRec.Code != http.StatusOK {
		t.Errorf("blob status = %d, want %d", blobRec.Code, http.StatusOK)
	}
	if blobRec.Body.String() != string(blobContent) {
		t.Errorf("blob body = %q, want %q", blobRec.Body.String(), string(blobContent))
	}

	// 5. blob キャッシュヒット (再度取得)
	blobRec2 := httptest.NewRecorder()
	srv.ServeHTTP(blobRec2, blobReq)

	if blobRec2.Code != http.StatusOK {
		t.Errorf("blob cache hit status = %d, want %d", blobRec2.Code, http.StatusOK)
	}
	if blobRec2.Body.String() != string(blobContent) {
		t.Errorf("blob cache hit body = %q, want %q", blobRec2.Body.String(), string(blobContent))
	}
}

func TestStorageAndMaintenance(t *testing.T) {
	tmpDir := t.TempDir()
	storage, err := NewStorage(tmpDir, 1)
	if err != nil {
		t.Fatal(err)
	}

	content := []byte("dummy layer data")
	hasher := sha256.New()
	hasher.Write(content)
	digest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))

	// Save blob
	n, err := storage.SaveBlob(digest, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("SaveBlob failed: %v", err)
	}
	if n != int64(len(content)) {
		t.Errorf("saved bytes = %d, want %d", n, len(content))
	}

	// Checksum mismatch
	_, err = storage.SaveBlob("sha256:0000000000000000000000000000000000000000000000000000000000000000", bytes.NewReader(content))
	if err == nil {
		t.Errorf("checksum mismatch should fail")
	}

	// Save manifest
	err = storage.SaveManifest("docker.io", "library/alpine", "latest", "application/json", digest, []byte("{}"))
	if err != nil {
		t.Fatalf("SaveManifest failed: %v", err)
	}

	// Inspect cache
	stats, err := InspectCache(tmpDir)
	if err != nil {
		t.Fatalf("InspectCache failed: %v", err)
	}
	if stats.TotalBlobs != 1 {
		t.Errorf("TotalBlobs = %d, want 1", stats.TotalBlobs)
	}
	if len(stats.Images) != 1 {
		t.Errorf("Images len = %d, want 1", len(stats.Images))
	}

	// Clean cache
	if err := CleanCache(tmpDir); err != nil {
		t.Fatalf("CleanCache failed: %v", err)
	}

	statsAfter, err := InspectCache(tmpDir)
	if err != nil {
		t.Fatalf("InspectCache after clean failed: %v", err)
	}
	if statsAfter.TotalBlobs != 0 {
		t.Errorf("TotalBlobs after clean = %d, want 0", statsAfter.TotalBlobs)
	}
}
