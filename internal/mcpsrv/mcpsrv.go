// Package mcpsrv は guest のエージェントに向けた MCP サーバー (host 窓口の /mcp)。
package mcpsrv

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nananek/quagent/internal/access"
	"github.com/nananek/quagent/internal/pr"
)

// Path は窓口上の MCP エンドポイント。
const Path = "/mcp"

// 1 回のツール呼び出しで待つ上限。クライアント側のタイムアウトに掛からないよう
// 短めに切り、決まらなければ pending を返して wait_network_access で待ち直してもらう。
const waitChunk = 50 * time.Second

const instructions = `This VM has no outbound network access by default. LLM API calls are
already routed through the host and need nothing from you.

To reach anything else (package registries, git hosts, docs sites, container
registries ...), call request_network_access with every domain the task needs
and a concrete reason. A human approves or denies the whole request at once.
Approval works at the DNS level: approved names resolve and can be connected
to; everything else fails to resolve. Use "*.example.com" for subdomains.

Only one request can be pending at a time: wait for its result (wait_network_access)
before sending another. After a denial or a timeout, the same domains cannot be
requested again for 10 minutes.

When you no longer need a domain, call release_network_access. Already-open
connections keep working after release or expiry; only new connections stop.`

type requestIn struct {
	Domains []string `json:"domains" jsonschema:"Domain names to allow, e.g. [\"pypi.org\", \"files.pythonhosted.org\"]. Use \"*.example.com\" for all subdomains of example.com. List every domain the task needs in one request."`
	Reason  string   `json:"reason" jsonschema:"Why these domains are needed, concretely (what you will download or call, for which part of the task). If the reviewer asked a question earlier, include the answer here."`
}

type waitIn struct {
	RequestID int `json:"request_id" jsonschema:"request_id returned by request_network_access with status pending"`
}

type releaseIn struct {
	Domains []string `json:"domains" jsonschema:"Domains you no longer need. New connections to them stop; open connections are not cut."`
}

type releaseOut struct {
	Released []string `json:"released"`
}

type grantOut struct {
	Domain    string `json:"domain"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

type listOut struct {
	Grants []grantOut `json:"grants"`
}

type resultOut struct {
	access.Result
	Message string `json:"message"`
}

func describe(r access.Result) resultOut {
	out := resultOut{Result: r}
	switch r.Status {
	case access.Approved:
		switch {
		case r.Auto:
			out.Message = "Approved without review (previously trusted). Release when done."
		case r.Kind == access.Once:
			out.Message = fmt.Sprintf("Approved for new connections until %s. Release when done.", r.ExpiresAt)
		default:
			out.Message = "Approved for the rest of this session. Release when done."
		}
	case access.Denied:
		out.Message = "Denied by the reviewer. These domains cannot be requested again for 10 minutes; find another way or explain to the user."
	case access.Question:
		out.Message = "The reviewer has a question instead of a decision. Answer it in the reason of a new request_network_access call."
	case access.TimedOut:
		out.Message = fmt.Sprintf("Denied because the reviewer did not respond within %s (timeout, not an explicit denial). "+
			"These domains cannot be requested again for %s.", access.DecisionTimeout, access.DenyCooldown)
	case access.Pending:
		out.Message = "Still waiting for the reviewer. Call wait_network_access with this request_id to keep waiting."
	}
	return out
}

type prIn struct {
	Branch string `json:"branch" jsonschema:"Branch in /work that holds your commits. Must not be a protected branch (main, master, develop by default)."`
	Title  string `json:"title" jsonschema:"Pull request title"`
	Body   string `json:"body,omitempty" jsonschema:"Pull request description (markdown)"`
	Base   string `json:"base,omitempty" jsonschema:"Branch to merge into. Defaults to the repository's default branch."`
}

// PRPublisher は PR の作成・更新 (pr.Publisher)。
type PRPublisher interface {
	Publish(pr.Request) (pr.Result, error)
}

// Handler は MCP サーバーの HTTP ハンドラを返す。
func Handler(m *access.Manager, pub PRPublisher, logf func(string)) http.Handler {
	s := mcp.NewServer(&mcp.Implementation{Name: "quagent", Version: "0.1.0"},
		&mcp.ServerOptions{Instructions: instructions})

	mcp.AddTool(s, &mcp.Tool{
		Name: "request_network_access",
		Description: "Ask the human reviewer to allow outbound connections to some domains. " +
			"Blocks for up to ~50s; if still undecided returns status=pending, then call wait_network_access. " +
			"Statuses: approved, denied, question (reviewer asks something; answer it in a new request's reason), timeout (no response in 10 minutes; treated as denied), pending.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in requestIn) (*mcp.CallToolResult, resultOut, error) {
		r, err := m.Submit(in.Domains, in.Reason)
		if err != nil {
			return nil, resultOut{}, err
		}
		res, err := m.Wait(ctx, r.ID, waitChunk)
		if err != nil {
			return nil, resultOut{}, err
		}
		return nil, describe(res), nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "wait_network_access",
		Description: "Keep waiting for a pending request_network_access decision (blocks up to ~50s per call).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in waitIn) (*mcp.CallToolResult, resultOut, error) {
		res, err := m.Wait(ctx, in.RequestID, waitChunk)
		if err != nil {
			return nil, resultOut{}, err
		}
		return nil, describe(res), nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "release_network_access",
		Description: "Give up access to domains you no longer need. Do this as soon as you are done with them.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in releaseIn) (*mcp.CallToolResult, releaseOut, error) {
		released, err := m.Release(in.Domains)
		if err != nil {
			return nil, releaseOut{}, err
		}
		return nil, releaseOut{Released: nonNil(released)}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_network_access",
		Description: "List the domains currently allowed for new connections.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listOut, error) {
		var out listOut
		for d, exp := range m.Grants() {
			g := grantOut{Domain: d}
			if exp != 0 {
				g.ExpiresAt = time.Unix(exp, 0).Format(time.RFC3339)
			}
			out.Grants = append(out.Grants, g)
		}
		sort.Slice(out.Grants, func(i, j int) bool { return out.Grants[i].Domain < out.Grants[j].Domain })
		out.Grants = nonNil(out.Grants)
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "create_pull_request",
		Description: "Publish a branch of /work as a GitHub pull request. Commit your work on a non-protected branch first. " +
			"The host fetches the branch, signs the commits, pushes it and opens the PR (you have no GitHub credentials and cannot push yourself). " +
			"Call again with the same branch after adding commits to update the PR. Do not rewrite already-published commits.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in prIn) (*mcp.CallToolResult, pr.Result, error) {
		res, err := pub.Publish(pr.Request{Branch: in.Branch, Title: in.Title, Body: in.Body, Base: in.Base})
		if err != nil {
			logf("PR の作成に失敗 (" + in.Branch + "): " + err.Error())
			return nil, pr.Result{}, err
		}
		verb := "更新"
		if res.Created {
			verb = "作成"
		}
		logf(fmt.Sprintf("PR を%s: %s (署名 %d コミット) %s", verb, in.Branch, res.Signed, res.URL))
		return nil, res, nil
	})

	// セッションを覚えない (作っては捨てるを繰り返されても host のメモリが増えない)。
	// ツールはセッションの状態を使わない。リクエストの大きさも絞る。
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s },
		&mcp.StreamableHTTPOptions{Stateless: true})
	return http.MaxBytesHandler(h, 1<<20)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
