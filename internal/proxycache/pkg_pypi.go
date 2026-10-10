package proxycache

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func parsePyPIPackageFilename(fn string) (pkgName, version string) {
	stem := strings.TrimSuffix(fn, ".metadata")
	if strings.HasSuffix(stem, ".whl") {
		stem = strings.TrimSuffix(stem, ".whl")
		parts := strings.Split(stem, "-")
		pkgName = parts[0]
		if len(parts) >= 2 {
			version = parts[1]
		}
		return pkgName, version
	}

	for _, ext := range []string{".tar.gz", ".tar.bz2", ".tar.xz", ".zip", ".tgz"} {
		if strings.HasSuffix(stem, ext) {
			stem = strings.TrimSuffix(stem, ext)
			break
		}
	}

	if lastDash := strings.LastIndex(stem, "-"); lastDash != -1 {
		pkgName = stem[:lastDash]
		version = stem[lastDash+1:]
		return pkgName, version
	}

	return stem, ""
}

func parsePyPIPath(p string) (action, relPath, pkgName, filename string, ok bool) {
	p = strings.TrimPrefix(p, "/pypi/")
	p = strings.TrimPrefix(p, "/pypi")
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "", "", "", "", false
	}

	if strings.HasPrefix(p, "simple/") {
		rel := strings.TrimPrefix(p, "simple/")
		pkg := strings.TrimSuffix(rel, "/")
		return "simple", pkg, pkg, "", true
	} else if strings.HasPrefix(p, "packages/") {
		rel := strings.TrimPrefix(p, "packages/")
		parts := strings.Split(rel, "/")
		fn := parts[len(parts)-1]
		pkg, _ := parsePyPIPackageFilename(fn)
		return "packages", rel, pkg, fn, true
	}

	return "", "", "", "", false
}

func (s *Server) handlePyPI(w http.ResponseWriter, r *http.Request) {
	action, relPath, pkgName, filename, ok := parsePyPIPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	switch action {
	case "simple":
		s.handlePyPISimple(w, r, relPath)
	case "packages":
		s.handlePyPIPackages(w, r, relPath, pkgName, filename)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handlePyPISimple(w http.ResponseWriter, r *http.Request, pkg string) {
	cacheKey := "simple/" + pkg + "/index.html"

	upstreamURL := "https://pypi.org/simple/" + pkg + "/"
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// pip の Accept ヘッダを引き継ぐ (PEP 691 JSON または HTML)
	if accept := r.Header.Get("Accept"); accept != "" {
		req.Header.Set("Accept", accept)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		// キャッシュがあればフォールバック
		if data, err := s.storage.GetPkgFile("pypi", cacheKey); err == nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}
		http.Error(w, fmt.Sprintf("Upstream error: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// リンク先をローカルプロキシに置換
	// https://files.pythonhosted.org/packages/ -> http://127.0.0.1:5000/pypi/packages/
	rewritten := bytes.ReplaceAll(data, []byte("https://files.pythonhosted.org/packages/"), []byte("http://127.0.0.1:5000/pypi/packages/"))

	_, _ = s.storage.SavePkgFile("pypi", cacheKey, bytes.NewReader(rewritten))

	cType := resp.Header.Get("Content-Type")
	if cType == "" {
		cType = "text/html; charset=utf-8"
	}
	w.Header().Set("Content-Type", cType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(rewritten)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(rewritten)
}

func (s *Server) handlePyPIPackages(w http.ResponseWriter, r *http.Request, relPath, pkgName, filename string) {
	cacheKey := "packages/" + relPath

	// 1. キャッシュヒット確認
	if has, size := s.storage.HasPkgFile("pypi", cacheKey); has {
		f, _, err := s.storage.OpenPkgFile("pypi", cacheKey)
		if err == nil {
			defer f.Close()
			setPyPIContentType(w, filename)
			w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				_, _ = io.Copy(w, f)
			}
			return
		}
	}

	// PEP 658 の .metadata (METADATA ファイル) は依存関係解決用のテキストメタデータであり、
	// 実行可能コードを含まないため Simple API と同様に承認不要で中継・キャッシュする。
	isMetadata := strings.HasSuffix(filename, ".metadata")

	// 2. キャッシュミス: 承認 (メタデータ以外)
	if !isMetadata && s.batch != nil {
		_, version := parsePyPIPackageFilename(filename)
		displayVer := version
		if displayVer == "" {
			displayVer = filename
		}

		if !s.batch.IsApproved("pypi", pkgName) {
			resolver := NewPyPIResolver(s.client)
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			items, err := resolver.ResolveDependencies(ctx, pkgName, displayVer, 200)
			if err != nil || len(items) == 0 {
				items = []BatchItem{{Ecosystem: "pypi", Name: pkgName, Version: displayVer}}
			}
			if err := s.batch.RequestApprovalItems("pypi", items); err != nil {
				http.Error(w, fmt.Sprintf("Forbidden: %v", err), http.StatusForbidden)
				return
			}
		}
	}

	// 3. 上流からダウンロード (files.pythonhosted.org)
	upstreamURL := "https://files.pythonhosted.org/packages/" + relPath
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp, err := s.client.Do(req)
	if err != nil {
		http.Error(w, fmt.Sprintf("Upstream error: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	size, err := s.storage.SavePkgFile("pypi", cacheKey, resp.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("Save error: %v", err), http.StatusInternalServerError)
		return
	}

	setPyPIContentType(w, filename)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		f, _, err := s.storage.OpenPkgFile("pypi", cacheKey)
		if err == nil {
			defer f.Close()
			_, _ = io.Copy(w, f)
		}
	}
}

func setPyPIContentType(w http.ResponseWriter, filename string) {
	switch {
	case strings.HasSuffix(filename, ".whl"):
		w.Header().Set("Content-Type", "application/x-wheel+zip")
	case strings.HasSuffix(filename, ".tar.gz"):
		w.Header().Set("Content-Type", "application/gzip")
	case strings.HasSuffix(filename, ".zip"):
		w.Header().Set("Content-Type", "application/zip")
	case strings.HasSuffix(filename, ".metadata"):
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
}
