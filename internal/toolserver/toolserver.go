// Package toolserver は OpenAPI で公開された外部のツールサーバー (Open WebUI の
// ツールサーバーなど) を MCP のツールに変換し、guest のエージェントに使わせる。
//
// OpenAPI 仕様の取得も、ツールの呼び出しの転送も host が行う。guest はツールサーバー
// へ直接つながず (egress は塞いだまま)、認証の秘密も guest には渡らない。認証が
// 要らないサーバーは秘密の設定を省略すればそのまま使える。
package toolserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nananek/quagent/internal/config"
	"github.com/nananek/quagent/internal/guard"
)

const (
	// NameSep はサーバー名とツール名 (operationId) の区切り。
	NameSep = "__"
	// maxSpec は OpenAPI 仕様として読む上限。
	maxSpec = 8 << 20
	// maxResponse はツールの応答としてエージェントに返す本文の上限。
	maxResponse = 1 << 20
	// maxToolName は MCP クライアントが共通して受け付ける名前の長さ。
	maxToolName = 64
	// maxSchemaDepth は $ref を展開するときの深さの上限 (循環参照の保険)。
	maxSchemaDepth = 12

	specTimeout = 15 * time.Second
	callTimeout = 60 * time.Second
)

// guest が指定しても付けないヘッダ (認証や接続まわりは host が決める)。
var forbiddenHeaders = map[string]bool{
	"host": true, "cookie": true, "content-length": true, "content-type": true,
	"authorization": true, "proxy-authorization": true, "connection": true,
	"transfer-encoding": true, "accept-encoding": true,
}

// param は OpenAPI の parameter 1 つ。
type param struct {
	name     string
	in       string // path / query / header
	required bool
	explode  bool // query の配列を name=a&name=b にするか (既定 true)
}

// operation は OpenAPI の operation 1 つ (ツール 1 つ分)。
type operation struct {
	toolName    string
	description string
	method      string
	path        string
	params      []param
	hasBody     bool
	bodyKey     string // 本文を受ける引数名 (既定 "body")
	bodyJSON    bool   // false なら application/x-www-form-urlencoded
	bodyReq     bool
	schema      map[string]any
}

// Server は 1 台のツールサーバー (変換済み)。
type Server struct {
	name   string
	cfg    config.ToolServer
	base   string // 末尾の / を除いた base URL
	host   string
	ops    map[string]*operation
	client *http.Client
	guard  *guard.Guard
	logf   func(string)
}

