package toolserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nananek/quagent/internal/config"
)

// FastAPI (Open WebUI のツールサーバー) が返す形の仕様。$ref、パス・クエリ・ヘッダ
// パラメータ、JSON 本文を含む。
const testSpec = `{
  "openapi": "3.1.0",
  "info": {"title": "Test Tools", "version": "1"},
  "paths": {
    "/items/{item_id}": {
      "get": {
        "operationId": "get_item",
        "summary": "Get an item",
        "parameters": [
          {"name": "item_id", "in": "path", "required": true, "schema": {"type": "string"}},
          {"name": "tag", "in": "query", "schema": {"type": "array", "items": {"type": "string"}}},
          {"name": "X-Trace", "in": "header", "schema": {"type": "string"}},
          {"name": "Authorization", "in": "header", "schema": {"type": "string"}}
        ],
        "responses": {"200": {"description": "ok"}}
      }
    },
    "/items": {
      "post": {
        "operationId": "create item!",
        "description": "Create an item",
        "requestBody": {
          "required": true,
          "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Item"}}}
        }
      }
    },
    "/login": {
      "post": {
        "operationId": "login",
        "requestBody": {"content": {"application/x-www-form-urlencoded": {"schema": {"type": "object"}}}}
      }
    },
    "/blob": {"get": {"operationId": "blob"}},
    "/fail": {"get": {"operationId": "fail"}}
  },
  "components": {"schemas": {
    "Item": {"type": "object", "required": ["name"], "properties": {
      "name": {"type": "string"},
      "child": {"$ref": "#/components/schemas/Item"}
    }}
  }}
}`

type seen struct {
	method, path, query, ctype, body string
	header                           http.Header
}

func newToolServer(t *testing.T, wantAuth string) (*httptest.Server, func() seen) {
	t.Helper()
	var mu sync.Mutex
	var last seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wantAuth != "" && r.Header.Get("Authorization") != wantAuth {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/openapi.json" {
			io.WriteString(w, testSpec)
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		last = seen{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get("Content-Type"), string(b), r.Header.Clone()}
		mu.Unlock()
		switch r.URL.Path {
		case "/blob":
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte{0x89, 'P', 'N', 'G'})
		case "/fail":
			http.Error(w, `{"detail":"boom"}`, http.StatusInternalServerError)
		default:
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"ok":true}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() seen { mu.Lock(); defer mu.Unlock(); return last }
}

func text(t *testing.T, r *mcp.CallToolResult) string {
	t.Helper()
	if len(r.Content) != 1 {
		t.Fatalf("content = %d 個", len(r.Content))
	}
	return r.Content[0].(*mcp.TextContent).Text
}

func call(s *Server, tool string, args string) *mcp.CallToolResult {
	return s.call(context.Background(), s.ops[tool], json.RawMessage(args))
}

