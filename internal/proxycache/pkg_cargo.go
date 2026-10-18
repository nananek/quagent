package proxycache

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/nananek/quagent/internal/guest"
)

func parseCargoPath(p string) (action, relPath, crate, version string, ok bool) {
	p = strings.TrimPrefix(p, "/cargo/")
	p = strings.TrimPrefix(p, "/cargo")
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "", "", "", "", false
	}
	if !safePkgRelPath(p) {
		return "", "", "", "", false
	}

	if strings.HasPrefix(p, "index/") {
		rel := strings.TrimPrefix(p, "index/")
		return "index", rel, "", "", true
	} else if strings.HasPrefix(p, "crates/") {
		rel := strings.TrimPrefix(p, "crates/")
		// <crate>/<crate>-<version>.crate
		parts := strings.Split(rel, "/")
		if len(parts) >= 2 {
			crate = parts[0]
			filename := parts[len(parts)-1]
			filename = strings.TrimSuffix(filename, ".crate")
			if strings.HasPrefix(filename, crate+"-") {
				version = strings.TrimPrefix(filename, crate+"-")
			}
		}
		return "crates", rel, crate, version, true
	}

	return "", "", "", "", false
}

func (s *Server) handleCargo(w http.ResponseWriter, r *http.Request) {
	action, relPath, crate, version, ok := parseCargoPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	switch action {
	case "index":
		s.handleCargoIndex(w, r, relPath)
	case "crates":
		s.handleCargoCrates(w, r, relPath, crate, version)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleCargoIndex(w http.ResponseWriter, r *http.Request, relPath string) {
	cacheKey := "index/" + relPath

	// config.json の特別処理
	if relPath == "config.json" {
		upstreamURL := "https://index.crates.io/config.json"
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp, err := s.client.Do(req)
		if err != nil {
			// キャッシュがあればフォールバック
			if data, err := s.storage.GetPkgFile("cargo", cacheKey); err == nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(data)
				return
			}
			http.Error(w, fmt.Sprintf("Upstream error: %v", err), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		// エラー応答はキャッシュせず、キャッシュがあればそれを返す。
		if resp.StatusCode != http.StatusOK {
			if data, err := s.storage.GetPkgFile("cargo", cacheKey); err == nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(data)
				return
			}
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, resp.Body)
			return
		}

		data, err := io.ReadAll(resp.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// dl URL をローカルプロキシに書き換え
		var cfg map[string]any
		if err := json.Unmarshal(data, &cfg); err == nil {
			cfg["dl"] = "http://" + guest.GuestProxyCacheAddr + "/cargo/crates/{crate}/{crate}-{version}.crate"
			if rewritten, err := json.Marshal(cfg); err == nil {
				data = rewritten
			}
		}

		_, _ = s.storage.SavePkgFile("cargo", cacheKey, bytes.NewReader(data))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}

	// 一般インデックスファイル (JSON Lines)
	if has, size := s.storage.HasPkgFile("cargo", cacheKey); has {
		f, _, err := s.storage.OpenPkgFile("cargo", cacheKey)
		if err == nil {
			defer f.Close()
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				_, _ = io.Copy(w, f)
			}
			return
		}
	}

	upstreamURL := "https://index.crates.io/" + relPath
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

	size, err := s.storage.SavePkgFile("cargo", cacheKey, resp.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("Save error: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		f, _, err := s.storage.OpenPkgFile("cargo", cacheKey)
		if err == nil {
			defer f.Close()
			_, _ = io.Copy(w, f)
		}
	}
}

func (s *Server) handleCargoCrates(w http.ResponseWriter, r *http.Request, relPath, crate, version string) {
	cacheKey := "crates/" + relPath

	// 1. キャッシュヒット確認
	if has, size := s.storage.HasPkgFile("cargo", cacheKey); has {
		f, _, err := s.storage.OpenPkgFile("cargo", cacheKey)
		if err == nil {
			defer f.Close()
			w.Header().Set("Content-Type", "application/x-tar")
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
		if err := s.batch.RequestApproval("cargo", crate, version); err != nil {
			http.Error(w, fmt.Sprintf("Forbidden: %v", err), http.StatusForbidden)
			return
		}
	}

	// 3. 上流からダウンロード (static.crates.io)
	upstreamURL := "https://static.crates.io/crates/" + relPath
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

	size, err := s.storage.SavePkgFile("cargo", cacheKey, resp.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("Save error: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		f, _, err := s.storage.OpenPkgFile("cargo", cacheKey)
		if err == nil {
			defer f.Close()
			_, _ = io.Copy(w, f)
		}
	}
}
