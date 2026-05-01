package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
)

const (
	defaultClaudeCLIToolCallBatchWait      = 25 * time.Millisecond
	defaultClaudeCLIToolCallChannelBufSize = 16
)

type claudeCLIProxyRunner interface {
	Run(ctx context.Context, req claudeCLIProcessRequest) error
}

type ClaudeCLIProxy struct {
	runner             claudeCLIProxyRunner
	tempDir            string
	now                func() time.Time
	newSessionID       func() string
	mcpMu              sync.Mutex
	mcpServer          *claudeCLIHTTPMCPServer
	pendingMu          sync.Mutex
	pendingByToolUseID map[string]*claudeCLIPendingToolRun
}

func NewClaudeCLIProxy() *ClaudeCLIProxy {
	return &ClaudeCLIProxy{
		runner:       claudeCLIProcessRunner{},
		now:          time.Now,
		newSessionID: newClaudeCLISessionID,
		mcpServer:    newClaudeCLIHTTPMCPServer(time.Now),
	}
}

type claudeCLIProcessOutput struct {
	data []byte
	err  error
}

func (p *ClaudeCLIProxy) Forward(ctx context.Context, c *gin.Context, account *Account, parsed *ParsedRequest, startTime time.Time) (*ForwardResult, error) {
	if account == nil {
		return nil, fmt.Errorf("claude cli proxy: account is nil")
	}
	if !account.IsClaudeCLIProxyEnabled() {
		return nil, fmt.Errorf("claude cli proxy: account is not enabled")
	}
	if p == nil {
		return nil, fmt.Errorf("claude cli proxy: proxy is nil")
	}
	auth, err := account.GetClaudeCLIAuth()
	if err != nil {
		return nil, err
	}

	input, err := buildClaudeCLIInput(parsed)
	if err != nil {
		return nil, err
	}
	if mappedModel := strings.TrimSpace(account.GetMappedModel(input.Model)); mappedModel != "" {
		input.Model = mappedModel
	}

	if pending, results := p.takePendingToolResults(parsed); pending != nil {
		return p.forwardPendingToolResults(ctx, c, account, parsed, startTime, input, pending, results)
	}

	now := time.Now
	if p.now != nil {
		now = p.now
	}
	newSessionID := newClaudeCLISessionID
	if p.newSessionID != nil {
		newSessionID = p.newSessionID
	}
	sessionID := resolveClaudeCLISessionID(c, parsed, newSessionID)

	var mcpSession claudeCLIHTTPMCPSession
	var cleanupMCP func()
	mcpConfig := ""
	if len(input.Tools) > 0 {
		mcpServer := p.getMCPServer(now)
		mcpEndpoint, session, cleanup, err := mcpServer.Register(input.Tools, claudeCLIPendingToolCallTTL(account))
		if err != nil {
			return nil, err
		}
		mcpSession = session
		cleanupMCP = cleanup
		logClaudeCLIMCPDebug("proxy registered mcp session=%s endpoint=%s tool_count=%d", mcpSession.ID, mcpEndpoint, len(input.Tools))
		mcpConfig, err = buildClaudeCLIMCPConfig(mcpEndpoint, mcpSession.Token)
		if err != nil {
			if cleanupMCP != nil {
				cleanupMCP()
			}
			return nil, err
		}
	}
	ws, err := prepareClaudeCLIWorkspace(p.tempDir, input, claudeCLIWorkspaceOptions{
		SessionID: sessionID,
		Now:       now,
		MCPConfig: mcpConfig,
		UserID:    account.GetClaudeCLIUserID(),
	})
	if err != nil {
		if cleanupMCP != nil {
			cleanupMCP()
		}
		return nil, err
	}
	if isClaudeCLIDebugEnabled() {
		stdinPreview := truncateString(string(ws.Stdin), 4096)
		logger.LegacyPrintf(
			"service.claude_cli",
			"prepared claude cli workspace dir=%s system_prompt=%s mcp_config=%s stdin_preview=%s",
			ws.Dir,
			ws.SystemPromptPath,
			ws.MCPConfigPath,
			stdinPreview,
		)
	}

	stdin := bytes.NewReader(ws.Stdin)

	var stderr bytes.Buffer
	runner := p.runner
	if runner == nil {
		runner = claudeCLIProcessRunner{}
	}

	if len(input.Tools) > 0 {
		return p.forwardWithClientTools(ctx, c, account, parsed, startTime, input, auth, ws, stdin, &stderr, runner, mcpSession.ID, cleanupMCP)
	}
	defer ws.Cleanup()
	if cleanupMCP != nil {
		defer cleanupMCP()
	}

	if input.Stream {
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Status(http.StatusOK)
		if flusher, ok := c.Writer.(http.Flusher); ok {
			flusher.Flush()
		}
		streamWriter := io.Writer(c.Writer)
		if flusher, ok := c.Writer.(http.Flusher); ok {
			streamWriter = claudeCLIFlushingWriter{w: c.Writer, flusher: flusher}
		}

		stdoutReader, stdoutWriter := io.Pipe()
		stdoutPreview := &claudeCLIOutputPreview{limit: 8192}
		errCh := make(chan error, 1)
		go func() {
			runErr := runner.Run(ctx, claudeCLIProcessRequest{
				Command:  account.GetClaudeCLICommand(),
				Args:     ws.Args,
				Auth:     auth,
				ProxyURL: claudeCLIAccountProxyURL(account),
				Dir:      ws.Dir,
				Stdin:    stdin,
				Stdout:   stdoutWriter,
				Stderr:   &stderr,
			})
			_ = stdoutWriter.Close()
			errCh <- runErr
		}()

		if parsed != nil && parsed.OnUpstreamAccepted != nil {
			parsed.OnUpstreamAccepted()
		}

		usage, translateErr := translateClaudeCLIStream(io.TeeReader(stdoutReader, stdoutPreview), streamWriter, withClaudeCLIResponseModel(claudeCLITranslateOptions{
			Stream: true,
		}, parsed, input.Model))
		if translateErr != nil {
			_ = stdoutReader.CloseWithError(translateErr)
		}
		runErr := <-errCh
		claudeCLIUserID := readClaudeCLIWorkspaceUserIDForResult(ws)
		if translateErr != nil {
			if stderr.Len() > 0 {
				if stdout := stdoutPreview.String(); stdout != "" {
					return nil, fmt.Errorf("%w: stderr=%s stdout=%s", translateErr, stderr.String(), stdout)
				}
				return nil, fmt.Errorf("%w: stderr=%s", translateErr, stderr.String())
			}
			if stdout := stdoutPreview.String(); stdout != "" {
				return nil, fmt.Errorf("%w: stdout=%s", translateErr, stdout)
			}
			return nil, translateErr
		}
		if runErr != nil {
			if stdoutPreview.String() != "" {
				result := &ForwardResult{
					Usage:           usage,
					Model:           parsed.Model,
					UpstreamModel:   input.Model,
					Stream:          parsed.Stream,
					Duration:        time.Since(startTime),
					ClaudeCLIUserID: claudeCLIUserID,
				}
				p.applyRateLimitSignal(result, []byte(stdoutPreview.String()))
				return result, nil
			}
			return nil, formatClaudeCLIRunError(runErr, stderr.String(), stdoutPreview.String())
		}

		result := &ForwardResult{
			Usage:           usage,
			Model:           parsed.Model,
			UpstreamModel:   input.Model,
			Stream:          parsed.Stream,
			Duration:        time.Since(startTime),
			ClaudeCLIUserID: claudeCLIUserID,
		}
		p.applyRateLimitSignal(result, []byte(stdoutPreview.String()))
		return result, nil
	}

	var stdout bytes.Buffer
	if err := runner.Run(ctx, claudeCLIProcessRequest{
		Command:  account.GetClaudeCLICommand(),
		Args:     ws.Args,
		Auth:     auth,
		ProxyURL: claudeCLIAccountProxyURL(account),
		Dir:      ws.Dir,
		Stdin:    stdin,
		Stdout:   &stdout,
		Stderr:   &stderr,
	}); err != nil {
		claudeCLIUserID := readClaudeCLIWorkspaceUserIDForResult(ws)
		if stdout.Len() > 0 {
			result, writeErr := p.writeClaudeCLITranslatedOutput(c, parsed, input.Model, startTime, stdout.Bytes(), claudeCLITranslateOptions{})
			if result != nil {
				result.ClaudeCLIUserID = claudeCLIUserID
			}
			return result, writeErr
		}
		return nil, formatClaudeCLIRunError(err, stderr.String(), stdout.String())
	}
	claudeCLIUserID := readClaudeCLIWorkspaceUserIDForResult(ws)

	if parsed != nil && parsed.OnUpstreamAccepted != nil {
		parsed.OnUpstreamAccepted()
	}

	translated, usage, err := translateClaudeCLIOutputForNonStream(stdout.Bytes(), withClaudeCLIResponseModel(claudeCLITranslateOptions{}, parsed, input.Model))
	if err != nil {
		if stderr.Len() > 0 {
			return nil, fmt.Errorf("%w: stderr=%s", err, stderr.String())
		}
		if stdout.Len() > 0 {
			return nil, fmt.Errorf("%w: stdout=%s", err, truncateString(stdout.String(), 8192))
		}
		return nil, err
	}

	c.Writer.Header().Set("Content-Type", "application/json")
	c.Status(http.StatusOK)
	if _, err := c.Writer.Write(translated); err != nil {
		return nil, fmt.Errorf("claude cli proxy: write response: %w", err)
	}

	result := &ForwardResult{
		Usage:           usage,
		Model:           parsed.Model,
		UpstreamModel:   input.Model,
		Stream:          parsed.Stream,
		Duration:        time.Since(startTime),
		ClaudeCLIUserID: claudeCLIUserID,
	}
	p.applyRateLimitSignal(result, stdout.Bytes())
	return result, nil
}

