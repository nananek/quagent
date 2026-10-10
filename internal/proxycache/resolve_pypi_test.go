package proxycache

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParsePyPIDependency(t *testing.T) {
	tests := []struct {
		input       string
		wantPkg     string
		wantIsExtra bool
		wantOK      bool
	}{
		{"requests>=2.20", "requests", false, true},
		{"urllib3<3,>=1.21.1", "urllib3", false, true},
		{"charset-normalizer<4,>=2", "charset-normalizer", false, true},
		{"async_lru>=1.0.0", "async-lru", false, true},
		{"ipykernel>=6.5.0; sys_platform != 'win32'", "ipykernel", false, true},
		{"pytest; extra == 'test'", "", true, false},
		{`cryptography>=3.3.2; (python_version < '3.8') and (extra == 'socks')`, "", true, false},
		{"", "", false, false},
	}

	for _, tt := range tests {
		pkg, isExtra, ok := parsePyPIDependency(tt.input)
		if pkg != tt.wantPkg || isExtra != tt.wantIsExtra || ok != tt.wantOK {
			t.Errorf("parsePyPIDependency(%q) = (%q, %v, %v), want (%q, %v, %v)",
				tt.input, pkg, isExtra, ok, tt.wantPkg, tt.wantIsExtra, tt.wantOK)
		}
	}
}

func TestPyPIResolver_ResolveDependencies(t *testing.T) {
	// モック PyPI JSON API サーバー
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/pypi/jupyterlab/json":
			_ = json.NewEncoder(w).Encode(pypiJSONResponse{
				Info: struct {
					Name         string   `json:"name"`
					Version      string   `json:"version"`
					RequiresDist []string `json:"requires_dist"`
				}{
					Name:    "jupyterlab",
					Version: "4.6.4",
					RequiresDist: []string{
						"async-lru>=1.0.0",
						"ipykernel>=6.5.0",
						"pytest; extra == 'test'",
					},
				},
			})
		case "/pypi/async-lru/json":
			_ = json.NewEncoder(w).Encode(pypiJSONResponse{
				Info: struct {
					Name         string   `json:"name"`
					Version      string   `json:"version"`
					RequiresDist []string `json:"requires_dist"`
				}{
					Name:    "async-lru",
					Version: "2.0.4",
					RequiresDist: []string{
						"typing-extensions>=4.0.0",
					},
				},
			})
		case "/pypi/ipykernel/json":
			_ = json.NewEncoder(w).Encode(pypiJSONResponse{
				Info: struct {
					Name         string   `json:"name"`
					Version      string   `json:"version"`
					RequiresDist []string `json:"requires_dist"`
				}{
					Name:    "ipykernel",
					Version: "6.29.5",
					RequiresDist: []string{
						"traitlets>=5.4.0",
						"jupyterlab", // 循環参照テスト
					},
				},
			})
		case "/pypi/typing-extensions/json":
			_ = json.NewEncoder(w).Encode(pypiJSONResponse{
				Info: struct {
					Name         string   `json:"name"`
					Version      string   `json:"version"`
					RequiresDist []string `json:"requires_dist"`
				}{
					Name:         "typing-extensions",
					Version:      "4.12.2",
					RequiresDist: nil,
				},
			})
		case "/pypi/traitlets/json":
			_ = json.NewEncoder(w).Encode(pypiJSONResponse{
				Info: struct {
					Name         string   `json:"name"`
					Version      string   `json:"version"`
					RequiresDist []string `json:"requires_dist"`
				}{
					Name:         "traitlets",
					Version:      "5.14.3",
					RequiresDist: nil,
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	resolver := NewPyPIResolver(ts.Client())
	resolver.BaseURL = ts.URL

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	items, err := resolver.ResolveDependencies(ctx, "jupyterlab", "4.6.4", 50)
	if err != nil {
		t.Fatalf("ResolveDependencies failed: %v", err)
	}

	// 期待されるパッケージ: jupyterlab, async-lru, ipykernel, typing-extensions, traitlets (計5個)
	itemMap := make(map[string]bool)
	for _, it := range items {
		itemMap[it.Name] = true
	}

	expected := []string{"jupyterlab", "async-lru", "ipykernel", "typing-extensions", "traitlets"}
	if len(items) != len(expected) {
		t.Errorf("expected %d items, got %d: %+v", len(expected), len(items), items)
	}

	for _, exp := range expected {
		if !itemMap[exp] {
			t.Errorf("expected item %s not found in resolved items", exp)
		}
	}

	// extra依存の pytest は含まれていないこと
	if itemMap["pytest"] {
		t.Errorf("extra dependency pytest should not be included")
	}
}
