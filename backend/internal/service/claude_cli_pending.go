package service

import (
	"context"
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
)

const defaultClaudeCLIPendingToolCallTTL = 5 * time.Minute
const maxClaudeCLIPendingToolRunHoldTTL = 55 * time.Second

type claudeCLIPendingToolResult struct {
	ToolUseID string
	Content   any
	IsError   bool
}

type claudeCLIPendingToolRun struct {
	proxy            *ClaudeCLIProxy
	mcpSessionID     string
	inputModel       string
	cancelRun        context.CancelFunc
	cancelWait       context.CancelFunc
	cleanupMCP       func()
	cleanupWorkspace func()
	stdoutDone       <-chan claudeCLIProcessOutput
	stdoutCollector  *claudeCLIOutputCollector
	errCh            <-chan error
	processDone      <-chan struct{}
	toolCallCh       <-chan claudeCLIToolCall
	timer            *time.Timer
	toolUseIDs       map[string]struct{}
	returnedIDs      []string
	closed           bool
}

func claudeCLIPendingToolCallTTL(account *Account) time.Duration {
	if account == nil {
		return defaultClaudeCLIPendingToolCallTTL
	}
	minutes := account.GetSessionIdleTimeoutMinutes()
	if minutes <= 0 {
		return defaultClaudeCLIPendingToolCallTTL
	}
	return time.Duration(minutes) * time.Minute
}

func claudeCLIPendingToolRunHoldTTL(account *Account) time.Duration {
	ttl := claudeCLIPendingToolCallTTL(account)
	if ttl <= 0 || ttl > maxClaudeCLIPendingToolRunHoldTTL {
		return maxClaudeCLIPendingToolRunHoldTTL
	}
	return ttl
}

func extractClaudeCLIPendingToolResults(parsed *ParsedRequest) []claudeCLIPendingToolResult {
	if parsed == nil || len(parsed.Messages) == 0 {
		return nil
	}
	var results []claudeCLIPendingToolResult
	for _, raw := range parsed.Messages {
		message, ok, err := convertAnthropicMessage(raw)
		if err != nil || !ok || message.Type != "user" {
			continue
		}
		for _, block := range message.Message.Content {
			if block.Type != "tool_result" || block.ToolUseID == "" {
				continue
			}
			results = append(results, claudeCLIPendingToolResult{
				ToolUseID: block.ToolUseID,
				Content:   block.Content,
				IsError:   block.IsError,
			})
		}
	}
	return results
}

func (p *ClaudeCLIProxy) takePendingToolResults(parsed *ParsedRequest) (*claudeCLIPendingToolRun, []claudeCLIPendingToolResult) {
	results := extractClaudeCLIPendingToolResults(parsed)
	if len(results) == 0 || p == nil {
		return nil, nil
	}

	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()

	var pending *claudeCLIPendingToolRun
	matched := make([]claudeCLIPendingToolResult, 0, len(results))
	for _, result := range results {
		run := p.pendingByToolUseID[result.ToolUseID]
		if run == nil {
			continue
		}
		if pending == nil {
			pending = run
		}
		if pending != run {
			continue
		}
		delete(p.pendingByToolUseID, result.ToolUseID)
		if pending.toolUseIDs != nil {
			delete(pending.toolUseIDs, result.ToolUseID)
		}
		matched = append(matched, result)
	}
	if pending == nil || len(matched) == 0 {
		return nil, nil
	}
	if pending.timer != nil {
		pending.timer.Stop()
	}
	return pending, matched
}

func (p *ClaudeCLIProxy) registerPendingToolCalls(run *claudeCLIPendingToolRun, calls []claudeCLIToolCall, ttl time.Duration) {
	if p == nil || run == nil || len(calls) == 0 {
		return
	}
	if ttl <= 0 {
		ttl = defaultClaudeCLIPendingToolCallTTL
	}

	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()

	if p.pendingByToolUseID == nil {
		p.pendingByToolUseID = make(map[string]*claudeCLIPendingToolRun)
	}
	if run.toolUseIDs == nil {
		run.toolUseIDs = make(map[string]struct{})
	}
	if run.closed {
		return
	}
	for _, call := range calls {
		if call.ID == "" {
			continue
		}
		p.pendingByToolUseID[call.ID] = run
		run.toolUseIDs[call.ID] = struct{}{}
		run.returnedIDs = append(run.returnedIDs, call.ID)
	}
	if run.timer != nil {
		run.timer.Stop()
	}
	run.timer = time.AfterFunc(ttl, func() {
		p.closePendingToolRun(run, true)
	})
}

