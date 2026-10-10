package proxycache

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

// EcosystemStats は各言語エコシステムのキャッシュ統計。
type EcosystemStats struct {
	TotalFiles int   `json:"total_files"`
	TotalBytes int64 `json:"total_bytes"`
}

// CacheStats は Docker およびパッケージキャッシュの全体統計。
type CacheStats struct {
	TotalBlobs     int                       `json:"total_blobs"`
	TotalBlobBytes int64                     `json:"total_blob_bytes"`
	Images         []CachedImage             `json:"images"`
	Packages       map[string]EcosystemStats `json:"packages"`
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
	stats := &CacheStats{
		Packages: make(map[string]EcosystemStats),
	}

	// 1. blobs の集計
	blobsDir := filepath.Join(rootDir, "docker", "blobs", "sha256")
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
	manifestsDir := filepath.Join(rootDir, "docker", "manifests")
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

	// 3. 各言語パッケージの集計 (go, cargo, npm, pypi)
	for _, eco := range []string{"go", "cargo", "npm", "pypi"} {
		ecoDir := filepath.Join(rootDir, eco)
		if _, err := os.Stat(ecoDir); err == nil {
			var es EcosystemStats
			_ = filepath.WalkDir(ecoDir, func(path string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return nil
				}
				if fi, err := d.Info(); err == nil {
					es.TotalFiles++
					es.TotalBytes += fi.Size()
				}
				return nil
			})
			if es.TotalFiles > 0 {
				stats.Packages[eco] = es
			}
		}
	}

	return stats, nil
}

// CleanCache はキャッシュされたすべてのファイル (Docker および言語パッケージ) を削除する。
func CleanCache(rootDir string) error {
	for _, sub := range []string{"docker", "go", "cargo", "npm", "pypi", "tmp"} {
		dir := filepath.Join(rootDir, sub)
		if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	_ = os.MkdirAll(filepath.Join(rootDir, "docker", "blobs", "sha256"), 0755)
	_ = os.MkdirAll(filepath.Join(rootDir, "docker", "manifests"), 0755)
	return nil
}

// CleanEcosystemCache は指定されたエコシステム (docker, go, cargo, npm, pypi) のキャッシュのみを削除する。
func CleanEcosystemCache(rootDir, ecosystem string) error {
	switch ecosystem {
	case "docker":
		dir := filepath.Join(rootDir, "docker")
		_ = os.RemoveAll(dir)
		_ = os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0755)
		_ = os.MkdirAll(filepath.Join(dir, "manifests"), 0755)
		return nil
	case "go", "cargo", "npm", "pypi":
		dir := filepath.Join(rootDir, ecosystem)
		_ = os.RemoveAll(dir)
		_ = os.MkdirAll(dir, 0755)
		return nil
	default:
		return fmt.Errorf("未知のエコシステム: %s (docker, go, cargo, npm, pypi のいずれかを指定してください)", ecosystem)
	}
}
