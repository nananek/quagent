package proxycache

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type npmPackument struct {
	Versions map[string]struct {
		Dependencies map[string]string `json:"dependencies"`
	} `json:"versions"`
	DistTags map[string]string `json:"dist-tags"`
}

// NPMResolver は npm registry API を用いて依存関係ツリーを事前解決する。
type NPMResolver struct {
	Client  *http.Client
	BaseURL string // テスト用 (既定: "https://registry.npmjs.org")
	// Logf は上流へ問い合わせるパッケージ名を記録する (nil なら記録しない)。
	Logf func(string)

	cacheMu sync.RWMutex
	cache   map[string][]string // pkgName -> direct dependencies
}

// NewNPMResolver は新しい NPMResolver を作成する。
func NewNPMResolver(client *http.Client) *NPMResolver {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &NPMResolver{
		Client:  client,
		BaseURL: "https://registry.npmjs.org",
		cache:   make(map[string][]string),
	}
}

// ResolveDependencies はルートパッケージとそのバージョンから依存関係を再帰走査する。
func (r *NPMResolver) ResolveDependencies(ctx context.Context, rootPkg, rootVersion string, maxItems int) ([]BatchItem, error) {
	if maxItems <= 0 {
		maxItems = 200
	}

	rootNorm := strings.ToLower(strings.TrimSpace(rootPkg))
	visited := make(map[string]bool)
	visited[rootNorm] = true

	var items []BatchItem
	items = append(items, BatchItem{
		Ecosystem: "npm",
		Name:      rootPkg,
		Version:   rootVersion,
	})

	queue := []struct {
		pkg string
		ver string
	}{{pkg: rootNorm, ver: rootVersion}}

	for len(queue) > 0 && len(items) < maxItems {
		current := queue[0]
		queue = queue[1:]

		deps, err := r.fetchDirectDeps(ctx, current.pkg, current.ver)
		if err != nil {
			continue
		}

		for _, dep := range deps {
			if len(items) >= maxItems {
				break
			}
			depNorm := strings.ToLower(strings.TrimSpace(dep))
			if !visited[depNorm] {
				visited[depNorm] = true
				items = append(items, BatchItem{
					Ecosystem: "npm",
					Name:      dep,
					Version:   "",
				})
				queue = append(queue, struct {
					pkg string
					ver string
				}{pkg: depNorm, ver: ""})
			}
		}
	}

	return items, nil
}

func (r *NPMResolver) fetchDirectDeps(ctx context.Context, pkg, version string) ([]string, error) {
	norm := strings.ToLower(strings.TrimSpace(pkg))

	r.cacheMu.RLock()
	cached, ok := r.cache[norm]
	r.cacheMu.RUnlock()
	if ok {
		return cached, nil
	}

	if r.Logf != nil {
		r.Logf(norm)
	}

	url := fmt.Sprintf("%s/%s", r.BaseURL, norm)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := r.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024)) // 2MB 制限
	if err != nil {
		return nil, err
	}

	var data npmPackument
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}

	// 対象バージョンを決定
	targetVer := version
	if targetVer == "" || targetVer == "latest" {
		if latest, ok := data.DistTags["latest"]; ok {
			targetVer = latest
		} else {
			for v := range data.Versions {
				targetVer = v
				break
			}
		}
	}

	var deps []string
	if verData, ok := data.Versions[targetVer]; ok {
		for dep := range verData.Dependencies {
			deps = append(deps, dep)
		}
	} else {
		// 見つからない場合は任意のバージョンから取得
		for _, verData := range data.Versions {
			for dep := range verData.Dependencies {
				deps = append(deps, dep)
			}
			break
		}
	}

	r.cacheMu.Lock()
	r.cache[norm] = deps
	r.cacheMu.Unlock()

	return deps, nil
}
