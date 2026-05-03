package service

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

const claudeCLIStreamMaxLineSize = 10 * 1024 * 1024

type claudeCLITranslateOptions struct {
	Stream        bool
	ResponseModel string
	UpstreamModel string
}

type claudeCLIStreamEnvelope struct {
	Type  string          `json:"type"`
	Event json.RawMessage `json:"event"`
}

type claudeCLIStreamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message struct {
		ID      string         `json:"id"`
		Role    string         `json:"role"`
		Model   string         `json:"model"`
		Content []any          `json:"content"`
		Usage   map[string]any `json:"usage"`
	} `json:"message"`
	ContentBlock map[string]any `json:"content_block"`
	Delta        struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage map[string]any `json:"usage"`
}

type claudeCLIFinalAssistantEnvelope struct {
	Type    string `json:"type"`
	Message struct {
		ID           string           `json:"id"`
		Type         string           `json:"type"`
		Role         string           `json:"role"`
		Model        string           `json:"model"`
		Content      []map[string]any `json:"content"`
		StopReason   string           `json:"stop_reason"`
		StopSequence any              `json:"stop_sequence"`
		Usage        map[string]any   `json:"usage"`
	} `json:"message"`
}

func translateClaudeCLIStream(r io.Reader, w io.Writer, opts claudeCLITranslateOptions) (ClaudeUsage, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), claudeCLIStreamMaxLineSize)

	var (
		messageID        string
		model            string
		role             = "assistant"
		stopReason       string
		usage            ClaudeUsage
		blocksByIndex    = map[int]map[string]any{}
		inputJSONByIndex = map[int]string{}
		sawStreamEvent   bool
		finalAssistant   *claudeCLIFinalAssistantEnvelope
	)

	writeStreamEvent := func(event claudeCLIStreamEvent, raw json.RawMessage) error {
		if !opts.Stream {
			return nil
		}
		if event.Type == "content_block_start" {
			var err error
			raw, err = rewriteClaudeCLIStreamToolUseName(raw)
			if err != nil {
				return err
			}
		}
		if event.Type == "message_start" {
			var err error
			raw, err = rewriteClaudeCLIStreamMessageStartModel(raw, opts)
			if err != nil {
				return err
			}
		}
		return writeAnthropicSSEEvent(w, event.Type, raw)
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var envelope claudeCLIStreamEnvelope
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			return usage, fmt.Errorf("claude cli stream: decode wrapper: %w", err)
		}
		if envelope.Type != "stream_event" {
			if envelope.Type == "assistant" {
				var assistant claudeCLIFinalAssistantEnvelope
				if err := json.Unmarshal([]byte(line), &assistant); err != nil {
					return usage, fmt.Errorf("claude cli stream: decode assistant: %w", err)
				}
				finalAssistant = &assistant
				mergeClaudeCLIUsage(&usage, assistant.Message.Usage, false)
			}
			continue
		}
		sawStreamEvent = true

		var event claudeCLIStreamEvent
		if err := json.Unmarshal(envelope.Event, &event); err != nil {
			return usage, fmt.Errorf("claude cli stream: decode event: %w", err)
		}

		if err := writeStreamEvent(event, envelope.Event); err != nil {
			return usage, err
		}

		switch event.Type {
		case "message_start":
			messageID = event.Message.ID
			if event.Message.Role != "" {
				role = event.Message.Role
			}
			model = opts.rewriteResponseModel(event.Message.Model)
			stopReason = ""
			blocksByIndex = map[int]map[string]any{}
			inputJSONByIndex = map[int]string{}
			mergeClaudeCLIUsage(&usage, event.Message.Usage, true)
		case "content_block_start":
			if opts.Stream {
				break
			}
			if len(event.ContentBlock) > 0 {
				block := make(map[string]any, len(event.ContentBlock))
				for key, value := range event.ContentBlock {
					block[key] = value
				}
				rewriteClaudeCLIToolUseBlockName(block)
				blocksByIndex[event.Index] = block
			}
		case "content_block_delta":
			if opts.Stream {
				break
			}
			switch event.Delta.Type {
			case "text_delta":
				block := blocksByIndex[event.Index]
				if block == nil {
					block = map[string]any{"type": "text"}
					blocksByIndex[event.Index] = block
				}
				text, _ := block["text"].(string)
				block["text"] = text + event.Delta.Text
			case "input_json_delta":
				inputJSONByIndex[event.Index] += event.Delta.PartialJSON
			}
		case "content_block_stop":
			if opts.Stream {
				break
			}
			inputJSON := inputJSONByIndex[event.Index]
			if inputJSON != "" {
				var input map[string]any
				if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
					return usage, fmt.Errorf("claude cli stream: decode tool input at content block %d: %w", event.Index, err)
				}
				block := blocksByIndex[event.Index]
				if block == nil {
					block = map[string]any{"type": "tool_use"}
					blocksByIndex[event.Index] = block
				}
				block["input"] = input
			}
		case "message_delta":
			if event.Delta.StopReason != "" {
				stopReason = event.Delta.StopReason
			}
			mergeClaudeCLIUsage(&usage, event.Usage, false)
		}
	}
	if err := scanner.Err(); err != nil {
		return usage, fmt.Errorf("claude cli stream: scan: %w", err)
	}

	if opts.Stream {
		if !sawStreamEvent && finalAssistant != nil {
			if err := writeFinalAssistantAsAnthropicSSE(w, finalAssistant, opts); err != nil {
				return usage, err
			}
		}
		if !sawStreamEvent && finalAssistant == nil {
			return usage, fmt.Errorf("claude cli stream: no assistant output")
		}
		return usage, nil
	}

	content := make([]any, 0, len(blocksByIndex))
	indices := make([]int, 0, len(blocksByIndex))
	for index := range blocksByIndex {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	for _, index := range indices {
		content = append(content, blocksByIndex[index])
	}
	if len(content) == 0 && finalAssistant != nil {
		messageID = finalAssistant.Message.ID
		if finalAssistant.Message.Role != "" {
			role = finalAssistant.Message.Role
		}
		model = opts.rewriteResponseModel(finalAssistant.Message.Model)
		stopReason = finalAssistant.Message.StopReason
		content = make([]any, 0, len(finalAssistant.Message.Content))
		for _, block := range finalAssistant.Message.Content {
			copied := make(map[string]any, len(block))
			for key, value := range block {
				copied[key] = value
			}
			rewriteClaudeCLIToolUseBlockName(copied)
			content = append(content, copied)
		}
	}
	if messageID == "" && len(content) == 0 && finalAssistant == nil {
		return usage, fmt.Errorf("claude cli stream: no assistant output")
	}

	message := map[string]any{
		"id":            messageID,
		"type":          "message",
		"role":          role,
		"model":         model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         claudeCLIResponseUsage(usage),
	}
	if err := json.NewEncoder(w).Encode(message); err != nil {
		return usage, fmt.Errorf("claude cli stream: encode message: %w", err)
	}

	return usage, nil
}

