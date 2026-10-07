package main

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nananek/quagent/internal/config"
	"github.com/nananek/quagent/internal/guard"
	"github.com/nananek/quagent/internal/toolserver"
)

// toolServerTools は設定の tool_servers (OpenAPI のツールサーバー) を host で読み、
// MCP のツールとして登録する関数を返す。到達できない・仕様を読めないサーバーは
// 警告を出して飛ばす (他のサーバーや run そのものは止めない)。
func toolServerTools(cfg *config.Config, g *guard.Guard, consoleLog func(string)) []func(*mcp.Server) {
	names := make([]string, 0, len(cfg.ToolServers))
	for n := range cfg.ToolServers {
		names = append(names, n)
	}
	sort.Strings(names)

	var regs []func(*mcp.Server)
	for _, n := range names {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		srv, err := toolserver.Load(ctx, n, cfg.ToolServers[n], g, consoleLog)
		cancel()
		if err != nil {
			logf("警告: ツールサーバーを使えない (飛ばす): %v", err)
			continue
		}
		auth := "認証なし"
		if cfg.ToolServers[n].NeedsAuth() {
			auth = "認証あり"
		}
		logf("ツールサーバー %s (%s): %d 個のツールを MCP に載せる: %s",
			n, auth, len(srv.ToolNames()), strings.Join(srv.ToolNames(), ", "))
		regs = append(regs, srv.Register)
	}
	return regs
}