// 認証なしのサーバーを登録できる (secret を省略)。ヘッダは付かない。
func TestNoAuth(t *testing.T) {
	ts, last := newToolServer(t, "")
	s, err := Load(context.Background(), "tools", config.ToolServer{URL: ts.URL + "/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := call(s, "tools__get_item", `{"item_id":"a/b c","tag":["x","y"],"X-Trace":"t1","Authorization":"Bearer evil"}`)
	if r.IsError || text(t, r) != `{"ok":true}` {
		t.Fatalf("結果: %+v", r)
	}
	got := last()
	if got.method != "GET" || got.path != "/items/a%2Fb%20c" || got.query != "tag=x&tag=y" {
		t.Errorf("転送先: %+v", got)
	}
	if got.header.Get("X-Trace") != "t1" {
		t.Errorf("ヘッダパラメータが渡らない: %v", got.header)
	}
	if got.header.Get("Authorization") != "" {
		t.Errorf("guest 指定の Authorization を転送した: %q", got.header.Get("Authorization"))
	}
}

// 認証ありのサーバーでは host が秘密を付ける。秘密は仕様の取得にも使う。
func TestAuth(t *testing.T) {
	t.Setenv("TOOL_KEY", "s3cret\n")
	ts, last := newToolServer(t, "Bearer s3cret")
	cfg := config.ToolServer{URL: ts.URL, SecretEnv: "TOOL_KEY"}
	s, err := Load(context.Background(), "tools", cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := call(s, "tools__get_item", `{"item_id":"1"}`); r.IsError {
		t.Fatalf("認証つきで失敗: %s", text(t, r))
	}
	if last().header.Get("Authorization") != "Bearer s3cret" {
		t.Errorf("Authorization = %q", last().header.Get("Authorization"))
	}
	// 秘密を付けないと仕様も取れない
	if _, err := Load(context.Background(), "tools", config.ToolServer{URL: ts.URL}, nil); err == nil {
		t.Error("認証なしで 401 のサーバーを読めた")
	}
	// header / prefix で変えられる
	empty := ""
	if _, err := Load(context.Background(), "tools", config.ToolServer{URL: ts.URL, Header: "X-Key", Prefix: &empty, SecretEnv: "TOOL_KEY"}, nil); err == nil {
		t.Error("別ヘッダで認証が通ってしまった")
	}
}

func TestBodyAndSchema(t *testing.T) {
	ts, last := newToolServer(t, "")
	s, err := Load(context.Background(), "tools", config.ToolServer{URL: ts.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := strings.Join(s.ToolNames(), ",")
	if names != "tools__blob,tools__create_item,tools__fail,tools__get_item,tools__login" {
		t.Fatalf("ツール名: %s", names)
	}
	// $ref は展開され (循環は打ち切り)、必須が入力スキーマに出る
	b, _ := json.Marshal(s.ops["tools__create_item"].schema)
	if !strings.Contains(string(b), `"required":["body"]`) || strings.Contains(string(b), "$ref") ||
		!strings.Contains(string(b), `"name":{"type":"string"}`) {
		t.Errorf("schema = %s", b)
	}
	if r := call(s, "tools__create_item", `{"body":{"name":"n"}}`); r.IsError {
		t.Fatalf("POST 失敗: %s", text(t, r))
	}
	if g := last(); g.method != "POST" || g.ctype != "application/json" || g.body != `{"name":"n"}` {
		t.Errorf("本文: %+v", g)
	}
	if r := call(s, "tools__create_item", `{}`); !r.IsError {
		t.Error("必須の body なしを通した")
	}
	// フォーム
	if r := call(s, "tools__login", `{"body":{"user":"u","pw":"a b"}}`); r.IsError {
		t.Fatalf("フォーム失敗: %s", text(t, r))
	}
	if g := last(); g.ctype != "application/x-www-form-urlencoded" || g.body != "pw=a+b&user=u" {
		t.Errorf("フォーム本文: %+v", g)
	}
	// 必須パラメータの欠落 / エラー応答 / バイナリ
	if r := call(s, "tools__get_item", `{}`); !r.IsError {
		t.Error("path パラメータなしを通した")
	}
	if r := call(s, "tools__fail", `{}`); !r.IsError || !strings.Contains(text(t, r), "HTTP 500") {
		t.Errorf("500 の扱い: %+v", r)
	}
	if r := call(s, "tools__blob", ``); r.IsError || !strings.Contains(text(t, r), "binary response") {
		t.Errorf("バイナリの扱い: %+v", r)
	}
}

// MCP クライアントからツール一覧と呼び出しができる (登録の結線)。
func TestMCPEndToEnd(t *testing.T) {
	ts, _ := newToolServer(t, "")
	s, err := Load(context.Background(), "tools", config.ToolServer{URL: ts.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	s.Register(srv)
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	list, err := cs.ListTools(ctx, nil)
	if err != nil || len(list.Tools) != 5 {
		t.Fatalf("ListTools: %v (%d 個)", err, len(list.Tools))
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "tools__get_item", Arguments: map[string]any{"item_id": "7"}})
	if err != nil || res.IsError || text(t, res) != `{"ok":true}` {
		t.Fatalf("CallTool: %v %+v", err, res)
	}
}

func TestLoadErrors(t *testing.T) {
	ts, _ := newToolServer(t, "")
	for _, c := range []struct {
		name string
		cfg  config.ToolServer
	}{
		{"bad__name", config.ToolServer{URL: ts.URL}},
		{"x", config.ToolServer{URL: "ftp://example.com"}},
		{"x", config.ToolServer{URL: ts.URL, OpenAPIPath: "/missing.json"}},
	} {
		if _, err := Load(context.Background(), c.name, c.cfg, nil); err == nil {
			t.Errorf("%s %+v: エラーにならなかった", c.name, c.cfg)
		}
	}
}

func TestToolName(t *testing.T) {
	if got := toolName("srv", "a b/c"); got != "srv__a_b_c" {
		t.Errorf("toolName = %q", got)
	}
	long := toolName("srv", strings.Repeat("x", 100))
	if len(long) != maxToolName {
		t.Errorf("長さ = %d", len(long))
	}
}