func (p *ClaudeCLIProxy) forwardWithClientTools(ctx context.Context, c *gin.Context, account *Account, parsed *ParsedRequest, startTime time.Time, input *claudeCLIInput, auth claudeCLIProcessAuth, ws *claudeCLIWorkspace, stdin io.Reader, stderr *bytes.Buffer, runner claudeCLIProxyRunner, mcpSessionID string, cleanupMCP func()) (*ForwardResult, error) {
	runCtx, cancelRun := context.WithCancel(context.Background())
	stdoutReader, stdoutWriter := io.Pipe()
	stdoutCollector := newClaudeCLIOutputCollector()
	stdoutDone := make(chan claudeCLIProcessOutput, 1)
	go func() {
		err := stdoutCollector.collectFrom(stdoutReader)
		stdoutDone <- claudeCLIProcessOutput{data: stdoutCollector.Bytes(), err: err}
	}()

	errCh := make(chan error, 1)
	processDone := make(chan struct{})
	go func() {
		runErr := runner.Run(runCtx, claudeCLIProcessRequest{
			Command:  account.GetClaudeCLICommand(),
			Args:     ws.Args,
			Auth:     auth,
			ProxyURL: claudeCLIAccountProxyURL(account),
			Dir:      ws.Dir,
			Stdin:    stdin,
			Stdout:   stdoutWriter,
			Stderr:   stderr,
		})
		_ = stdoutWriter.Close()
		errCh <- runErr
		close(processDone)
	}()

	waitCtx, cancelWait := context.WithCancel(context.Background())
	toolCallCh := make(chan claudeCLIToolCall, defaultClaudeCLIToolCallChannelBufSize)
	go func() {
		for {
			call, err := p.getMCPServer(time.Now).registry.WaitToolCall(waitCtx, mcpSessionID)
			if err != nil {
				logClaudeCLIMCPDebug("proxy wait stopped session=%s err=%v", mcpSessionID, err)
				return
			}
			select {
			case toolCallCh <- call:
				logClaudeCLIMCPDebug("proxy queued call session=%s call_id=%s name=%s", mcpSessionID, call.ID, call.Name)
			case <-waitCtx.Done():
				logClaudeCLIMCPDebug("proxy queue context done session=%s call_id=%s name=%s err=%v", mcpSessionID, call.ID, call.Name, waitCtx.Err())
				return
			}
		}
	}()
	pending := &claudeCLIPendingToolRun{
		proxy:        p,
		mcpSessionID: mcpSessionID,
		inputModel:   input.Model,
		cancelRun:    cancelRun,
		cancelWait:   cancelWait,
		cleanupMCP:   cleanupMCP,
		cleanupWorkspace: func() {
			if ws != nil {
				ws.Cleanup()
			}
		},
		stdoutDone:      stdoutDone,
		stdoutCollector: stdoutCollector,
		errCh:           errCh,
		processDone:     processDone,
		toolCallCh:      toolCallCh,
	}

	select {
	case call := <-toolCallCh:
		logClaudeCLIMCPDebug("proxy received call session=%s call_id=%s name=%s", mcpSessionID, call.ID, call.Name)
		calls := collectClaudeCLIToolCalls(ctx, toolCallCh, call)
		p.registerPendingToolCalls(pending, calls, claudeCLIPendingToolRunHoldTTL(account))
		if parsed != nil && parsed.OnUpstreamAccepted != nil {
			parsed.OnUpstreamAccepted()
		}
		prefixBlocks := claudeCLIToolUsePrefixBlocks(ctx, stdoutCollector, calls)
		if err := writeClaudeCLIToolUseResponse(c, input, parsed, calls, prefixBlocks); err != nil {
			p.closePendingToolRun(pending, true)
			return nil, err
		}
		return &ForwardResult{
			Model:           parsed.Model,
			UpstreamModel:   input.Model,
			Stream:          parsed.Stream,
			Duration:        time.Since(startTime),
			ClaudeCLIUserID: readClaudeCLIWorkspaceUserIDForResult(ws),
		}, nil
	case runErr := <-errCh:
		output := <-stdoutDone
		p.closePendingToolRun(pending, false)
		if output.err != nil {
			return nil, fmt.Errorf("claude cli proxy: read stdout: %w", output.err)
		}
		filtered, err := filterClaudeCLIOutputForCompletedToolUses(output.data, pending.returnedIDs)
		if err != nil {
			return nil, err
		}
		if runErr != nil {
			if len(filtered) > 0 {
				result, writeErr := p.writeClaudeCLITranslatedOutput(c, parsed, input.Model, startTime, filtered, claudeCLITranslateOptions{})
				if result != nil {
					result.ClaudeCLIUserID = readClaudeCLIWorkspaceUserIDForResult(ws)
				}
				return result, writeErr
			}
			return nil, formatClaudeCLIRunError(runErr, stderr.String(), string(output.data))
		}
		if parsed != nil && parsed.OnUpstreamAccepted != nil {
			parsed.OnUpstreamAccepted()
		}
		result, writeErr := p.writeClaudeCLITranslatedOutput(c, parsed, input.Model, startTime, filtered, claudeCLITranslateOptions{})
		if result != nil {
			result.ClaudeCLIUserID = readClaudeCLIWorkspaceUserIDForResult(ws)
		}
		return result, writeErr
	case <-ctx.Done():
		p.closePendingToolRun(pending, true)
		return nil, ctx.Err()
	}
}