func writeFinalAssistantAsAnthropicSSE(w io.Writer, assistant *claudeCLIFinalAssistantEnvelope, opts claudeCLITranslateOptions) error {
	if assistant == nil {
		return nil
	}
	message := map[string]any{
		"id":            assistant.Message.ID,
		"type":          "message",
		"role":          "assistant",
		"model":         opts.rewriteResponseModel(assistant.Message.Model),
		"content":       []any{},
		"stop_reason":   nil,
		"stop_sequence": nil,
		"usage":         claudeCLIResponseUsage(usageFromMap(assistant.Message.Usage)),
	}
	if assistant.Message.Role != "" {
		message["role"] = assistant.Message.Role
	}
	if err := writeAnthropicSSEEventObject(w, "message_start", map[string]any{
		"type":    "message_start",
		"message": message,
	}); err != nil {
		return err
	}
	visibleIndex := 0
	for _, block := range assistant.Message.Content {
		blockType, _ := block["type"].(string)
		contentBlock := make(map[string]any, len(block))
		for key, value := range block {
			contentBlock[key] = value
		}
		rewriteClaudeCLIToolUseBlockName(contentBlock)
		switch blockType {
		case "text":
			text, _ := contentBlock["text"].(string)
			contentBlock["text"] = ""
			if err := writeAnthropicSSEEventObject(w, "content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         visibleIndex,
				"content_block": contentBlock,
			}); err != nil {
				return err
			}
			if text != "" {
				if err := writeAnthropicSSEEventObject(w, "content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": visibleIndex,
					"delta": map[string]any{"type": "text_delta", "text": text},
				}); err != nil {
					return err
				}
			}
		case "tool_use":
			input := contentBlock["input"]
			contentBlock["input"] = map[string]any{}
			if err := writeAnthropicSSEEventObject(w, "content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         visibleIndex,
				"content_block": contentBlock,
			}); err != nil {
				return err
			}
			if input != nil {
				encoded, err := json.Marshal(input)
				if err != nil {
					return fmt.Errorf("claude cli stream: encode tool input delta: %w", err)
				}
				if err := writeAnthropicSSEEventObject(w, "content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": visibleIndex,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": string(encoded)},
				}); err != nil {
					return err
				}
			}
		default:
			if err := writeAnthropicSSEEventObject(w, "content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         visibleIndex,
				"content_block": contentBlock,
			}); err != nil {
				return err
			}
		}
		if err := writeAnthropicSSEEventObject(w, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": visibleIndex,
		}); err != nil {
			return err
		}
		visibleIndex++
	}
	if err := writeAnthropicSSEEventObject(w, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": assistant.Message.StopReason, "stop_sequence": assistant.Message.StopSequence},
		"usage": assistant.Message.Usage,
	}); err != nil {
		return err
	}
	return writeAnthropicSSEEventObject(w, "message_stop", map[string]any{"type": "message_stop"})
}

