// Package authproxy は guest の LLM API 呼び出しに host 側で秘密を付けて転送する。
// 秘密は host のプロセスから出ず、guest には API の到達経路も与えない。
package authproxy

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"

	"github.com/nananek/quagent/internal/config"
)

// Prefix は窓口上のルートの前置き。guest からは http://quagent.host/llm/<id>/... で使う。
const Prefix = "/llm/"

// guest が送ってきても捨てる認証系ヘッダ。
var strippedHeaders = []string{"Authorization", "X-Api-Key", "Api-Key", "Proxy-Authorization", "Cookie"}

// Register は providers の転送ルートを mux に登録し、登録した provider ID を返す。
// 秘密は起動時に一度だけ取り出す (secret_command の対話を run 開始時に済ませるため)。
func Register(mux *http.ServeMux, providers map[string]config.Provider, logger *log.Logger) ([]string, error) {
	var ids []string
	for id, p := range providers {
		up, err := url.Parse(p.Upstream)
		if err != nil || up.Scheme != "https" || up.Host == "" {
			return nil, fmt.Errorf("provider %s: upstream は https の URL にする: %q", id, p.Upstream)
		}
		secret, err := p.Secret()
		if err != nil {
			return nil, fmt.Errorf("provider %s: 秘密を取り出せない: %w", id, err)
		}
		mux.Handle(Prefix+id+"/", handler(id, up, p.HeaderName(), p.HeaderPrefix()+secret, logger))
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func handler(id string, up *url.URL, header, value string, logger *log.Logger) http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			rest := strings.TrimPrefix(r.In.URL.Path, Prefix+id)
			r.Out.URL.Scheme = up.Scheme
			r.Out.URL.Host = up.Host
			r.Out.URL.Path = strings.TrimSuffix(up.Path, "/") + rest
			r.Out.URL.RawPath = ""
			r.Out.Host = up.Host
			for _, h := range strippedHeaders {
				r.Out.Header.Del(h)
			}
			r.Out.Header.Set(header, value)
		},
		// ストリーミング応答 (SSE) をそのまま流す
		FlushInterval: -1,
		ErrorLog:      logger,
		ModifyResponse: func(resp *http.Response) error {
			logger.Printf("llm %s %s %s -> %d", id, resp.Request.Method, resp.Request.URL.Path, resp.StatusCode)
			return nil
		},
	}
	return rp
}

// GuestBaseURL は guest に設定する provider の baseURL を返す。
func GuestBaseURL(guestHost, id string) string {
	return "http://" + guestHost + Prefix + id
}