func (p *ClaudeCLIProxy) writeClaudeCLITranslatedOutput(c *gin.Context, parsed *ParsedRequest, upstreamModel string, startTime time.Time, output []byte, opts claudeCLITranslateOptions) (*ForwardResult, error) {
	result, err := writeClaudeCLITranslatedOutput(c, parsed, upstreamModel, startTime, output, opts)
	p.applyRateLimitSignal(result, output)
	return result, err
}

func (p *ClaudeCLIProxy) applyRateLimitSignal(result *ForwardResult, output []byte) {
	if result == nil || len(output) == 0 {
		return
	}
	now := time.Now
	if p != nil && p.now != nil {
		now = p.now
	}
	signal := detectClaudeCLIRateLimit(output, now())
	if signal == nil {
		return
	}
	resetAt := signal.ResetAt
	result.RateLimitResetAt = &resetAt
}

func cleanupClaudeCLIProcessAfterToolUse(stdoutDone <-chan claudeCLIProcessOutput, errCh <-chan error, processDone <-chan struct{}, cleanup func()) {
	if processDone != nil {
		<-processDone
	}
	if stdoutDone != nil {
		<-stdoutDone
	}
	if errCh != nil {
		<-errCh
	}
	if cleanup != nil {
		cleanup()
	}
}

