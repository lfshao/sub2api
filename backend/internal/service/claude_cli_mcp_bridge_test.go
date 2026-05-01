package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBuildClaudeCLIMCPConfigUsesHTTPTransport(t *testing.T) {
	cfg, err := buildClaudeCLIMCPConfig("http://127.0.0.1:12345/mcp/session-1", "secret-token")
	require.NoError(t, err)

	var parsed claudeCLIMCPConfig
	require.NoError(t, json.Unmarshal([]byte(cfg), &parsed))

	server, ok := parsed.MCPServers["tools"]
	require.True(t, ok)
	require.Equal(t, "http", server.Type)
	require.Equal(t, "http://127.0.0.1:12345/mcp/session-1", server.URL)
	require.Equal(t, map[string]string{"Authorization": "Bearer secret-token"}, server.Headers)
	require.Empty(t, server.Command)
	require.Empty(t, server.Args)
}

func TestClaudeCLIHTTPMCPHandlerInitializeAndToolsList(t *testing.T) {
	registry := newClaudeCLIHTTPMCPRegistry(time.Now)
	session := registry.Register(claudeCLIMCPTestTools(), time.Minute)

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
	session := registry.Register(claudeCLIMCPTestTools(), time.Minute)

	req := httptest.NewRequest(http.MethodPost, "/mcp/"+session.ID, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	registry.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestClaudeCLIHTTPMCPHandlerDeletesSession(t *testing.T) {
	registry := newClaudeCLIHTTPMCPRegistry(time.Now)
	session := registry.Register(claudeCLIMCPTestTools(), time.Minute)

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
	session := registry.Register(claudeCLIMCPTestTools(), time.Minute)

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

func TestClaudeCLIHTTPMCPHandlerReusesClaudeCodeToolUseID(t *testing.T) {
	registry := newClaudeCLIHTTPMCPRegistry(time.Now)
	session := registry.Register(claudeCLIMCPTestTools(), time.Minute)

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

	req := httptest.NewRequest(http.MethodPost, "/mcp/"+session.ID, strings.NewReader(body))
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
