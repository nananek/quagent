package dockercache

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CachedImage はキャッシュされているイメージ情報。
type CachedImage struct {
	Registry   string    `json:"registry"`
	Repository string    `json:"repository"`
	Reference  string    `json:"reference"`
	Digest     string    `json:"digest"`
	CachedAt   time.Time `json:"cached_at"`
}

// CacheStats は Docker キャッシュの全体統計。
type CacheStats struct {
	TotalBlobs     int           `json:"total_blobs"`
	TotalBlobBytes int64         `json:"total_blob_bytes"`
	Images         []CachedImage `json:"images"`
}

// FormatSize はバイト数を人間が読みやすい形式 (MiB/GiB) に変換する。
func FormatSize(b int64) string {
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case b >= gib:
		return fmt.Sprintf("%.2f GiB", float64(b)/float64(gib))
	case b >= mib:
		return fmt.Sprintf("%.2f MiB", float64(b)/float64(mib))
	case b >= kib:
		return fmt.Sprintf("%.2f KiB", float64(b)/float64(kib))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// InspectCache はキャッシュディレクトリの統計とイメージ一覧を取得する。
func InspectCache(rootDir string) (*CacheStats, error) {
	stats := &CacheStats{}

	// 1. blobs の集計
	blobsDir := filepath.Join(rootDir, "blobs", "sha256")
	if entries, err := os.ReadDir(blobsDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if fi, err := e.Info(); err == nil {
				stats.TotalBlobs++
				stats.TotalBlobBytes += fi.Size()
			}
		}
	}

	// 2. manifests の走査
	manifestsDir := filepath.Join(rootDir, "manifests")
	if _, err := os.Stat(manifestsDir); err == nil {
		_ = filepath.WalkDir(manifestsDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".meta") {
				return nil
			}

			rel, err := filepath.Rel(manifestsDir, path)
			if err != nil {
				return nil
			}

			// 例: docker.io/library/alpine/latest.meta
			parts := strings.Split(rel, string(filepath.Separator))
			if len(parts) < 3 {
				return nil
			}

			registry := parts[0]
			refWithExt := parts[len(parts)-1]
			ref := strings.TrimSuffix(refWithExt, ".meta")
			repository := strings.Join(parts[1:len(parts)-1], "/")

			metaBytes, err := os.ReadFile(path)
			if err != nil {
				return nil
			}

			var meta ManifestMeta
			_ = json.Unmarshal(metaBytes, &meta)

			stats.Images = append(stats.Images, CachedImage{
				Registry:   registry,
				Repository: repository,
				Reference:  ref,
				Digest:     meta.Digest,
				CachedAt:   meta.CachedAt,
			})
			return nil
		})
	}

	return stats, nil
}

// CleanCache はキャッシュされたすべての blob およびマニフェストを削除する。
func CleanCache(rootDir string) error {
	for _, sub := range []string{"blobs", "manifests", "tmp"} {
		dir := filepath.Join(rootDir, sub)
		if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	// blobs/sha256 サブディレクトリを再作成
	_ = os.MkdirAll(filepath.Join(rootDir, "blobs", "sha256"), 0755)
	return nil
}