type claudeCLIOutputPreview struct {
	limit int
	buf   bytes.Buffer
}

func (p *claudeCLIOutputPreview) Write(data []byte) (int, error) {
	if p == nil {
		return len(data), nil
	}
	remaining := p.limit - p.buf.Len()
	if remaining > 0 {
		if len(data) < remaining {
			remaining = len(data)
		}
		_, _ = p.buf.Write(data[:remaining])
	}
	return len(data), nil
}

func (p *claudeCLIOutputPreview) String() string {
	if p == nil {
		return ""
	}
	return p.buf.String()
}

func formatClaudeCLIRunError(runErr error, stderr, stdout string) error {
	if runErr == nil {
		return nil
	}
	stderr = strings.TrimSpace(stderr)
	stdout = strings.TrimSpace(stdout)
	switch {
	case stderr != "" && stdout != "":
		return fmt.Errorf("claude cli proxy: run: %w: stderr=%s stdout=%s", runErr, stderr, truncateString(stdout, 8192))
	case stderr != "":
		return fmt.Errorf("claude cli proxy: run: %w: stderr=%s", runErr, stderr)
	case stdout != "":
		return fmt.Errorf("claude cli proxy: run: %w: stdout=%s", runErr, truncateString(stdout, 8192))
	default:
		return fmt.Errorf("claude cli proxy: run: %w", runErr)
	}
}

