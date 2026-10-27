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

// normalizePyPIName は PyPI の仕様 (PEP 503) に従いパッケージ名を正規化する。
func normalizePyPIName(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.ReplaceAll(n, "_", "-")
	n = strings.ReplaceAll(n, ".", "-")
	return n
}

// parsePyPIDependency は PEP 508 依存文字列からパッケージ名と extra フラグを抽出する。
func parsePyPIDependency(dep string) (pkgName string, isExtra bool, ok bool) {
	dep = strings.TrimSpace(dep)
	if dep == "" {
		return "", false, false
	}

	var marker string
	mainPart := dep
	if idx := strings.Index(dep, ";"); idx != -1 {
		mainPart = strings.TrimSpace(dep[:idx])
		marker = strings.TrimSpace(dep[idx+1:])
	}

	// extra 条件付き依存 (オプション機能用) は通常インストール対象外としてスキップ
	if strings.Contains(marker, "extra ==") || strings.Contains(marker, "extra==") ||
		strings.Contains(marker, `extra == "`) || strings.Contains(marker, `extra == '`) {
		return "", true, false
	}

	// パッケージ名を抽出 (<>=!~;[ ( の直前まで)
	nameIdx := strings.IndexAny(mainPart, "<>=!~;[ (")
	name := mainPart
	if nameIdx != -1 {
		name = mainPart[:nameIdx]
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false, false
	}

	return normalizePyPIName(name), false, true
}

type pypiJSONResponse struct {
	Info struct {
		Name         string   `json:"name"`
		Version      string   `json:"version"`
		RequiresDist []string `json:"requires_dist"`
	} `json:"info"`
}

// PyPIResolver は PyPI のメタデータ API を用いて依存関係ツリーを事前解決する。
type PyPIResolver struct {
	Client  *http.Client
	BaseURL string // テスト用 (既定: "https://pypi.org")
	// Logf は上流へ問い合わせるパッケージ名を記録する (nil なら記録しない)。
	Logf func(string)

	cacheMu sync.RWMutex
	cache   map[string][]string // pkgName -> direct dependencies
}

// NewPyPIResolver は新しい PyPIResolver を作成する。
func NewPyPIResolver(client *http.Client) *PyPIResolver {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &PyPIResolver{
		Client:  client,
		BaseURL: "https://pypi.org",
		cache:   make(map[string][]string),
	}
}

// ResolveDependencies はルートパッケージとそのバージョンから、
// 再帰的に依存関係を走査し BatchItem のリストを返す。
func (r *PyPIResolver) ResolveDependencies(ctx context.Context, rootPkg, rootVersion string, maxItems int) ([]BatchItem, error) {
	if maxItems <= 0 {
		maxItems = 200
	}

	rootNorm := normalizePyPIName(rootPkg)
	visited := make(map[string]bool)
	visited[rootNorm] = true

	var items []BatchItem
	items = append(items, BatchItem{
		Ecosystem: "pypi",
		Name:      rootPkg,
		Version:   rootVersion,
	})

	queue := []string{rootNorm}

	// 並行ワーカーで依存関係をフェッチ
	type fetchJob struct {
		pkg string
	}
	type fetchResult struct {
		pkg  string
		deps []string
		err  error
	}

	for len(queue) > 0 && len(items) < maxItems {
		// 現在の階層のパッケージ群を並行取得
		currentBatch := queue
		queue = nil

		jobs := make(chan fetchJob, len(currentBatch))
		results := make(chan fetchResult, len(currentBatch))

		workerCount := min(8, len(currentBatch))
		var wg sync.WaitGroup
		for i := 0; i < workerCount; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for job := range jobs {
					select {
					case <-ctx.Done():
						results <- fetchResult{pkg: job.pkg, err: ctx.Err()}
						return
					default:
					}
					deps, err := r.fetchDirectDeps(ctx, job.pkg)
					results <- fetchResult{pkg: job.pkg, deps: deps, err: err}
				}
			}()
		}

		for _, p := range currentBatch {
			jobs <- fetchJob{pkg: p}
		}
		close(jobs)
		wg.Wait()
		close(results)

		for res := range results {
			if res.err != nil {
				// フェッチ失敗は無視して進める (古いパッケージや sdist 等)
				continue
			}
			for _, dep := range res.deps {
				if len(items) >= maxItems {
					break
				}
				if !visited[dep] {
					visited[dep] = true
					items = append(items, BatchItem{
						Ecosystem: "pypi",
						Name:      dep,
						Version:   "",
					})
					queue = append(queue, dep)
				}
			}
		}
	}

	return items, nil
}

func (r *PyPIResolver) fetchDirectDeps(ctx context.Context, pkg string) ([]string, error) {
	norm := normalizePyPIName(pkg)

	r.cacheMu.RLock()
	cached, ok := r.cache[norm]
	r.cacheMu.RUnlock()
	if ok {
		return cached, nil
	}

	if r.Logf != nil {
		r.Logf(norm)
	}

	url := fmt.Sprintf("%s/pypi/%s/json", r.BaseURL, norm)
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

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024)) // 1MB 制限
	if err != nil {
		return nil, err
	}

	var data pypiJSONResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}

	var deps []string
	seen := make(map[string]bool)
	for _, reqDist := range data.Info.RequiresDist {
		depPkg, isExtra, ok := parsePyPIDependency(reqDist)
		if !ok || isExtra || depPkg == "" || depPkg == norm {
			continue
		}
		if !seen[depPkg] {
			seen[depPkg] = true
			deps = append(deps, depPkg)
		}
	}

	r.cacheMu.Lock()
	r.cache[norm] = deps
	r.cacheMu.Unlock()

	return deps, nil
}
