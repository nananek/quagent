package dockercache

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNPMResolver_ResolveDependencies(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/express":
			_ = json.NewEncoder(w).Encode(npmPackument{
				DistTags: map[string]string{"latest": "4.18.2"},
				Versions: map[string]struct {
					Dependencies map[string]string `json:"dependencies"`
				}{
					"4.18.2": {
						Dependencies: map[string]string{
							"accepts":     "~1.3.8",
							"body-parser": "1.20.1",
						},
					},
				},
			})
		case "/accepts":
			_ = json.NewEncoder(w).Encode(npmPackument{
				DistTags: map[string]string{"latest": "1.3.8"},
				Versions: map[string]struct {
					Dependencies map[string]string `json:"dependencies"`
				}{
					"1.3.8": {
						Dependencies: map[string]string{
							"mime-types": "~2.1.34",
						},
					},
				},
			})
		case "/body-parser":
			_ = json.NewEncoder(w).Encode(npmPackument{
				DistTags: map[string]string{"latest": "1.20.1"},
				Versions: map[string]struct {
					Dependencies map[string]string `json:"dependencies"`
				}{
					"1.20.1": {
						Dependencies: map[string]string{
							"bytes": "3.1.2",
						},
					},
				},
			})
		case "/mime-types":
			_ = json.NewEncoder(w).Encode(npmPackument{
				DistTags: map[string]string{"latest": "2.1.34"},
				Versions: map[string]struct {
					Dependencies map[string]string `json:"dependencies"`
				}{
					"2.1.34": {
						Dependencies: nil,
					},
				},
			})
		case "/bytes":
			_ = json.NewEncoder(w).Encode(npmPackument{
				DistTags: map[string]string{"latest": "3.1.2"},
				Versions: map[string]struct {
					Dependencies map[string]string `json:"dependencies"`
				}{
					"3.1.2": {
						Dependencies: nil,
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	resolver := NewNPMResolver(ts.Client())
	resolver.BaseURL = ts.URL

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	items, err := resolver.ResolveDependencies(ctx, "express", "4.18.2", 50)
	if err != nil {
		t.Fatalf("ResolveDependencies failed: %v", err)
	}

	itemMap := make(map[string]bool)
	for _, it := range items {
		itemMap[it.Name] = true
	}

	expected := []string{"express", "accepts", "body-parser", "mime-types", "bytes"}
	if len(items) != len(expected) {
		t.Errorf("expected %d items, got %d: %+v", len(expected), len(items), items)
	}

	for _, exp := range expected {
		if !itemMap[exp] {
			t.Errorf("expected item %s not found in resolved items", exp)
		}
	}
}