func collectClaudeCLIToolCalls(ctx context.Context, ch <-chan claudeCLIToolCall, first claudeCLIToolCall) []claudeCLIToolCall {
	calls := []claudeCLIToolCall{first}
	timer := time.NewTimer(defaultClaudeCLIToolCallBatchWait)
	defer timer.Stop()
	for {
		select {
		case call := <-ch:
			calls = append(calls, call)
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(defaultClaudeCLIToolCallBatchWait)
		case <-timer.C:
			return calls
		case <-ctx.Done():
			return calls
		}
	}
}

func claudeCLIAccountProxyURL(account *Account) string {
	if account == nil || account.Proxy == nil {
		return ""
	}
	return account.Proxy.URL()
}

func writeClaudeCLIToolUseResponse(c *gin.Context, input *claudeCLIInput, parsed *ParsedRequest, calls []claudeCLIToolCall, prefixBlocks []map[string]any) error {
	if len(calls) == 0 {
		return nil
	}
	if parsed != nil && parsed.Stream {
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Status(http.StatusOK)
		events := []json.RawMessage{
			json.RawMessage(fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_%s","role":"assistant","model":%q,"content":[],"usage":{"input_tokens":0,"output_tokens":0}}}`, calls[0].ID, input.Model)),
		}
		visibleIndex := 0
		for _, block := range prefixBlocks {
			var err error
			events, err = appendClaudeCLISyntheticBlockEvents(events, visibleIndex, block)
			if err != nil {
				return err
			}
			visibleIndex++
		}
		for _, call := range calls {
			events = append(events,
				mustClaudeCLIRawJSON(map[string]any{"type": "content_block_start", "index": visibleIndex, "content_block": map[string]any{"type": "tool_use", "id": call.ID, "name": call.Name, "input": map[string]any{}}}),
				mustClaudeCLIRawJSON(map[string]any{"type": "content_block_delta", "index": visibleIndex, "delta": map[string]any{"type": "input_json_delta", "partial_json": mustClaudeCLIJSONString(call.Input)}}),
				mustClaudeCLIRawJSON(map[string]any{"type": "content_block_stop", "index": visibleIndex}),
			)
			visibleIndex++
		}
		events = append(events,
			json.RawMessage(`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":0}}`),
			json.RawMessage(`{"type":"message_stop"}`),
		)
		for _, event := range events {
			var eventType struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(event, &eventType)
			if err := writeAnthropicSSEEvent(c.Writer, eventType.Type, event); err != nil {
				return err
			}
		}
		return nil
	}

	c.Writer.Header().Set("Content-Type", "application/json")
	c.Status(http.StatusOK)
	content := make([]any, 0, len(prefixBlocks)+len(calls))
	for _, block := range prefixBlocks {
		if err := validateClaudeCLIBlockForSyntheticResponse(block); err != nil {
			return err
		}
		content = append(content, cloneClaudeCLIBlock(block))
	}
	for _, call := range calls {
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    call.ID,
			"name":  call.Name,
			"input": call.Input,
		})
	}
	message := map[string]any{
		"id":            "msg_" + calls[0].ID,
		"type":          "message",
		"role":          "assistant",
		"model":         input.Model,
		"content":       content,
		"stop_reason":   "tool_use",
		"stop_sequence": nil,
		"usage":         claudeCLIResponseUsage(ClaudeUsage{}),
	}
	return json.NewEncoder(c.Writer).Encode(message)
}

