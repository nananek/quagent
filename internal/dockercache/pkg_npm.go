package dockercache

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func parseNPMPath(p string) (action, relPath, pkgName, version string, ok bool) {
	p = strings.TrimPrefix(p, "/npm/")
	p = strings.TrimPrefix(p, "/npm")
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "", "", "", "", false
	}

	unescaped, err := url.PathUnescape(p)
	if err == nil {
		p = unescaped
	}

	// tarball: <pkg...>/-/<pkg>-<version>.tgz
	if idx := strings.Index(p, "/-/"); idx != -1 {
		pkgName = p[:idx]
		filename := p[idx+3:]
		// バージョン抽出
		v := strings.TrimSuffix(filename, ".tgz")
		lastDash := strings.LastIndex(v, "-")
		if lastDash != -1 {
			version = v[lastDash+1:]
		}
		return "tarball", p, pkgName, version, true
	}

	// packument メタデータ: <pkg...>
	return "metadata", p, p, "", true
}

func (s *Server) handleNPM(w http.ResponseWriter, r *http.Request) {
	action, relPath, pkgName, version, ok := parseNPMPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	switch action {
	case "metadata":
		s.handleNPMMetadata(w, r, relPath)
	case "tarball":
		s.handleNPMTarball(w, r, relPath, pkgName, version)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleNPMMetadata(w http.ResponseWriter, r *http.Request, relPath string) {
	cacheKey := "metadata/" + relPath + ".json"

	// 上流から最新メタデータを取得
	upstreamURL := "https://registry.npmjs.org/" + relPath
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// npm の標準 Accept ヘッダ
	req.Header.Set("Accept", "application/vnd.npm.install-v1+json; q=1.0, application/json; q=0.8, */*")

	resp, err := s.client.Do(req)
	if err != nil {
		// キャッシュがあればフォールバック
		if data, err := s.storage.GetPkgFile("npm", cacheKey); err == nil {
			w.Header().Set("Content-Type", "application/json")
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

	// tarball URL をローカルプロキシに置換
	// https://registry.npmjs.org/ -> http://127.0.0.1:5000/npm/
	rewritten := bytes.ReplaceAll(data, []byte("https://registry.npmjs.org/"), []byte("http://127.0.0.1:5000/npm/"))

	_, _ = s.storage.SavePkgFile("npm", cacheKey, bytes.NewReader(rewritten))

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(rewritten)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(rewritten)
}

func (s *Server) handleNPMTarball(w http.ResponseWriter, r *http.Request, relPath, pkgName, version string) {
	cacheKey := "tarballs/" + relPath

	// 1. キャッシュヒット確認
	if has, size := s.storage.HasPkgFile("npm", cacheKey); has {
		f, _, err := s.storage.OpenPkgFile("npm", cacheKey)
		if err == nil {
			defer f.Close()
			w.Header().Set("Content-Type", "application/octet-stream")
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
		if !s.batch.IsApproved("npm", pkgName) {
			resolver := NewNPMResolver(s.client)
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			items, err := resolver.ResolveDependencies(ctx, pkgName, version, 200)
			if err != nil || len(items) == 0 {
				items = []BatchItem{{Ecosystem: "npm", Name: pkgName, Version: version}}
			}
			if err := s.batch.RequestApprovalItems("npm", items); err != nil {
				http.Error(w, fmt.Sprintf("Forbidden: %v", err), http.StatusForbidden)
				return
			}
		}
	}

	// 3. 上流からダウンロード
	upstreamURL := "https://registry.npmjs.org/" + relPath
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

	size, err := s.storage.SavePkgFile("npm", cacheKey, resp.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("Save error: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		f, _, err := s.storage.OpenPkgFile("npm", cacheKey)
		if err == nil {
			defer f.Close()
			_, _ = io.Copy(w, f)
		}
	}
}
