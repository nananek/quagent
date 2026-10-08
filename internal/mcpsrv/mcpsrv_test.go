package mcpsrv

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nananek/quagent/internal/access"
	"github.com/nananek/quagent/internal/netns"
	"github.com/nananek/quagent/internal/pr"
)

type dummyApplier struct{}

func (dummyApplier) SetGrants([]netns.Grant) error { return nil }

type testPublisher struct {
	res pr.Result
	err error
}

func (p testPublisher) Publish(req pr.Request) (pr.Result, error) {
	return p.res, p.err
}

func TestDescribe(t *testing.T) {
	cases := []struct {
		res  access.Result
		want string
	}{
		{
			res:  access.Result{Status: access.Approved, Auto: true},
			want: "Approved without review",
		},
		{
			res:  access.Result{Status: access.Approved, Kind: access.Once, ExpiresAt: "12:00:00"},
			want: "Approved for new connections until 12:00:00",
		},
		{
			res:  access.Result{Status: access.Approved, Kind: access.Session},
			want: "Approved for the rest of this session",
		},
		{
			res:  access.Result{Status: access.Denied},
			want: "Denied by the reviewer",
		},
		{
			res:  access.Result{Status: access.Question},
			want: "The reviewer has a question",
		},
		{
			res:  access.Result{Status: access.TimedOut},
			want: "timeout, not an explicit denial",
		},
		{
			res:  access.Result{Status: access.Pending},
			want: "Still waiting for the reviewer",
		},
	}

	for _, c := range cases {
		out := describe(c.res)
		if !strings.Contains(out.Message, c.want) {
			t.Errorf("describe(%+v).Message = %q, want containing %q", c.res, out.Message, c.want)
		}
	}
}

func TestNonNil(t *testing.T) {
	var nilSlice []string
	if got := nonNil(nilSlice); got == nil || len(got) != 0 {
		t.Errorf("nonNil(nil) = %#v, want empty non-nil slice", got)
	}

	nonEmpty := []int{1, 2, 3}
	if got := nonNil(nonEmpty); len(got) != 3 {
		t.Errorf("nonNil(nonEmpty) = %#v, want slice of len 3", got)
	}
}

func TestHandlerToolsIntegration(t *testing.T) {
	tmpDir := t.TempDir()
	alwaysFile := filepath.Join(tmpDir, "always.json")
	mgr, err := access.NewManager(dummyApplier{}, alwaysFile)
	if err != nil {
		t.Fatal(err)
	}

	// 自動承認のドメインを登録
	if err := mgr.Preallow([]string{"trusted.com"}); err != nil {
		t.Fatal(err)
	}

	var logs []string
	logf := func(s string) {
		logs = append(logs, s)
	}
	pub := testPublisher{
		res: pr.Result{Created: true, Signed: 2, URL: "https://github.com/org/repo/pull/1"},
	}

	h := Handler(mgr, pub, logf)
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.1.0"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:             srv.URL + "/mcp",
		DisableStandaloneSSE: true,
	}

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("client.Connect failed: %v", err)
	}
	defer session.Close()

	// 1. ListTools
	toolsRes, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}
	toolNames := map[string]bool{}
	for _, tool := range toolsRes.Tools {
		toolNames[tool.Name] = true
	}
	expectedTools := []string{
		"request_network_access",
		"wait_network_access",
		"release_network_access",
		"list_network_access",
		"create_pull_request",
	}
	for _, expected := range expectedTools {
		if !toolNames[expected] {
			t.Errorf("missing tool %q", expected)
		}
	}

	// 2. request_network_access (auto-approved domain)
	reqCall, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "request_network_access",
		Arguments: map[string]any{
			"domains": []string{"trusted.com"},
			"reason":  "testing auto approve",
		},
	})
	if err != nil {
		t.Fatalf("CallTool request_network_access failed: %v", err)
	}
	if reqCall.IsError {
		t.Fatalf("request_network_access returned error: %v", reqCall.Content)
	}

	// 3. list_network_access
	listCall, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "list_network_access",
	})
	if err != nil {
		t.Fatalf("CallTool list_network_access failed: %v", err)
	}
	if listCall.IsError {
		t.Fatalf("list_network_access returned error: %v", listCall.Content)
	}

	// 4. release_network_access
	relCall, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "release_network_access",
		Arguments: map[string]any{
			"domains": []string{"trusted.com"},
		},
	})
	if err != nil {
		t.Fatalf("CallTool release_network_access failed: %v", err)
	}
	if relCall.IsError {
		t.Fatalf("release_network_access returned error: %v", relCall.Content)
	}

	// 5. create_pull_request
	prCall, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "create_pull_request",
		Arguments: map[string]any{
			"branch": "feature/test",
			"title":  "Test PR",
			"body":   "PR body",
		},
	})
	if err != nil {
		t.Fatalf("CallTool create_pull_request failed: %v", err)
	}
	if prCall.IsError {
		t.Fatalf("create_pull_request returned error: %v", prCall.Content)
	}
	if len(logs) == 0 || !strings.Contains(logs[0], "PR を作成") {
		t.Errorf("expected log message for PR creation, got %v", logs)
	}
}

func TestHandlerExtraTools(t *testing.T) {
	extraCalled := false
	extra := func(s *mcp.Server) {
		extraCalled = true
	}
	h := Handler(nil, nil, func(string) {}, extra)
	if h == nil || !extraCalled {
		t.Error("expected extra registrar to be called")
	}
}
