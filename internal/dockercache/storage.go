package dockercache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var validDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// Storage はキャッシュされたマニフェストとレイヤー blob を管理する。
type Storage struct {
	rootDir  string
	maxBytes int64

	mu sync.Mutex
}

// ManifestMeta はキャッシュされたマニフェストのヘッダー情報。
type ManifestMeta struct {
	ContentType   string `json:"content_type"`
	Digest        string `json:"digest"`
	CachedAt      time.Time `json:"cached_at"`
}

// NewStorage は指定されたディレクトリと最大容量で Storage を作成する。
func NewStorage(rootDir string, maxSizeGiB int) (*Storage, error) {
	if rootDir == "" {
		return nil, errors.New("rootDir が指定されていません")
	}
	if maxSizeGiB <= 0 {
		maxSizeGiB = 10
	}

	for _, sub := range []string{"blobs/sha256", "manifests", "tmp"} {
		if err := os.MkdirAll(filepath.Join(rootDir, sub), 0755); err != nil {
			return nil, fmt.Errorf("キャッシュディレクトリ作成失敗: %w", err)
		}
	}

	return &Storage{
		rootDir:  rootDir,
		maxBytes: int64(maxSizeGiB) * 1024 * 1024 * 1024,
	}, nil
}

// blobPath は指定された digest に対する blob ファイルのパスを返す。
func (s *Storage) blobPath(digest string) (string, error) {
	if !validDigestPattern.MatchString(digest) {
		return "", fmt.Errorf("不正な digest 形式: %s", digest)
	}
	hash := strings.TrimPrefix(digest, "sha256:")
	return filepath.Join(s.rootDir, "blobs", "sha256", hash), nil
}

// HasBlob は blob がキャッシュに存在するかを返す。
func (s *Storage) HasBlob(digest string) (bool, int64) {
	p, err := s.blobPath(digest)
	if err != nil {
		return false, 0
	}
	fi, err := os.Stat(p)
	if err != nil {
		return false, 0
	}
	return true, fi.Size()
}

// OpenBlob はキャッシュされた blob を読み取り用に開く (アクセス時刻更新)。
func (s *Storage) OpenBlob(digest string) (*os.File, int64, error) {
	p, err := s.blobPath(digest)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}

	// LRU 用にアクセス時刻 (mtime/atime) を更新 (エラーは無視)
	now := time.Now()
	_ = os.Chtimes(p, now, now)

	return f, fi.Size(), nil
}

// SaveBlob はストリームから blob を読み取って SHA256 検証を行い、アトミックにキャッシュへ保存する。
// 書き込み完了後は必要に応じて LRU パージを実行する。
func (s *Storage) SaveBlob(digest string, r io.Reader) (int64, error) {
	targetPath, err := s.blobPath(digest)
	if err != nil {
		return 0, err
	}
	expectedHash := strings.TrimPrefix(digest, "sha256:")

	s.mu.Lock()
	defer s.mu.Unlock()

	// 既に存在していればそのまま返す
	if fi, err := os.Stat(targetPath); err == nil {
		return fi.Size(), nil
	}

	// 一時ファイルにダウンロード
	tmpFile, err := os.CreateTemp(filepath.Join(s.rootDir, "tmp"), "blob-*")
	if err != nil {
		return 0, fmt.Errorf("一時ファイル作成失敗: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		tmpFile.Close()
		_ = os.Remove(tmpPath)
	}()

	hasher := sha256.New()
	writer := io.MultiWriter(tmpFile, hasher)

	n, err := io.Copy(writer, r)
	if err != nil {
		return 0, fmt.Errorf("blob 書き込み失敗: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return 0, fmt.Errorf("一時ファイル close 失敗: %w", err)
	}

	actualHash := hex.EncodeToString(hasher.Sum(nil))
	if actualHash != expectedHash {
		return 0, fmt.Errorf("blob のチェックサム不一致: expected %s, got %s", expectedHash, actualHash)
	}

	if err := os.Rename(tmpPath, targetPath); err != nil {
		return 0, fmt.Errorf("blob 移動失敗: %w", err)
	}

	// 容量制限を超えていれば古い blob をパージ
	s.evictIfNeededLocked()

	return n, nil
}

// manifestPaths はマニフェストファイルとそのメタデータファイルのパスを返す。
func (s *Storage) manifestPaths(registry, repository, reference string) (dataPath, metaPath string, err error) {
	// ディレクトリトラバーサル防止
	if strings.Contains(registry, "..") || strings.Contains(repository, "..") || strings.Contains(reference, "..") {
		return "", "", errors.New("不正なパス文字が含まれています")
	}
	dir := filepath.Join(s.rootDir, "manifests", registry, filepath.Clean(repository))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", "", err
	}
	cleanRef := filepath.Clean(reference)
	return filepath.Join(dir, cleanRef+".json"), filepath.Join(dir, cleanRef+".meta"), nil
}

// SaveManifest はマニフェストとメタデータを保存する。
func (s *Storage) SaveManifest(registry, repository, reference, contentType, digest string, data []byte) error {
	dataPath, metaPath, err := s.manifestPaths(registry, repository, reference)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.WriteFile(dataPath, data, 0644); err != nil {
		return fmt.Errorf("マニフェストデータ保存失敗: %w", err)
	}

	meta := ManifestMeta{
		ContentType: contentType,
		Digest:      digest,
		CachedAt:    time.Now(),
	}
	metaBytes, _ := json.Marshal(meta)
	if err := os.WriteFile(metaPath, metaBytes, 0644); err != nil {
		return fmt.Errorf("マニフェストメタデータ保存失敗: %w", err)
	}

	return nil
}

// GetManifest はキャッシュされたマニフェストとメタデータを読み出す。
func (s *Storage) GetManifest(registry, repository, reference string) ([]byte, *ManifestMeta, error) {
	dataPath, metaPath, err := s.manifestPaths(registry, repository, reference)
	if err != nil {
		return nil, nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(dataPath)
	if err != nil {
		return nil, nil, err
	}

	metaBytes, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, nil, err
	}

	var meta ManifestMeta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return nil, nil, err
	}

	return data, &meta, nil
}

type blobInfo struct {
	path    string
	size    int64
	modTime time.Time
}

// evictIfNeededLocked は blob の合計サイズが maxBytes を超えている場合、
// 最も古い blob から順に削除する (s.mu 保持前提)。
func (s *Storage) evictIfNeededLocked() {
	blobsDir := filepath.Join(s.rootDir, "blobs", "sha256")
	entries, err := os.ReadDir(blobsDir)
	if err != nil {
		return
	}

	var blobs []blobInfo
	var totalSize int64

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		blobs = append(blobs, blobInfo{
			path:    filepath.Join(blobsDir, e.Name()),
			size:    info.Size(),
			modTime: info.ModTime(),
		})
		totalSize += info.Size()
	}

	if totalSize <= s.maxBytes {
		return
	}

	// 最も古い順 (modTime 昇順) にソート
	sort.Slice(blobs, func(i, j int) bool {
		return blobs[i].modTime.Before(blobs[j].modTime)
	})

	for _, b := range blobs {
		if totalSize <= s.maxBytes {
			break
		}
		if err := os.Remove(b.path); err == nil {
			totalSize -= b.size
		}
	}
}