func appendClaudeCLISyntheticBlockEvents(events []json.RawMessage, index int, block map[string]any) ([]json.RawMessage, error) {
	if err := validateClaudeCLIBlockForSyntheticResponse(block); err != nil {
		return nil, err
	}
	block = cloneClaudeCLIBlock(block)
	blockType, _ := block["type"].(string)
	switch blockType {
	case "thinking":
		thinking, _ := block["thinking"].(string)
		signature, _ := block["signature"].(string)
		block["thinking"] = ""
		events = append(events, mustClaudeCLIRawJSON(map[string]any{"type": "content_block_start", "index": index, "content_block": block}))
		if thinking != "" {
			events = append(events, mustClaudeCLIRawJSON(map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "thinking_delta", "thinking": thinking}}))
		}
		events = append(events, mustClaudeCLIRawJSON(map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "signature_delta", "signature": signature}}))
	case "text":
		text, _ := block["text"].(string)
		block["text"] = ""
		events = append(events, mustClaudeCLIRawJSON(map[string]any{"type": "content_block_start", "index": index, "content_block": block}))
		if text != "" {
			events = append(events, mustClaudeCLIRawJSON(map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "text_delta", "text": text}}))
		}
	default:
		events = append(events, mustClaudeCLIRawJSON(map[string]any{"type": "content_block_start", "index": index, "content_block": block}))
	}
	events = append(events, mustClaudeCLIRawJSON(map[string]any{"type": "content_block_stop", "index": index}))
	return events, nil
}