// Load は cfg の OpenAPI 仕様を取得して MCP のツールに変換する。g が nil でなければ、
// ツール呼び出しの中身もコンテンツガードに通す。logf は承認コンソール向けの記録。
func Load(ctx context.Context, name string, cfg config.ToolServer, g *guard.Guard, logf func(string)) (*Server, error) {
	if name == "" || strings.Contains(name, NameSep) || !validName(name) {
		return nil, fmt.Errorf("tool_servers の名前は英数字・- ・_ だけ ('__' を含まない): %q", name)
	}
	u, err := url.Parse(strings.TrimSpace(cfg.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("tool_servers.%s: url は http(s) の URL にする: %q", name, cfg.URL)
	}
	u.RawQuery, u.Fragment = "", ""
	s := &Server{
		name: name, cfg: cfg, base: strings.TrimRight(u.String(), "/"), host: u.Host,
		client: &http.Client{
			Timeout: callTimeout,
			// 転送先を勝手に変えさせない (リダイレクト先に認証ヘッダを送らない)
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		guard: g, logf: logf,
	}
	if s.logf == nil {
		s.logf = func(string) {}
	}
	specPath := cfg.OpenAPIPath
	if specPath == "" {
		specPath = "/openapi.json"
	}
	if !strings.HasPrefix(specPath, "/") {
		specPath = "/" + specPath
	}
	spec, err := s.fetchSpec(ctx, s.base+specPath)
	if err != nil {
		return nil, fmt.Errorf("tool_servers.%s: OpenAPI 仕様を取得できない: %w", name, err)
	}
	if s.ops, err = convert(name, spec); err != nil {
		return nil, fmt.Errorf("tool_servers.%s: %w", name, err)
	}
	return s, nil
}

func validName(s string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func (s *Server) auth() (header, value string, err error) {
	v, err := s.cfg.AuthValue()
	if err != nil || v == "" {
		return "", "", err
	}
	return s.cfg.HeaderName(), v, nil
}

func (s *Server) fetchSpec(ctx context.Context, specURL string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, specTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, specURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	h, v, err := s.auth()
	if err != nil {
		return nil, fmt.Errorf("秘密を取り出せない: %w", err)
	}
	if h != "" {
		req.Header.Set(h, v)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("HTTP %d (%s)", resp.StatusCode, specURL)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxSpec+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSpec {
		return nil, fmt.Errorf("仕様が大きすぎる (%d バイト超)", maxSpec)
	}
	var spec map[string]any
	if err := json.Unmarshal(b, &spec); err != nil {
		return nil, fmt.Errorf("JSON として読めない (YAML は未対応): %w", err)
	}
	return spec, nil
}

// ToolNames は変換したツール名を並べて返す。
func (s *Server) ToolNames() []string {
	var names []string
	for n := range s.ops {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Register は変換したツールを MCP サーバーに登録する。
func (s *Server) Register(srv *mcp.Server) {
	for _, n := range s.ToolNames() {
		op := s.ops[n]
		srv.AddTool(&mcp.Tool{Name: op.toolName, Description: op.description, InputSchema: op.schema},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return s.call(ctx, op, req.Params.Arguments), nil
			})
	}
}

func errResult(format string, a ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, a...)}}}
}

// call はツール 1 回の呼び出しをツールサーバーへの HTTP リクエストにして転送する。
func (s *Server) call(ctx context.Context, op *operation, raw json.RawMessage) *mcp.CallToolResult {
	args := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return errResult("arguments must be a JSON object: %v", err)
		}
	}
	path := op.path
	query := url.Values{}
	header := http.Header{}
	for _, p := range op.params {
		v, ok := args[p.name]
		if !ok || string(bytes.TrimSpace(v)) == "null" {
			if p.required {
				return errResult("missing required argument %q", p.name)
			}
			continue
		}
		vals := stringify(v)
		switch p.in {
		case "path":
			path = strings.ReplaceAll(path, "{"+p.name+"}", url.PathEscape(strings.Join(vals, ",")))
		case "query":
			if p.explode {
				for _, x := range vals {
					query.Add(p.name, x)
				}
			} else {
				query.Set(p.name, strings.Join(vals, ","))
			}
		case "header":
			if !forbiddenHeaders[strings.ToLower(p.name)] {
				header.Set(p.name, strings.Join(vals, ","))
			}
		}
	}
	if strings.Contains(path, "{") {
		return errResult("path parameters are not filled in: %s", path)
	}

	var body []byte
	if op.hasBody {
		if b, ok := args[op.bodyKey]; ok && string(bytes.TrimSpace(b)) != "null" {
			if op.bodyJSON {
				body = b
				header.Set("Content-Type", "application/json")
			} else {
				form, err := formEncode(b)
				if err != nil {
					return errResult("body must be an object for a form request: %v", err)
				}
				body = []byte(form)
				header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
		} else if op.bodyReq {
			return errResult("missing required argument %q", op.bodyKey)
		}
	}

	target := s.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	hdr, val, err := s.auth()
	if err != nil {
		s.logf(fmt.Sprintf("ツールサーバー %s の秘密を取り出せない: %v", s.name, err))
		return errResult("tool server credentials are unavailable on the host")
	}

	// 認証ヘッダはガードにもエージェントにも見せない (付けるのは転送の直前だけ)
	if s.guard != nil {
		limit := s.guard.InspectLimit()
		peek, truncated := body, false
		if len(peek) > limit {
			peek, truncated = peek[:limit], true
		}
		gh := header.Clone()
		gh.Set("Accept", "application/json")
		if err := s.guard.Check(ctx, guard.Request{
			Provider: "tool:" + s.name, Method: op.method, Host: s.host,
			Path: path, Query: query.Encode(), Headers: gh, Body: peek, BodyTruncated: truncated,
		}); err != nil {
			s.logf(fmt.Sprintf("ツール呼び出しがコンテンツガードで止まった (%s %s): %v", s.name, op.toolName, err))
			return errResult("blocked by the request content guard: %v", err)
		}
	}

	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, op.method, target, rd)
	if err != nil {
		return errResult("could not build the request: %v", err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Accept", "application/json, text/plain;q=0.9, */*;q=0.5")
	if hdr != "" {
		req.Header.Set(hdr, val)
	}
	start := time.Now()
	resp, err := s.client.Do(req)
	if err != nil {
		s.logf(fmt.Sprintf("ツール %s の呼び出しに失敗: %v", op.toolName, scrub(err)))
		return errResult("request to the tool server failed: %v", scrub(err))
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return errResult("reading the tool server response failed: %v", scrub(err))
	}
	truncated := len(b) > maxResponse
	if truncated {
		b = b[:maxResponse]
	}
	s.logf(fmt.Sprintf("ツール %s: %s %s -> %d (%d bytes, %s)", op.toolName, op.method, path,
		resp.StatusCode, len(b), time.Since(start).Round(time.Millisecond)))

	var text string
	if !isText(resp.Header.Get("Content-Type"), b) {
		text = fmt.Sprintf("(binary response: %s, %d bytes; not shown)", resp.Header.Get("Content-Type"), len(b))
	} else {
		text = string(b)
		if truncated {
			text += fmt.Sprintf("\n\n(truncated: only the first %d bytes are shown)", maxResponse)
		}
	}
	if resp.StatusCode/100 != 2 {
		return errResult("HTTP %d\n%s", resp.StatusCode, text)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// scrub はエラーから URL (クエリに入った値など) を落として返す。
func scrub(err error) error {
	if ue, ok := err.(*url.Error); ok {
		return ue.Err
	}
	return err
}

// stringify は JSON の値をクエリ・パス・ヘッダ用の文字列にする (配列は要素ごと)。
func stringify(v json.RawMessage) []string {
	var arr []json.RawMessage
	if json.Unmarshal(v, &arr) == nil {
		var out []string
		for _, e := range arr {
			out = append(out, stringify(e)...)
		}
		return out
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		return []string{s}
	}
	return []string{string(bytes.TrimSpace(v))} // 数値・真偽値・オブジェクトはそのまま
}

// formEncode は JSON のオブジェクトを application/x-www-form-urlencoded にする。
func formEncode(b json.RawMessage) (string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return "", err
	}
	f := url.Values{}
	for k, v := range m {
		if string(bytes.TrimSpace(v)) == "null" {
			continue
		}
		for _, x := range stringify(v) {
			f.Add(k, x)
		}
	}
	return f.Encode(), nil
}

// isText は応答をそのままテキストとしてエージェントに見せてよいかを返す。
func isText(contentType string, b []byte) bool {
	if contentType == "" {
		return !bytes.Contains(b[:min(len(b), 512)], []byte{0})
	}
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	switch {
	case strings.HasPrefix(mt, "text/"), strings.HasSuffix(mt, "+json"), strings.HasSuffix(mt, "+xml"):
		return true
	}
	switch mt {
	case "application/json", "application/xml", "application/yaml", "application/x-yaml",
		"application/javascript", "application/x-www-form-urlencoded", "application/x-ndjson":
		return true
	}
	return false
}