func (opts claudeCLITranslateOptions) rewriteResponseModel(model string) string {
	if opts.ResponseModel == "" || opts.UpstreamModel == "" || opts.ResponseModel == opts.UpstreamModel {
		return model
	}
	if model == opts.UpstreamModel {
		return opts.ResponseModel
	}
	return model
}

func rewriteClaudeCLIStreamMessageStartModel(raw json.RawMessage, opts claudeCLITranslateOptions) (json.RawMessage, error) {
	if opts.ResponseModel == "" || opts.UpstreamModel == "" || opts.ResponseModel == opts.UpstreamModel {
		return raw, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("claude cli stream: decode message_start for model rewrite: %w", err)
	}
	message, ok := payload["message"].(map[string]any)
	if !ok {
		return raw, nil
	}
	model, _ := message["model"].(string)
	if model != opts.UpstreamModel {
		return raw, nil
	}
	message["model"] = opts.ResponseModel
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("claude cli stream: encode message_start for model rewrite: %w", err)
	}
	return rewritten, nil
}

func rewriteClaudeCLIStreamToolUseName(raw json.RawMessage) (json.RawMessage, error) {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("claude cli stream: decode content_block_start for tool name rewrite: %w", err)
	}
	contentBlock, ok := payload["content_block"].(map[string]any)
	if !ok {
		return raw, nil
	}
	if !rewriteClaudeCLIToolUseBlockName(contentBlock) {
		return raw, nil
	}
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("claude cli stream: encode content_block_start for tool name rewrite: %w", err)
	}
	return rewritten, nil
}

func rewriteClaudeCLIToolUseBlockName(block map[string]any) bool {
	if block == nil {
		return false
	}
	blockType, _ := block["type"].(string)
	if blockType != "tool_use" {
		return false
	}
	name, _ := block["name"].(string)
	clientName := claudeCLIClientToolName(name)
	if clientName == name {
		return false
	}
	block["name"] = clientName
	return true
}

func usageFromMap(raw map[string]any) ClaudeUsage {
	var usage ClaudeUsage
	mergeClaudeCLIUsage(&usage, raw, true)
	return usage
}

func filterClaudeCLIOutputForCompletedToolUses(output []byte, toolUseIDs []string) ([]byte, error) {
	if len(output) == 0 {
		return output, nil
	}
	skipIDs := make(map[string]struct{}, len(toolUseIDs))
	for _, id := range toolUseIDs {
		id = strings.TrimSpace(id)
		if id != "" {
			skipIDs[id] = struct{}{}
		}
	}
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), claudeCLIStreamMaxLineSize)

	var out bytes.Buffer
	var group []string
	inGroup := false
	skipGroup := false

	flushGroup := func() {
		if !skipGroup {
			for _, line := range group {
				_, _ = out.WriteString(line)
				_ = out.WriteByte('\n')
			}
		}
		group = nil
		inGroup = false
		skipGroup = false
	}

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if inGroup {
				group = append(group, line)
			} else {
				_, _ = out.WriteString(line)
				_ = out.WriteByte('\n')
			}
			continue
		}

		var envelope claudeCLIStreamEnvelope
		if err := json.Unmarshal([]byte(trimmed), &envelope); err != nil || envelope.Type != "stream_event" {
			if inGroup {
				group = append(group, line)
			} else {
				_, _ = out.WriteString(line)
				_ = out.WriteByte('\n')
			}
			continue
		}

		var event claudeCLIStreamEvent
		if err := json.Unmarshal(envelope.Event, &event); err != nil {
			return nil, fmt.Errorf("claude cli stream: decode event for filtering: %w", err)
		}

		if event.Type == "message_start" {
			if inGroup {
				flushGroup()
			}
			inGroup = true
		}
		if !inGroup {
			_, _ = out.WriteString(line)
			_ = out.WriteByte('\n')
			continue
		}

		group = append(group, line)
		if shouldSkipClaudeCLIToolUseEvent(event, skipIDs) {
			skipGroup = true
		}
		if event.Type == "message_stop" {
			flushGroup()
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("claude cli stream: scan output for filtering: %w", err)
	}
	if inGroup {
		flushGroup()
	}
	return out.Bytes(), nil
}

