package dockercache

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
)

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
		// パッケージ名の抽出 (filename から)
		pkg := fn
		if idx := strings.Index(fn, "-"); idx != -1 {
			pkg = fn[:idx]
		}
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

	// 2. キャッシュミス: 承認
	if s.batch != nil {
		if err := s.batch.RequestApproval("pypi", pkgName, filename); err != nil {
			http.Error(w, fmt.Sprintf("Forbidden: %v", err), http.StatusForbidden)
			return
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
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
}
