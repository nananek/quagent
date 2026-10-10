package dockercache

import (
	"fmt"
	"io"
	"net/http"
	"strings"
)

// parseGoPath は /go/<module>/... を解析する。
func parseGoPath(p string) (relPath, module, version string, ok bool) {
	p = strings.TrimPrefix(p, "/go/")
	p = strings.TrimPrefix(p, "/go")
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "", "", "", false
	}

	relPath = p
	if idx := strings.Index(p, "/@v/"); idx != -1 {
		module = p[:idx]
		rest := p[idx+4:]
		if rest == "list" {
			version = ""
		} else {
			// <version>.<ext>
			extIdx := strings.LastIndex(rest, ".")
			if extIdx != -1 {
				version = rest[:extIdx]
			} else {
				version = rest
			}
		}
		return relPath, module, version, true
	} else if strings.HasSuffix(p, "/@latest") {
		module = strings.TrimSuffix(p, "/@latest")
		return relPath, module, "latest", true
	}

	return "", "", "", false
}

func (s *Server) handleGo(w http.ResponseWriter, r *http.Request) {
	relPath, module, version, ok := parseGoPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	// 1. キャッシュヒット確認
	if has, size := s.storage.HasPkgFile("go", relPath); has {
		f, _, err := s.storage.OpenPkgFile("go", relPath)
		if err == nil {
			defer f.Close()
			setGoContentType(w, relPath)
			w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				_, _ = io.Copy(w, f)
			}
			return
		}
	}

	// 2. キャッシュミス: バッチ承認
	if s.batch != nil {
		if err := s.batch.RequestApproval("go", module, version); err != nil {
			http.Error(w, fmt.Sprintf("Forbidden: %v", err), http.StatusForbidden)
			return
		}
	}

	// 3. 上流からダウンロード
	upstreamURL := "https://proxy.golang.org/" + relPath
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

	// キャッシュに保存
	size, err := s.storage.SavePkgFile("go", relPath, resp.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("Save error: %v", err), http.StatusInternalServerError)
		return
	}

	setGoContentType(w, relPath)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		f, _, err := s.storage.OpenPkgFile("go", relPath)
		if err == nil {
			defer f.Close()
			_, _ = io.Copy(w, f)
		}
	}
}

func setGoContentType(w http.ResponseWriter, relPath string) {
	switch {
	case strings.HasSuffix(relPath, ".zip"):
		w.Header().Set("Content-Type", "application/zip")
	case strings.HasSuffix(relPath, ".info") || strings.HasSuffix(relPath, "@latest"):
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
}
