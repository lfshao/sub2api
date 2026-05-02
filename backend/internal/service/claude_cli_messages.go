package service

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf16"
)

type claudeCLIInput struct {
	Model        string
	Stream       bool
	Effort       string
	SystemPrompt string
	History      []claudeCLISDKMessage
	Prompt       claudeCLISDKMessage
	Tools        []claudeCLITool
}

type claudeCLISDKMessage struct {
	Type            string                 `json:"type"`
	SessionID       string                 `json:"session_id"`
	Message         claudeCLIMessageObject `json:"message"`
	ParentToolUseID *string                `json:"parent_tool_use_id"`
}

type claudeCLIMessageObject struct {
	ID           string                  `json:"id,omitempty"`
	Type         string                  `json:"type,omitempty"`
	Role         string                  `json:"role"`
	Model        string                  `json:"model,omitempty"`
	Content      []claudeCLIContentBlock `json:"content"`
	StopReason   string                  `json:"stop_reason,omitempty"`
	StopSequence any                     `json:"stop_sequence,omitempty"`
	Usage        map[string]any          `json:"usage,omitempty"`
}

type claudeCLIContentBlock struct {
	Type         string         `json:"type"`
	Text         string         `json:"text,omitempty"`
	Thinking     string         `json:"thinking,omitempty"`
	Signature    string         `json:"signature,omitempty"`
	Source       map[string]any `json:"source,omitempty"`
	CacheControl map[string]any `json:"cache_control,omitempty"`
	ID           string         `json:"id,omitempty"`
	Name         string         `json:"name,omitempty"`
	Input        map[string]any `json:"input,omitempty"`
	ToolUseID    string         `json:"tool_use_id,omitempty"`
	Content      any            `json:"content,omitempty"`
	IsError      bool           `json:"is_error,omitempty"`
}

func (b claudeCLIContentBlock) MarshalJSON() ([]byte, error) {
	out := map[string]any{"type": b.Type}
	if b.Text != "" {
		out["text"] = b.Text
	}
	if b.Type == "thinking" {
		out["thinking"] = b.Thinking
		out["signature"] = b.Signature
	}
	if b.Type == "image" && b.Source != nil {
		out["source"] = b.Source
	}
	if b.CacheControl != nil {
		out["cache_control"] = b.CacheControl
	}
	if b.ID != "" {
		out["id"] = b.ID
	}
	if b.Name != "" {
		out["name"] = b.Name
	}
	if b.Type == "tool_use" {
		input := b.Input
		if input == nil {
			input = map[string]any{}
		}
		out["input"] = input
	}
	if b.ToolUseID != "" {
		out["tool_use_id"] = b.ToolUseID
	}
	if b.Content != nil {
		out["content"] = b.Content
	}
	if b.IsError {
		out["is_error"] = true
	}
	return json.Marshal(out)
}

