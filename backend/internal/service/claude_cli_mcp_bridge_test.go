package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBuildClaudeCLIMCPConfigUsesHTTPTransport(t *testing.T) {
	cfg, err := buildClaudeCLIMCPConfig([]claudeCLIMCPConfigServer{{
		Name:  "claude",
		URL:   "http://127.0.0.1:12345/mcp/session-1/claude",
		Token: "secret-token",
	}})
	require.NoError(t, err)

	var parsed claudeCLIMCPConfig
	require.NoError(t, json.Unmarshal([]byte(cfg), &parsed))

	server, ok := parsed.MCPServers["claude"]
	require.True(t, ok)
	require.Equal(t, "http", server.Type)
	require.Equal(t, "http://127.0.0.1:12345/mcp/session-1/claude", server.URL)
	require.Equal(t, map[string]string{"Authorization": "Bearer secret-token"}, server.Headers)
	require.Empty(t, server.Command)
	require.Empty(t, server.Args)
}

func TestSplitClaudeCLIToolsByMCPServerKeepsExistingMCPNames(t *testing.T) {
	groups := splitClaudeCLIToolsByMCPServer([]claudeCLITool{
		{Name: "Bash", Description: "run shell"},
		{Name: "mcp__jetbrains__read_file", Description: "read file"},
	})

	require.Len(t, groups, 2)
	require.Equal(t, "claude", groups[0].ServerName)
	require.Equal(t, []claudeCLITool{{Name: "Bash", Description: "run shell"}}, groups[0].Tools)
	require.Equal(t, "jetbrains", groups[1].ServerName)
	require.Equal(t, []claudeCLITool{{Name: "read_file", Description: "read file", ClientName: "mcp__jetbrains__read_file"}}, groups[1].Tools)
}