func (p *ClaudeCLIProxy) closePendingToolRun(run *claudeCLIPendingToolRun, cancelProcess bool) {
	if run == nil {
		return
	}

	if p != nil {
		p.pendingMu.Lock()
		if run.closed {
			p.pendingMu.Unlock()
			return
		}
		run.closed = true
		for id := range run.toolUseIDs {
			delete(p.pendingByToolUseID, id)
		}
		run.toolUseIDs = nil
		if run.timer != nil {
			run.timer.Stop()
			run.timer = nil
		}
		p.pendingMu.Unlock()
	} else if run.closed {
		return
	} else {
		run.closed = true
	}

	if run.cancelWait != nil {
		run.cancelWait()
	}
	if cancelProcess && run.cancelRun != nil {
		run.cancelRun()
	}
	if run.cleanupMCP != nil {
		run.cleanupMCP()
	}
	if cancelProcess {
		go cleanupClaudeCLIProcessAfterToolUse(run.stdoutDone, run.errCh, run.processDone, run.cleanupWorkspace)
		return
	}
	if run.cleanupWorkspace != nil {
		run.cleanupWorkspace()
	}
}

func (p *ClaudeCLIProxy) forwardPendingToolResults(ctx context.Context, c *gin.Context, account *Account, parsed *ParsedRequest, startTime time.Time, input *claudeCLIInput, pending *claudeCLIPendingToolRun, results []claudeCLIPendingToolResult) (*ForwardResult, error) {
	if pending == nil || len(results) == 0 {
		return nil, fmt.Errorf("claude cli proxy: pending tool result not found")
	}
	registry := p.getMCPServer(time.Now).registry
	for _, result := range results {
		if err := registry.CompleteToolCall(result.ToolUseID, claudeCLIToolCallResult{
			Content: result.Content,
			IsError: result.IsError,
		}); err != nil {
			p.closePendingToolRun(pending, true)
			return nil, err
		}
	}

	if parsed != nil && parsed.OnUpstreamAccepted != nil {
		parsed.OnUpstreamAccepted()
	}

	for {
		select {
		case call := <-pending.toolCallCh:
			if pending.hasReturnedToolUseID(call.ID) {
				continue
			}
			calls := collectClaudeCLIToolCalls(ctx, pending.toolCallCh, call)
			calls = pending.filterNewToolCalls(calls)
			if len(calls) == 0 {
				continue
			}
			p.registerPendingToolCalls(pending, calls, claudeCLIPendingToolRunHoldTTL(account))
			prefixBlocks := claudeCLIToolUsePrefixBlocks(ctx, pending.stdoutCollector, calls)
			if err := writeClaudeCLIToolUseResponse(c, input, parsed, calls, prefixBlocks); err != nil {
				p.closePendingToolRun(pending, true)
				return nil, err
			}
			return &ForwardResult{
				Model:         parsed.Model,
				UpstreamModel: pending.inputModel,
				Stream:        parsed.Stream,
				Duration:      time.Since(startTime),
			}, nil
		case runErr := <-pending.errCh:
			output := <-pending.stdoutDone
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
					return p.writeClaudeCLITranslatedOutput(c, parsed, pending.inputModel, startTime, filtered, claudeCLITranslateOptions{})
				}
				return nil, formatClaudeCLIRunError(runErr, "", string(output.data))
			}
			return p.writeClaudeCLITranslatedOutput(c, parsed, pending.inputModel, startTime, filtered, claudeCLITranslateOptions{})
		case <-ctx.Done():
			p.closePendingToolRun(pending, true)
			return nil, ctx.Err()
		}
	}
}

func (r *claudeCLIPendingToolRun) hasReturnedToolUseID(id string) bool {
	if r == nil || id == "" {
		return false
	}
	for _, returnedID := range r.returnedIDs {
		if returnedID == id {
			return true
		}
	}
	return false
}

func (r *claudeCLIPendingToolRun) filterNewToolCalls(calls []claudeCLIToolCall) []claudeCLIToolCall {
	if r == nil || len(calls) == 0 {
		return calls
	}
	out := calls[:0]
	for _, call := range calls {
		if !r.hasReturnedToolUseID(call.ID) {
			out = append(out, call)
		}
	}
	return out
}
