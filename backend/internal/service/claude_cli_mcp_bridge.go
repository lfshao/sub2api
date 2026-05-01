package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type claudeCLIMCPConfig struct {
	MCPServers map[string]claudeCLIMCPServer `json:"mcpServers"`
}

type claudeCLIMCPServer struct {
	Type    string            `json:"type,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

const claudeCLIMCPServerName = "tools"

func buildClaudeCLIMCPConfig(endpointURL, token string) (string, error) {
	if endpointURL == "" {
		return "", errors.New("claude cli mcp config: endpoint url is empty")
	}
	if token == "" {
		return "", errors.New("claude cli mcp config: bearer token is empty")
	}

	cfg := claudeCLIMCPConfig{
		MCPServers: map[string]claudeCLIMCPServer{
			claudeCLIMCPServerName: {
				Type: "http",
				URL:  endpointURL,
				Headers: map[string]string{
					"Authorization": "Bearer " + token,
				},
			},
		},
	}

	encoded, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

type claudeCLIHTTPMCPSession struct {
	ID        string
	Token     string
	Tools     []claudeCLITool
	ExpiresAt time.Time

	server    *mcpsdk.Server
	toolCalls chan claudeCLIToolCall
	pending   map[string]chan claudeCLIToolCallResult
}

type claudeCLIToolCall struct {
	ID        string
	SessionID string
	Name      string
	Input     map[string]any
}

type claudeCLIToolCallResult struct {
	Content any
	IsError bool
}

type claudeCLIHTTPMCPRegistry struct {
	mu       sync.Mutex
	sessions map[string]*claudeCLIHTTPMCPSession
	now      func() time.Time
	handler  http.Handler
}

func newClaudeCLIHTTPMCPRegistry(now func() time.Time) *claudeCLIHTTPMCPRegistry {
	if now == nil {
		now = time.Now
	}
	r := &claudeCLIHTTPMCPRegistry{
		sessions: make(map[string]*claudeCLIHTTPMCPSession),
		now:      now,
	}
	r.handler = mcpsdk.NewStreamableHTTPHandler(r.sdkServerForRequest, &mcpsdk.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
	})
	return r
}

func logClaudeCLIMCPDebug(format string, args ...any) {
	if !isClaudeCLIDebugEnabled() {
		return
	}
	logger.LegacyPrintf("service.claude_cli", "[ClaudeCLIMCPDebug] "+format, args...)
}

func (r *claudeCLIHTTPMCPRegistry) Register(tools []claudeCLITool, ttl time.Duration) claudeCLIHTTPMCPSession {
	if r == nil {
		return claudeCLIHTTPMCPSession{}
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}

	session := &claudeCLIHTTPMCPSession{
		ID:        randomClaudeCLIMCPHex(16),
		Token:     randomClaudeCLIMCPHex(32),
		Tools:     append([]claudeCLITool(nil), tools...),
		ExpiresAt: r.now().Add(ttl),
		toolCalls: make(chan claudeCLIToolCall, 16),
		pending:   make(map[string]chan claudeCLIToolCallResult),
	}
	session.server = r.buildSDKServer(session)
	r.mu.Lock()
	r.sessions[session.ID] = session
	r.mu.Unlock()
	toolNames := make([]string, 0, len(tools))
	for _, tool := range tools {
		toolNames = append(toolNames, tool.Name)
	}
	logClaudeCLIMCPDebug("register session=%s tools=%v ttl=%s", session.ID, toolNames, ttl)
	return *session
}

func (r *claudeCLIHTTPMCPRegistry) Unregister(id string) {
	if r == nil || id == "" {
		return
	}
	r.mu.Lock()
	session := r.sessions[id]
	delete(r.sessions, id)
	var pending []chan claudeCLIToolCallResult
	if session != nil {
		pending = make([]chan claudeCLIToolCallResult, 0, len(session.pending))
		for callID, ch := range session.pending {
			pending = append(pending, ch)
			delete(session.pending, callID)
		}
	}
	r.mu.Unlock()
	for _, ch := range pending {
		select {
		case ch <- claudeCLIToolCallResult{
			Content: "client tool execution cancelled",
			IsError: true,
		}:
		default:
		}
	}
}

func (r *claudeCLIHTTPMCPRegistry) WaitToolCall(ctx context.Context, sessionID string) (claudeCLIToolCall, error) {
	session, ok := r.get(sessionID)
	if !ok {
		logClaudeCLIMCPDebug("wait session missing session=%s", sessionID)
		return claudeCLIToolCall{}, errors.New("claude cli mcp: session not found")
	}
	select {
	case call := <-session.toolCalls:
		logClaudeCLIMCPDebug("wait got call session=%s call_id=%s name=%s", sessionID, call.ID, call.Name)
		return call, nil
	case <-ctx.Done():
		logClaudeCLIMCPDebug("wait context done session=%s err=%v", sessionID, ctx.Err())
		return claudeCLIToolCall{}, ctx.Err()
	}
}

func (r *claudeCLIHTTPMCPRegistry) CompleteToolCall(callID string, result claudeCLIToolCallResult) error {
	if r == nil || callID == "" {
		return errors.New("claude cli mcp: tool call id is empty")
	}
	r.mu.Lock()
	var resultCh chan claudeCLIToolCallResult
	for _, session := range r.sessions {
		if session.pending == nil {
			continue
		}
		if ch, ok := session.pending[callID]; ok {
			resultCh = ch
			delete(session.pending, callID)
			break
		}
	}
	r.mu.Unlock()
	if resultCh == nil {
		return fmt.Errorf("claude cli mcp: pending tool call %q not found", callID)
	}
	resultCh <- result
	return nil
}

func (r *claudeCLIHTTPMCPRegistry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost && req.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sessionID := strings.TrimPrefix(req.URL.Path, "/mcp/")
	if sessionID == "" || sessionID == req.URL.Path || strings.Contains(sessionID, "/") {
		http.NotFound(w, req)
		return
	}

	session, ok := r.get(sessionID)
	if !ok {
		logClaudeCLIMCPDebug("http session not found method=%s path=%s session=%s", req.Method, req.URL.Path, sessionID)
		http.NotFound(w, req)
		return
	}
	if req.Header.Get("Authorization") != "Bearer "+session.Token {
		logClaudeCLIMCPDebug("http unauthorized method=%s path=%s session=%s", req.Method, req.URL.Path, sessionID)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if req.Method == http.MethodDelete {
		logClaudeCLIMCPDebug("http delete session=%s", session.ID)
		r.Unregister(session.ID)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	method, toolName := claudeCLIMCPDebugHTTPMethod(req)
	logClaudeCLIMCPDebug("http post session=%s path=%s method=%s tool=%s", session.ID, req.URL.Path, method, toolName)
	r.handler.ServeHTTP(w, req)
}

func claudeCLIMCPDebugHTTPMethod(req *http.Request) (string, string) {
	if req == nil || req.Body == nil {
		return "", ""
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		req.Body = io.NopCloser(bytes.NewReader(nil))
		return "read_error:" + err.Error(), ""
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	if len(body) == 0 {
		return "", ""
	}

	var payload struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "decode_error:" + err.Error(), ""
	}
	return payload.Method, payload.Params.Name
}

func (r *claudeCLIHTTPMCPRegistry) get(id string) (*claudeCLIHTTPMCPSession, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	session, ok := r.sessions[id]
	if !ok {
		return nil, false
	}
	if !session.ExpiresAt.IsZero() && !r.now().Before(session.ExpiresAt) {
		delete(r.sessions, id)
		return nil, false
	}
	return session, true
}

func (r *claudeCLIHTTPMCPRegistry) sdkServerForRequest(req *http.Request) *mcpsdk.Server {
	sessionID := strings.TrimPrefix(req.URL.Path, "/mcp/")
	if sessionID == "" || sessionID == req.URL.Path || strings.Contains(sessionID, "/") {
		return nil
	}
	session, ok := r.get(sessionID)
	if !ok {
		return nil
	}
	return session.server
}

func (r *claudeCLIHTTPMCPRegistry) buildSDKServer(session *claudeCLIHTTPMCPSession) *mcpsdk.Server {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "sub2api-client-tools", Version: "0.1.0"}, nil)
	for _, tool := range session.Tools {
		inputSchema := tool.InputSchema
		if inputSchema == nil {
			inputSchema = map[string]any{"type": "object"}
		}
		sdkTool := &mcpsdk.Tool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: inputSchema,
		}
		server.AddTool(sdkTool, r.sdkToolHandler(session.ID))
	}
	return server
}

func (r *claudeCLIHTTPMCPRegistry) sdkToolHandler(sessionID string) mcpsdk.ToolHandler {
	return func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		input := map[string]any{}
		if req != nil && req.Params != nil && len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &input); err != nil {
				logClaudeCLIMCPDebug("tool invalid arguments session=%s err=%v", sessionID, err)
				return &mcpsdk.CallToolResult{
					Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "invalid tool arguments"}},
					IsError: true,
				}, nil
			}
		}

		name := ""
		if req != nil && req.Params != nil {
			name = req.Params.Name
		}
		callID := claudeCLIToolUseIDFromMCPMeta(req)
		if callID == "" {
			callID = "toolu_" + randomClaudeCLIMCPHex(16)
		}
		logClaudeCLIMCPDebug("tool handler enter session=%s call_id=%s name=%s", sessionID, callID, name)
		resultCh := make(chan claudeCLIToolCallResult, 1)

		session, ok := r.get(sessionID)
		if !ok {
			logClaudeCLIMCPDebug("tool session missing session=%s call_id=%s name=%s", sessionID, callID, name)
			return nil, errors.New("claude cli mcp: session not found")
		}

		r.mu.Lock()
		session.pending[callID] = resultCh
		r.mu.Unlock()

		call := claudeCLIToolCall{
			ID:        callID,
			SessionID: sessionID,
			Name:      name,
			Input:     input,
		}
		select {
		case session.toolCalls <- call:
			logClaudeCLIMCPDebug("tool enqueue success session=%s call_id=%s name=%s", sessionID, callID, name)
		case <-ctx.Done():
			r.removePendingToolCall(callID)
			logClaudeCLIMCPDebug("tool enqueue context done session=%s call_id=%s name=%s err=%v", sessionID, callID, name, ctx.Err())
			return nil, ctx.Err()
		}

		select {
		case result := <-resultCh:
			logClaudeCLIMCPDebug("tool result session=%s call_id=%s name=%s is_error=%v", sessionID, callID, name, result.IsError)
			return result.toMCP(), nil
		case <-ctx.Done():
			r.removePendingToolCall(callID)
			logClaudeCLIMCPDebug("tool result context done session=%s call_id=%s name=%s err=%v", sessionID, callID, name, ctx.Err())
			return nil, ctx.Err()
		}
	}
}

func claudeCLIToolUseIDFromMCPMeta(req *mcpsdk.CallToolRequest) string {
	if req == nil || req.Params == nil || req.Params.Meta == nil {
		return ""
	}
	toolUseID, _ := req.Params.Meta["claudecode/toolUseId"].(string)
	return strings.TrimSpace(toolUseID)
}

func (r *claudeCLIHTTPMCPRegistry) removePendingToolCall(callID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, session := range r.sessions {
		delete(session.pending, callID)
	}
}

func (r claudeCLIToolCallResult) toMCP() *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{
		Content: claudeCLIToolResultContent(r.Content),
		IsError: r.IsError,
	}
}

func claudeCLIToolResultContent(raw any) []mcpsdk.Content {
	switch value := raw.(type) {
	case nil:
		return []mcpsdk.Content{&mcpsdk.TextContent{Text: ""}}
	case string:
		return []mcpsdk.Content{&mcpsdk.TextContent{Text: value}}
	case []any:
		out := make([]mcpsdk.Content, 0, len(value))
		for _, item := range value {
			out = append(out, claudeCLIToolResultContent(item)...)
		}
		if len(out) > 0 {
			return out
		}
		return []mcpsdk.Content{&mcpsdk.TextContent{Text: ""}}
	case map[string]any:
		if value["type"] == "text" {
			text, _ := value["text"].(string)
			return []mcpsdk.Content{&mcpsdk.TextContent{Text: text}}
		}
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return []mcpsdk.Content{&mcpsdk.TextContent{Text: fmt.Sprint(raw)}}
	}
	return []mcpsdk.Content{&mcpsdk.TextContent{Text: string(encoded)}}
}

type claudeCLIHTTPMCPServer struct {
	registry *claudeCLIHTTPMCPRegistry

	mu       sync.Mutex
	server   *http.Server
	listener net.Listener
	baseURL  string
}

func newClaudeCLIHTTPMCPServer(now func() time.Time) *claudeCLIHTTPMCPServer {
	return &claudeCLIHTTPMCPServer{
		registry: newClaudeCLIHTTPMCPRegistry(now),
	}
}

func (s *claudeCLIHTTPMCPServer) Register(tools []claudeCLITool, ttl time.Duration) (string, claudeCLIHTTPMCPSession, func(), error) {
	if s == nil {
		return "", claudeCLIHTTPMCPSession{}, nil, errors.New("claude cli mcp http server is nil")
	}
	baseURL, err := s.start()
	if err != nil {
		return "", claudeCLIHTTPMCPSession{}, nil, err
	}
	session := s.registry.Register(tools, ttl)
	endpoint := baseURL + "/mcp/" + session.ID
	cleanup := func() {
		s.registry.Unregister(session.ID)
	}
	return endpoint, session, cleanup, nil
}

func (s *claudeCLIHTTPMCPServer) start() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.baseURL != "" {
		return s.baseURL, nil
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("claude cli mcp http server: listen: %w", err)
	}
	server := &http.Server{
		Handler:           s.registry,
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.listener = listener
	s.server = server
	s.baseURL = "http://" + listener.Addr().String()
	go func() {
		_ = server.Serve(listener)
	}()
	return s.baseURL, nil
}

func randomClaudeCLIMCPHex(size int) string {
	b := make([]byte, size)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return fmt.Sprintf("%0*x", size*2, time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func (s *claudeCLIHTTPMCPServer) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	server := s.server
	s.server = nil
	s.listener = nil
	s.baseURL = ""
	s.mu.Unlock()

	if server == nil {
		return nil
	}
	return server.Close()
}