type claudeCLITool struct {
	Type        string         `json:"type,omitempty"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

const claudeCLIMaxMCPDescriptionLength = 2048

type claudeCLILongToolDescription struct {
	Name        string
	Description string
}

func buildClaudeCLIInput(parsed *ParsedRequest) (*claudeCLIInput, error) {
	if parsed == nil {
		return nil, errors.New("claude cli input: parsed request is nil")
	}
	if len(parsed.Messages) == 0 {
		return nil, errors.New("claude cli input: messages are empty")
	}

	messages := make([]claudeCLISDKMessage, 0, len(parsed.Messages))
	for _, raw := range parsed.Messages {
		message, ok, err := convertAnthropicMessage(raw)
		if err != nil {
			return nil, err
		}
		if ok {
			messages = append(messages, message)
		}
	}
	if len(messages) == 0 {
		return nil, errors.New("claude cli input: no supported messages")
	}

	tools := extractAnthropicTools(parsed.Body)
	tools, longToolDescriptions := moveLongBuiltinToolDescriptionsToMessage(tools)
	if len(longToolDescriptions) > 0 {
		prependClaudeCLILongToolDescriptionReminder(&messages[0], longToolDescriptions)
	}
	systemPrompt := buildClaudeCLISystemPrompt(parsed.System)

	history := messages[:len(messages)-1]
	prompt := messages[len(messages)-1]
	if isClaudeCLIPureToolResultMessage(prompt) {
		history = append(history, prompt)
		prompt = claudeCLIContinuePrompt()
	}

	effort := ""
	if normalized := NormalizeClaudeOutputEffort(parsed.OutputEffort); normalized != nil {
		effort = *normalized
	}

	return &claudeCLIInput{
		Model:        parsed.Model,
		Stream:       parsed.Stream,
		Effort:       effort,
		SystemPrompt: systemPrompt,
		History:      history,
		Prompt:       prompt,
		Tools:        tools,
	}, nil
}

func moveLongBuiltinToolDescriptionsToMessage(tools []claudeCLITool) ([]claudeCLITool, []claudeCLILongToolDescription) {
	if len(tools) == 0 {
		return tools, nil
	}
	out := make([]claudeCLITool, len(tools))
	copy(out, tools)
	longDescriptions := make([]claudeCLILongToolDescription, 0)
	for i := range out {
		tool := &out[i]
		description := strings.TrimSpace(tool.Description)
		if description == "" || isClaudeCLIMCPNamedTool(tool.Name) || claudeCLIUTF16Length(description) <= claudeCLIMaxMCPDescriptionLength {
			continue
		}
		longDescriptions = append(longDescriptions, claudeCLILongToolDescription{
			Name:        "mcp__" + claudeCLIMCPServerName + "__" + tool.Name,
			Description: tool.Description,
		})
		tool.Description = firstClaudeCLISentence(description)
	}
	return out, longDescriptions
}

func prependClaudeCLILongToolDescriptionReminder(message *claudeCLISDKMessage, descriptions []claudeCLILongToolDescription) {
	if message == nil || len(descriptions) == 0 {
		return
	}
	var b strings.Builder
	_, _ = b.WriteString("<system-reminder>\n")
	_, _ = b.WriteString("Some client tool descriptions are too long for Claude Code's MCP tool description limit, so their full descriptions are provided here. Use these full descriptions for the corresponding MCP tools.\n\n")
	for i, item := range descriptions {
		if i > 0 {
			_, _ = b.WriteString("\n\n")
		}
		_, _ = b.WriteString("Tool: ")
		_, _ = b.WriteString(item.Name)
		_, _ = b.WriteString("\nDescription:\n")
		_, _ = b.WriteString(strings.TrimSpace(item.Description))
	}
	_, _ = b.WriteString("\n</system-reminder>\n\n")
	reminder := claudeCLIContentBlock{Type: "text", Text: b.String()}
	message.Message.Content = append([]claudeCLIContentBlock{reminder}, message.Message.Content...)
}

func isClaudeCLIMCPNamedTool(name string) bool {
	return strings.HasPrefix(strings.TrimSpace(name), "mcp")
}

func claudeCLIUTF16Length(value string) int {
	return len(utf16.Encode([]rune(value)))
}

func firstClaudeCLISentence(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	for index, r := range value {
		switch r {
		case '.', '!', '?', '。', '！', '？':
			return strings.TrimSpace(value[:index+len(string(r))])
		case '\n', '\r':
			line := strings.TrimSpace(value[:index])
			if line != "" {
				return line
			}
		}
	}
	return value
}

func buildClaudeCLISystemPrompt(rawSystem any) string {
	parts := filterAnthropicSystemPrompt(rawSystem)
	return strings.Join(parts, "\n\n")
}

func isClaudeCLIPureToolResultMessage(message claudeCLISDKMessage) bool {
	if message.Type != "user" || message.Message.Role != "user" || len(message.Message.Content) == 0 {
		return false
	}
	for _, block := range message.Message.Content {
		if block.Type != "tool_result" || block.ToolUseID == "" {
			return false
		}
	}
	return true
}

func claudeCLIContinuePrompt() claudeCLISDKMessage {
	return claudeCLISDKMessage{
		Type: "user",
		Message: claudeCLIMessageObject{
			Role:    "user",
			Content: []claudeCLIContentBlock{{Type: "text", Text: "Continue from where you left off."}},
		},
	}
}

func filterAnthropicSystemPrompt(raw any) []string {
	texts := normalizeTextBlocks(raw)
	if len(texts) == 0 {
		return nil
	}

	filtered := make([]string, 0, len(texts))
	for _, text := range texts {
		trimmed := strings.TrimSpace(text)
		if trimmed == "" || isClaudeCodeGeneratedSystemText(trimmed) {
			continue
		}
		filtered = append(filtered, trimmed)
	}
	return filtered
}

func isClaudeCodeGeneratedSystemText(text string) bool {
	normalized := strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(text)), " "))
	for _, prefix := range []string{
		"x-anthropic-billing-header:",
		"you are claude code, anthropic's official cli for claude",
		"you are a claude agent, built on anthropic's claude agent sdk",
	} {
		if strings.HasPrefix(normalized, prefix) {
			return true
		}
	}
	return false
}

func convertAnthropicMessage(raw any) (claudeCLISDKMessage, bool, error) {
	messageMap, ok := raw.(map[string]any)
	if !ok {
		return claudeCLISDKMessage{}, false, nil
	}

	role, _ := messageMap["role"].(string)
	if role != "user" && role != "assistant" {
		return claudeCLISDKMessage{}, false, nil
	}

	content := convertAnthropicContent(messageMap["content"], role)
	if len(content) == 0 {
		return claudeCLISDKMessage{}, false, nil
	}

	return claudeCLISDKMessage{
		Type: role,
		Message: claudeCLIMessageObject{
			Role:    role,
			Content: content,
		},
	}, true, nil
}

func convertAnthropicContent(raw any, role string) []claudeCLIContentBlock {
	switch value := raw.(type) {
	case string:
		if strings.TrimSpace(value) == "" {
			return nil
		}
		return []claudeCLIContentBlock{{Type: "text", Text: value}}
	case []any:
		blocks := make([]claudeCLIContentBlock, 0, len(value))
		for _, rawBlock := range value {
			block, ok := convertAnthropicContentBlock(rawBlock, role)
			if ok {
				blocks = append(blocks, block)
			}
		}
		return blocks
	default:
		return nil
	}
}

func convertAnthropicContentBlock(raw any, role string) (claudeCLIContentBlock, bool) {
	blockMap, ok := raw.(map[string]any)
	if !ok {
		return claudeCLIContentBlock{}, false
	}

	blockType, _ := blockMap["type"].(string)
	switch blockType {
	case "text":
		text, _ := blockMap["text"].(string)
		if strings.TrimSpace(text) == "" {
			return claudeCLIContentBlock{}, false
		}
		return claudeCLIContentBlock{Type: "text", Text: text}, true
	case "image":
		if role != "user" {
			return claudeCLIContentBlock{}, false
		}
		source, ok := blockMap["source"].(map[string]any)
		if !ok || len(source) == 0 {
			return claudeCLIContentBlock{}, false
		}
		return claudeCLIContentBlock{Type: "image", Source: source}, true
	case "tool_use":
		if role != "assistant" {
			return claudeCLIContentBlock{}, false
		}
		id, _ := blockMap["id"].(string)
		name, _ := blockMap["name"].(string)
		if id == "" || name == "" {
			return claudeCLIContentBlock{}, false
		}
		input, ok := blockMap["input"].(map[string]any)
		if !ok {
			input = map[string]any{}
		}
		return claudeCLIContentBlock{Type: "tool_use", ID: id, Name: name, Input: input}, true
	case "thinking":
		if role != "assistant" {
			return claudeCLIContentBlock{}, false
		}
		thinking, _ := blockMap["thinking"].(string)
		signature, _ := blockMap["signature"].(string)
		return claudeCLIContentBlock{Type: "thinking", Thinking: thinking, Signature: signature}, true
	case "tool_result":
		if role != "user" {
			return claudeCLIContentBlock{}, false
		}
		toolUseID, _ := blockMap["tool_use_id"].(string)
		if toolUseID == "" {
			return claudeCLIContentBlock{}, false
		}
		isError, _ := blockMap["is_error"].(bool)
		return claudeCLIContentBlock{Type: "tool_result", ToolUseID: toolUseID, Content: blockMap["content"], IsError: isError}, true
	default:
		return claudeCLIContentBlock{}, false
	}
}

func normalizeTextBlocks(raw any) []string {
	switch value := raw.(type) {
	case nil:
		return nil
	case string:
		if value == "" {
			return nil
		}
		return []string{value}
	case []any:
		texts := make([]string, 0, len(value))
		for _, item := range value {
			texts = append(texts, normalizeTextBlocks(item)...)
		}
		return texts
	case map[string]any:
		if text, ok := value["text"].(string); ok && text != "" {
			return []string{text}
		}
		return nil
	default:
		return nil
	}
}

func extractAnthropicTools(body []byte) []claudeCLITool {
	if len(body) == 0 {
		return nil
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}

	rawTools, ok := payload["tools"].([]any)
	if !ok || len(rawTools) == 0 {
		return nil
	}

	tools := make([]claudeCLITool, 0, len(rawTools))
	for _, rawTool := range rawTools {
		toolMap, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		name, _ := toolMap["name"].(string)
		if name == "" {
			continue
		}
		toolType, _ := toolMap["type"].(string)
		description, _ := toolMap["description"].(string)
		inputSchema, _ := toolMap["input_schema"].(map[string]any)
		tools = append(tools, claudeCLITool{
			Type:        toolType,
			Name:        name,
			Description: description,
			InputSchema: inputSchema,
		})
	}
	return tools
}