func writeClaudeCLITranslatedOutput(c *gin.Context, parsed *ParsedRequest, upstreamModel string, startTime time.Time, output []byte, opts claudeCLITranslateOptions) (*ForwardResult, error) {
	stream := parsed != nil && parsed.Stream
	opts = withClaudeCLIResponseModel(opts, parsed, upstreamModel)
	opts.Stream = stream
	if stream {
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Status(http.StatusOK)
		usage, err := translateClaudeCLIStream(bytes.NewReader(output), c.Writer, opts)
		if err != nil {
			return nil, err
		}
		result := &ForwardResult{
			Usage:         usage,
			UpstreamModel: upstreamModel,
			Stream:        stream,
			Duration:      time.Since(startTime),
		}
		if parsed != nil {
			result.Model = parsed.Model
		}
		return result, nil
	} else {
		var translated bytes.Buffer
		usage, err := translateClaudeCLIStream(bytes.NewReader(output), &translated, opts)
		if err != nil {
			if len(output) > 0 {
				return nil, fmt.Errorf("%w: stdout=%s", err, truncateString(string(output), 8192))
			}
			return nil, err
		}
		c.Writer.Header().Set("Content-Type", "application/json")
		c.Status(http.StatusOK)
		if _, err := c.Writer.Write(translated.Bytes()); err != nil {
			return nil, fmt.Errorf("claude cli proxy: write response: %w", err)
		}
		result := &ForwardResult{
			Usage:         usage,
			UpstreamModel: upstreamModel,
			Stream:        stream,
			Duration:      time.Since(startTime),
		}
		if parsed != nil {
			result.Model = parsed.Model
		}
		return result, nil
	}
}

func withClaudeCLIResponseModel(opts claudeCLITranslateOptions, parsed *ParsedRequest, upstreamModel string) claudeCLITranslateOptions {
	if opts.ResponseModel == "" && parsed != nil {
		opts.ResponseModel = parsed.Model
	}
	if opts.UpstreamModel == "" {
		opts.UpstreamModel = upstreamModel
	}
	return opts
}

func translateClaudeCLIOutputForNonStream(output []byte, opts claudeCLITranslateOptions) ([]byte, ClaudeUsage, error) {
	var translated bytes.Buffer
	opts.Stream = false
	usage, err := translateClaudeCLIStream(bytes.NewReader(output), &translated, opts)
	if err != nil {
		return nil, usage, err
	}
	return translated.Bytes(), usage, nil
}

func mustClaudeCLIJSONString(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func mustClaudeCLIRawJSON(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return json.RawMessage(encoded)
}

func (p *ClaudeCLIProxy) getMCPServer(now func() time.Time) *claudeCLIHTTPMCPServer {
	p.mcpMu.Lock()
	defer p.mcpMu.Unlock()
	if p.mcpServer == nil {
		p.mcpServer = newClaudeCLIHTTPMCPServer(now)
	}
	return p.mcpServer
}

func isClaudeCLIDebugEnabled() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv("SUB2API_CLAUDE_CLI_DEBUG")))
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

type claudeCLIFlushingWriter struct {
	w       io.Writer
	flusher http.Flusher
}

func (w claudeCLIFlushingWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if n > 0 && w.flusher != nil {
		w.flusher.Flush()
	}
	return n, err
}

func newClaudeCLISessionID() string {
	var b [16]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return deriveClaudeCLIMessageUUID(fmt.Sprintf("%d", time.Now().UnixNano()), 0)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80

	hexValue := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hexValue[0:8],
		hexValue[8:12],
		hexValue[12:16],
		hexValue[16:20],
		hexValue[20:32],
	)
}

func resolveClaudeCLISessionID(c *gin.Context, parsed *ParsedRequest, fallback func() string) string {
	if c != nil && c.Request != nil {
		if sessionID := normalizeClaudeCLIClientSessionID(c.GetHeader("X-Claude-Code-Session-Id")); sessionID != "" {
			return sessionID
		}
	}
	if parsed != nil && parsed.MetadataUserID != "" {
		if uid := ParseMetadataUserID(parsed.MetadataUserID); uid != nil {
			if sessionID := normalizeClaudeCLIClientSessionID(uid.SessionID); sessionID != "" {
				return sessionID
			}
		}
	}
	if fallback != nil {
		return fallback()
	}
	return newClaudeCLISessionID()
}

func normalizeClaudeCLIClientSessionID(sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if validateClaudeCLISessionIDForPath(sessionID) != nil {
		return ""
	}
	return sessionID
}
