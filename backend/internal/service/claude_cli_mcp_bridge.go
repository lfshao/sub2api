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

type claudeCLIMCPConfigServer struct {
	Name  string
	URL   string
	Token string
}

const claudeCLIInternalMCPServerName = "claude"

func buildClaudeCLIMCPConfig(servers []claudeCLIMCPConfigServer) (string, error) {
	if len(servers) == 0 {
		return "", errors.New("claude cli mcp config: no servers")
	}

	cfg := claudeCLIMCPConfig{
		MCPServers: make(map[string]claudeCLIMCPServer, len(servers)),
	}
	for _, server := range servers {
		if server.Name == "" {
			return "", errors.New("claude cli mcp config: server name is empty")
		}
		if server.URL == "" {
			return "", fmt.Errorf("claude cli mcp config: endpoint url is empty for server %q", server.Name)
		}
		if server.Token == "" {
			return "", fmt.Errorf("claude cli mcp config: bearer token is empty for server %q", server.Name)
		}
		cfg.MCPServers[server.Name] = claudeCLIMCPServer{
			Type: "http",
			URL:  server.URL,
			Headers: map[string]string{
				"Authorization": "Bearer " + server.Token,
			},
		}
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
	Groups    []claudeCLIToolGroup
	ExpiresAt time.Time

	servers   map[string]*mcpsdk.Server
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

func (r *claudeCLIHTTPMCPRegistry) Register(groups []claudeCLIToolGroup, ttl time.Duration) claudeCLIHTTPMCPSession {
	if r == nil {
		return claudeCLIHTTPMCPSession{}
	}
	if len(groups) == 0 {
		groups = []claudeCLIToolGroup{{ServerName: claudeCLIInternalMCPServerName}}
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	groups = normalizeClaudeCLIToolGroups(groups)

	session := &claudeCLIHTTPMCPSession{
		ID:        randomClaudeCLIMCPHex(16),
		Token:     randomClaudeCLIMCPHex(32),
		Groups:    groups,
		ExpiresAt: r.now().Add(ttl),
		servers:   make(map[string]*mcpsdk.Server, len(groups)),
		toolCalls: make(chan claudeCLIToolCall, 16),
		pending:   make(map[string]chan claudeCLIToolCallResult),
	}
	for _, group := range groups {
		session.servers[group.ServerName] = r.buildSDKServer(session, group)
	}
	r.mu.Lock()
	r.sessions[session.ID] = session
	r.mu.Unlock()
	toolNamesByServer := make(map[string][]string, len(groups))
	for _, group := range groups {
		for _, tool := range group.Tools {
			toolNamesByServer[group.ServerName] = append(toolNamesByServer[group.ServerName], tool.Name)
		}
	}
	logClaudeCLIMCPDebug("register session=%s tools=%v ttl=%s", session.ID, toolNamesByServer, ttl)
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

	sessionID, serverName, ok := parseClaudeCLIMCPPath(req.URL.Path)
	if !ok {
		http.NotFound(w, req)
		return
	}

	session, ok := r.get(sessionID)
	if !ok {
		logClaudeCLIMCPDebug("http session not found method=%s path=%s session=%s server=%s", req.Method, req.URL.Path, sessionID, serverName)
		http.NotFound(w, req)
		return
	}
	if req.Header.Get("Authorization") != "Bearer "+session.Token {
		logClaudeCLIMCPDebug("http unauthorized method=%s path=%s session=%s server=%s", req.Method, req.URL.Path, sessionID, serverName)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if req.Method == http.MethodDelete {
		logClaudeCLIMCPDebug("http delete session=%s server=%s", session.ID, serverName)
		r.Unregister(session.ID)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	method, toolName, body := claudeCLIMCPDebugHTTPPayload(req)
	logClaudeCLIMCPDebug("http post session=%s server=%s path=%s method=%s tool=%s", session.ID, serverName, req.URL.Path, method, toolName)
	logClaudeCLIDebugPayload("mcp.http.request", body)
	if normalized, changed, err := normalizeClaudeCLIMCPHTTPToolCallName(body); err != nil {
		logClaudeCLIMCPDebug("http normalize tool name failed session=%s err=%v", session.ID, err)
	} else if changed {
		logClaudeCLIDebugPayload("mcp.http.request.normalized", normalized)
		req.Body = io.NopCloser(bytes.NewReader(normalized))
		req.ContentLength = int64(len(normalized))
	}
	r.handler.ServeHTTP(w, req)
}

func parseClaudeCLIMCPPath(path string) (string, string, bool) {
	trimmed := strings.TrimPrefix(path, "/mcp/")
	if trimmed == "" || trimmed == path {
		return "", "", false
	}
	parts := strings.Split(trimmed, "/")
	if len(parts) == 1 && parts[0] != "" {
		return parts[0], claudeCLIInternalMCPServerName, true
	}
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		return parts[0], parts[1], true
	}
	return "", "", false
}

func claudeCLIMCPDebugHTTPPayload(req *http.Request) (string, string, []byte) {
	if req == nil || req.Body == nil {
		return "", "", nil
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		req.Body = io.NopCloser(bytes.NewReader(nil))
		return "read_error:" + err.Error(), "", nil
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	if len(body) == 0 {
		return "", "", nil
	}

	var payload struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "decode_error:" + err.Error(), "", body
	}
	return payload.Method, payload.Params.Name, body
}

func normalizeClaudeCLIMCPHTTPToolCallName(body []byte) ([]byte, bool, error) {
	if len(body) == 0 {
		return body, false, nil
	}
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, false, err
	}
	changed := normalizeClaudeCLIMCPJSONRPCPayloadToolCallName(payload)
	if !changed {
		return body, false, nil
	}
	normalized, err := json.Marshal(payload)
	if err != nil {
		return body, false, err
	}
	return normalized, true, nil
}

func normalizeClaudeCLIMCPJSONRPCPayloadToolCallName(payload any) bool {
	switch value := payload.(type) {
	case []any:
		changed := false
		for _, item := range value {
			if normalizeClaudeCLIMCPJSONRPCPayloadToolCallName(item) {
				changed = true
			}
		}
		return changed
	case map[string]any:
		method, _ := value["method"].(string)
		if method != "tools/call" {
			return false
		}
		params, ok := value["params"].(map[string]any)
		if !ok {
			return false
		}
		name, _ := params["name"].(string)
		serverName, toolName, ok := parseClaudeCLIMCPToolName(name)
		if !ok || serverName != claudeCLIInternalMCPServerName {
			return false
		}
		params["name"] = toolName
		return true
	default:
		return false
	}
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
	sessionID, serverName, ok := parseClaudeCLIMCPPath(req.URL.Path)
	if !ok {
		return nil
	}
	session, ok := r.get(sessionID)
	if !ok {
		return nil
	}
	return session.servers[serverName]
}

func (r *claudeCLIHTTPMCPRegistry) buildSDKServer(session *claudeCLIHTTPMCPSession, group claudeCLIToolGroup) *mcpsdk.Server {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "sub2api-client-tools", Version: "0.1.0"}, nil)
	for _, tool := range group.Tools {
		inputSchema := tool.InputSchema
		if inputSchema == nil {
			inputSchema = map[string]any{"type": "object"}
		}
		sdkTool := &mcpsdk.Tool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: inputSchema,
		}
		server.AddTool(sdkTool, r.sdkToolHandler(session.ID, group.ServerName))
	}
	return server
}