func shouldSkipClaudeCLIToolUseEvent(event claudeCLIStreamEvent, skipIDs map[string]struct{}) bool {
	if len(event.ContentBlock) == 0 {
		return false
	}
	blockType, _ := event.ContentBlock["type"].(string)
	if blockType != "tool_use" {
		return false
	}
	id, _ := event.ContentBlock["id"].(string)
	if _, ok := skipIDs[id]; ok {
		return true
	}
	name, _ := event.ContentBlock["name"].(string)
	return !isClaudeCLIMCPToolUseName(name)
}

func isClaudeCLIMCPToolUseName(name string) bool {
	return strings.HasPrefix(name, "mcp__")
}

func writeAnthropicSSEEventObject(w io.Writer, eventType string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("claude cli stream: encode sse event: %w", err)
	}
	return writeAnthropicSSEEvent(w, eventType, encoded)
}

func mergeClaudeCLIUsage(usage *ClaudeUsage, usageObj map[string]any, start bool) {
	if usage == nil || len(usageObj) == 0 {
		return
	}

	mergeInt := func(field *int, key string) {
		value, ok := parseSSEUsageInt(usageObj[key])
		if !ok {
			return
		}
		if start || value > 0 {
			*field = value
		}
	}

	mergeInt(&usage.InputTokens, "input_tokens")
	mergeInt(&usage.OutputTokens, "output_tokens")
	mergeInt(&usage.CacheCreationInputTokens, "cache_creation_input_tokens")

	cacheRead, hasCacheRead := parseSSEUsageInt(usageObj["cache_read_input_tokens"])
	cachedTokens, hasCachedTokens := parseSSEUsageInt(usageObj["cached_tokens"])
	if (!hasCacheRead || cacheRead <= 0) && hasCachedTokens && cachedTokens > 0 {
		cacheRead = cachedTokens
		hasCacheRead = true
	}
	if hasCacheRead && (start || cacheRead > 0) {
		usage.CacheReadInputTokens = cacheRead
	}

	cacheCreation, _ := usageObj["cache_creation"].(map[string]any)
	if value, ok := parseSSEUsageInt(cacheCreation["ephemeral_5m_input_tokens"]); ok && (start || value > 0) {
		usage.CacheCreation5mTokens = value
	}
	if value, ok := parseSSEUsageInt(cacheCreation["ephemeral_1h_input_tokens"]); ok && (start || value > 0) {
		usage.CacheCreation1hTokens = value
	}
}

func claudeCLIResponseUsage(usage ClaudeUsage) map[string]any {
	responseUsage := map[string]any{
		"input_tokens":                usage.InputTokens,
		"output_tokens":               usage.OutputTokens,
		"cache_creation_input_tokens": usage.CacheCreationInputTokens,
		"cache_read_input_tokens":     usage.CacheReadInputTokens,
	}
	if usage.CacheCreation5mTokens > 0 || usage.CacheCreation1hTokens > 0 {
		responseUsage["cache_creation"] = map[string]any{
			"ephemeral_5m_input_tokens": usage.CacheCreation5mTokens,
			"ephemeral_1h_input_tokens": usage.CacheCreation1hTokens,
		}
	}
	return responseUsage
}

func writeAnthropicSSEEvent(w io.Writer, eventType string, payload json.RawMessage) error {
	if _, err := fmt.Fprintf(w, "event: %s\n", eventType); err != nil {
		return fmt.Errorf("claude cli stream: write sse event: %w", err)
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
		return fmt.Errorf("claude cli stream: write sse data: %w", err)
	}
	return nil
}