func TestClaudeCLIHTTPMCPHandlerInitializeAndToolsList(t *testing.T) {
	registry := newClaudeCLIHTTPMCPRegistry(time.Now)
	session := registry.Register([]claudeCLIToolGroup{{ServerName: "claude", Tools: claudeCLIMCPTestTools()}}, time.Minute)

	initialize := postClaudeCLIHTTPMCP(t, registry, session, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	result, ok := initialize["result"].(map[string]any)
	require.True(t, ok)
	capabilities, ok := result["capabilities"].(map[string]any)
	require.True(t, ok)
	require.Contains(t, capabilities, "tools")

	toolsList := postClaudeCLIHTTPMCP(t, registry, session, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	listResult, ok := toolsList["result"].(map[string]any)
	require.True(t, ok)
	tools, ok := listResult["tools"].([]any)
	require.True(t, ok)
	require.Len(t, tools, 1)
	tool, ok := tools[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "lookup", tool["name"])
	require.Equal(t, "lookup data", tool["description"])
	inputSchema, ok := tool["inputSchema"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "object", inputSchema["type"])
}

func TestClaudeCLIHTTPMCPHandlerRequiresBearerToken(t *testing.T) {
	registry := newClaudeCLIHTTPMCPRegistry(time.Now)
	session := registry.Register([]claudeCLIToolGroup{{ServerName: "claude", Tools: claudeCLIMCPTestTools()}}, time.Minute)

	req := httptest.NewRequest(http.MethodPost, "/mcp/"+session.ID, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	registry.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestClaudeCLIHTTPMCPHandlerDeletesSession(t *testing.T) {
	registry := newClaudeCLIHTTPMCPRegistry(time.Now)
	session := registry.Register([]claudeCLIToolGroup{{ServerName: "claude", Tools: claudeCLIMCPTestTools()}}, time.Minute)

	req := httptest.NewRequest(http.MethodDelete, "/mcp/"+session.ID, nil)
	req.Header.Set("Authorization", "Bearer "+session.Token)
	rec := httptest.NewRecorder()
	registry.ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)

	req = httptest.NewRequest(http.MethodPost, "/mcp/"+session.ID, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+session.Token)
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	registry.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestClaudeCLIHTTPMCPHandlerToolsCallWaitsForClientResult(t *testing.T) {
	registry := newClaudeCLIHTTPMCPRegistry(time.Now)
	session := registry.Register([]claudeCLIToolGroup{{ServerName: "claude", Tools: claudeCLIMCPTestTools()}}, time.Minute)

	responseCh := make(chan map[string]any, 1)
	go func() {
		responseCh <- postClaudeCLIHTTPMCP(t, registry, session, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup","arguments":{"q":"hello"}}}`)
	}()

	call, err := registry.WaitToolCall(context.Background(), session.ID)
	require.NoError(t, err)
	require.Equal(t, "lookup", call.Name)
	require.Equal(t, map[string]any{"q": "hello"}, call.Input)

	select {
	case response := <-responseCh:
		t.Fatalf("tools/call returned before client result: %#v", response)
	case <-time.After(50 * time.Millisecond):
	}

	require.NoError(t, registry.CompleteToolCall(call.ID, claudeCLIToolCallResult{
		Content: "lookup result",
	}))

	response := <-responseCh
	result, ok := response["result"].(map[string]any)
	require.True(t, ok)
	content, ok := result["content"].([]any)
	require.True(t, ok)
	require.Len(t, content, 1)
	text, ok := content[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "text", text["type"])
	require.Equal(t, "lookup result", text["text"])
}

func TestClaudeCLIHTTPMCPHandlerAcceptsClaudeCodePrefixedToolName(t *testing.T) {
	registry := newClaudeCLIHTTPMCPRegistry(time.Now)
	session := registry.Register([]claudeCLIToolGroup{{ServerName: "claude", Tools: []claudeCLITool{
		{Name: "Bash", Description: "run shell command", InputSchema: map[string]any{"type": "object"}},
	}}}, time.Minute)

	responseCh := make(chan map[string]any, 1)
	go func() {
		responseCh <- postClaudeCLIHTTPMCP(t, registry, session, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"mcp__claude__Bash","arguments":{"command":"date"}}}`)
	}()

	call, err := registry.WaitToolCall(context.Background(), session.ID)
	require.NoError(t, err)
	require.Equal(t, "Bash", call.Name)
	require.Equal(t, map[string]any{"command": "date"}, call.Input)

	require.NoError(t, registry.CompleteToolCall(call.ID, claudeCLIToolCallResult{
		Content: "date result",
	}))
	response := <-responseCh
	require.NotContains(t, fmt.Sprint(response), "No such tool")
	result, ok := response["result"].(map[string]any)
	require.True(t, ok)
	content, ok := result["content"].([]any)
	require.True(t, ok)
	text, ok := content[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "date result", text["text"])
}

func TestClaudeCLIHTTPMCPHandlerRoutesMultipleServersToOneSession(t *testing.T) {
	registry := newClaudeCLIHTTPMCPRegistry(time.Now)
	session := registry.Register([]claudeCLIToolGroup{
		{ServerName: "claude", Tools: []claudeCLITool{{Name: "Bash", Description: "run shell command", InputSchema: map[string]any{"type": "object"}}}},
		{ServerName: "jetbrains", Tools: []claudeCLITool{{Name: "get_repositories", Description: "get repos", InputSchema: map[string]any{"type": "object"}}}},
	}, time.Minute)

	claudeTools := postClaudeCLIHTTPMCPForServer(t, registry, session, "claude", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	jetbrainsTools := postClaudeCLIHTTPMCPForServer(t, registry, session, "jetbrains", `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	require.Contains(t, fmt.Sprint(claudeTools), "Bash")
	require.NotContains(t, fmt.Sprint(claudeTools), "get_repositories")
	require.Contains(t, fmt.Sprint(jetbrainsTools), "get_repositories")
	require.NotContains(t, fmt.Sprint(jetbrainsTools), "Bash")

	responseCh := make(chan map[string]any, 1)
	go func() {
		responseCh <- postClaudeCLIHTTPMCPForServer(t, registry, session, "jetbrains", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_repositories","arguments":{},"_meta":{"claudecode/toolUseId":"toolu_jetbrains"}}}`)
	}()

	call, err := registry.WaitToolCall(context.Background(), session.ID)
	require.NoError(t, err)
	require.Equal(t, "toolu_jetbrains", call.ID)
	require.Equal(t, "mcp__jetbrains__get_repositories", call.Name)

	require.NoError(t, registry.CompleteToolCall(call.ID, claudeCLIToolCallResult{Content: "repos"}))
	<-responseCh
}

func TestClaudeCLIHTTPMCPHandlerReusesClaudeCodeToolUseID(t *testing.T) {
	registry := newClaudeCLIHTTPMCPRegistry(time.Now)
	session := registry.Register([]claudeCLIToolGroup{{ServerName: "claude", Tools: claudeCLIMCPTestTools()}}, time.Minute)

	const toolUseID = "toolu_074a782bb9ff4651823e4127"
	responseCh := make(chan map[string]any, 1)
	go func() {
		responseCh <- postClaudeCLIHTTPMCP(t, registry, session, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup","arguments":{"q":"hello"},"_meta":{"claudecode/toolUseId":"`+toolUseID+`"}}}`)
	}()

	call, err := registry.WaitToolCall(context.Background(), session.ID)
	require.NoError(t, err)
	require.Equal(t, toolUseID, call.ID)

	require.NoError(t, registry.CompleteToolCall(call.ID, claudeCLIToolCallResult{
		Content: "lookup result",
	}))
	<-responseCh
}

func claudeCLIMCPTestTools() []claudeCLITool {
	return []claudeCLITool{
		{Name: "lookup", Description: "lookup data", InputSchema: map[string]any{"type": "object"}},
	}
}

func postClaudeCLIHTTPMCP(t *testing.T, registry *claudeCLIHTTPMCPRegistry, session claudeCLIHTTPMCPSession, body string) map[string]any {
	t.Helper()
	return postClaudeCLIHTTPMCPForServer(t, registry, session, "claude", body)
}

func postClaudeCLIHTTPMCPForServer(t *testing.T, registry *claudeCLIHTTPMCPRegistry, session claudeCLIHTTPMCPSession, serverName, body string) map[string]any {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/mcp/"+session.ID+"/"+serverName, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+session.Token)
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	registry.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var response map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	return response
}