func (r *claudeCLIHTTPMCPRegistry) sdkToolHandler(sessionID, serverName string) mcpsdk.ToolHandler {
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
		logClaudeCLIDebugPayload("mcp.tool.arguments", input)
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
			Name:      claudeCLIClientToolNameForServer(serverName, name),
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
			logClaudeCLIDebugPayload("mcp.tool.result", result)
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

func (s *claudeCLIHTTPMCPServer) Register(groups []claudeCLIToolGroup, ttl time.Duration) ([]claudeCLIMCPConfigServer, claudeCLIHTTPMCPSession, func(), error) {
	if s == nil {
		return nil, claudeCLIHTTPMCPSession{}, nil, errors.New("claude cli mcp http server is nil")
	}
	baseURL, err := s.start()
	if err != nil {
		return nil, claudeCLIHTTPMCPSession{}, nil, err
	}
	session := s.registry.Register(groups, ttl)
	configServers := make([]claudeCLIMCPConfigServer, 0, len(session.Groups))
	for _, group := range session.Groups {
		configServers = append(configServers, claudeCLIMCPConfigServer{
			Name:  group.ServerName,
			URL:   baseURL + "/mcp/" + session.ID + "/" + group.ServerName,
			Token: session.Token,
		})
	}
	cleanup := func() {
		s.registry.Unregister(session.ID)
	}
	return configServers, session, cleanup, nil
}

type claudeCLIToolGroup struct {
	ServerName string
	Tools      []claudeCLITool
}

func splitClaudeCLIToolsByMCPServer(tools []claudeCLITool) []claudeCLIToolGroup {
	if len(tools) == 0 {
		return nil
	}
	groups := make([]claudeCLIToolGroup, 0)
	indexByServer := make(map[string]int)
	for _, tool := range tools {
		serverName := claudeCLIInternalMCPServerName
		toolName := tool.Name
		clientName := tool.ClientName
		if parsedServer, parsedTool, ok := parseClaudeCLIMCPToolName(tool.Name); ok && parsedServer != "" && parsedTool != "" {
			serverName = parsedServer
			toolName = parsedTool
			if clientName == "" {
				clientName = tool.Name
			}
		}
		groupIndex, ok := indexByServer[serverName]
		if !ok {
			groupIndex = len(groups)
			indexByServer[serverName] = groupIndex
			groups = append(groups, claudeCLIToolGroup{ServerName: serverName})
		}
		tool.Name = toolName
		tool.ClientName = clientName
		groups[groupIndex].Tools = append(groups[groupIndex].Tools, tool)
	}
	return groups
}

func normalizeClaudeCLIToolGroups(groups []claudeCLIToolGroup) []claudeCLIToolGroup {
	out := make([]claudeCLIToolGroup, 0, len(groups))
	for _, group := range groups {
		serverName := strings.TrimSpace(group.ServerName)
		if serverName == "" {
			serverName = claudeCLIInternalMCPServerName
		}
		tools := make([]claudeCLITool, len(group.Tools))
		copy(tools, group.Tools)
		out = append(out, claudeCLIToolGroup{ServerName: serverName, Tools: tools})
	}
	return out
}

func parseClaudeCLIMCPToolName(name string) (string, string, bool) {
	parts := strings.SplitN(strings.TrimSpace(name), "__", 3)
	if len(parts) != 3 || parts[0] != "mcp" || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func claudeCLIClientToolNameForServer(serverName, toolName string) string {
	if serverName == "" || serverName == claudeCLIInternalMCPServerName {
		return toolName
	}
	return "mcp__" + serverName + "__" + toolName
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
