package service

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTranslateClaudeCLIStreamToNonStreamingMessage(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_1","role":"assistant","model":"claude-opus-4-7","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`,
		`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}}`,
		`{"type":"stream_event","event":{"type":"message_stop"}}`,
	}, "\n")

	var out bytes.Buffer
	usage, err := translateClaudeCLIStream(strings.NewReader(input), &out, claudeCLITranslateOptions{Stream: false})
	require.NoError(t, err)
	require.Equal(t, 10, usage.InputTokens)
	require.Equal(t, 3, usage.OutputTokens)

	var message struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string      `json:"stop_reason"`
		StopSeq    any         `json:"stop_sequence"`
		Usage      ClaudeUsage `json:"usage"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &message))
	require.Equal(t, "msg_1", message.ID)
	require.Equal(t, "message", message.Type)
	require.Equal(t, "assistant", message.Role)
	require.Equal(t, "claude-opus-4-7", message.Model)
	require.Len(t, message.Content, 1)
	require.Equal(t, "text", message.Content[0].Type)
	require.Equal(t, "hello", message.Content[0].Text)
	require.Equal(t, "end_turn", message.StopReason)
	require.Nil(t, message.StopSeq)
}

func TestTranslateClaudeCLIStreamRewritesMappedModelForNonStreamingResponse(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_1","role":"assistant","model":"claude-sonnet-4-5","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`,
		`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}}`,
		`{"type":"stream_event","event":{"type":"message_stop"}}`,
	}, "\n")

	var out bytes.Buffer
	_, err := translateClaudeCLIStream(strings.NewReader(input), &out, claudeCLITranslateOptions{
		Stream:        false,
		ResponseModel: "claude-opus-4-7",
		UpstreamModel: "claude-sonnet-4-5",
	})
	require.NoError(t, err)

	var message map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &message))
	require.Equal(t, "claude-opus-4-7", message["model"])
}

func TestTranslateClaudeCLIStreamRewritesMappedModelForStreamingResponse(t *testing.T) {
	input := `{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_1","role":"assistant","model":"claude-sonnet-4-5","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}}`

	var out bytes.Buffer
	_, err := translateClaudeCLIStream(strings.NewReader(input), &out, claudeCLITranslateOptions{
		Stream:        true,
		ResponseModel: "claude-opus-4-7",
		UpstreamModel: "claude-sonnet-4-5",
	})
	require.NoError(t, err)
	require.Contains(t, out.String(), `"model":"claude-opus-4-7"`)
	require.NotContains(t, out.String(), `"model":"claude-sonnet-4-5"`)
}

func TestTranslateClaudeCLIStreamIncludesToolUse(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_1","role":"assistant","model":"claude-opus-4-7","content":[],"usage":{"input_tokens":4,"output_tokens":0}}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"hel"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"lo\"}"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`,
		`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":8}}}`,
		`{"type":"stream_event","event":{"type":"message_stop"}}`,
	}, "\n")

	var out bytes.Buffer
	usage, err := translateClaudeCLIStream(strings.NewReader(input), &out, claudeCLITranslateOptions{Stream: false})
	require.NoError(t, err)
	require.Equal(t, 4, usage.InputTokens)
	require.Equal(t, 8, usage.OutputTokens)

	var message map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &message))
	require.Equal(t, "tool_use", message["stop_reason"])

	content, ok := message["content"].([]any)
	require.True(t, ok)
	require.Len(t, content, 1)

	block, ok := content[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "tool_use", block["type"])
	require.Equal(t, "toolu_1", block["id"])
	require.Equal(t, "lookup", block["name"])

	toolInput, ok := block["input"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "hello", toolInput["q"])
}

func TestTranslateClaudeCLIStreamStripsMCPServerPrefixForStreamingToolUse(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_1","role":"assistant","model":"claude-opus-4-7","content":[],"usage":{"input_tokens":4,"output_tokens":0}}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"mcp__claude__Bash","input":{}}}}`,
	}, "\n")

	var out bytes.Buffer
	_, err := translateClaudeCLIStream(strings.NewReader(input), &out, claudeCLITranslateOptions{Stream: true})
	require.NoError(t, err)
	require.Contains(t, out.String(), `"name":"Bash"`)
	require.NotContains(t, out.String(), `"name":"mcp__claude__Bash"`)
}

func TestTranslateClaudeCLIStreamStripsMCPServerPrefixForNonStreamingToolUse(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_1","role":"assistant","model":"claude-opus-4-7","content":[],"usage":{"input_tokens":4,"output_tokens":0}}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"mcp__claude__Bash","input":{}}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"date\"}"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`,
		`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":8}}}`,
		`{"type":"stream_event","event":{"type":"message_stop"}}`,
	}, "\n")

	var out bytes.Buffer
	_, err := translateClaudeCLIStream(strings.NewReader(input), &out, claudeCLITranslateOptions{Stream: false})
	require.NoError(t, err)

	var message map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &message))
	content := requireSliceAny(t, message["content"])
	require.Len(t, content, 1)
	block := requireMapAny(t, content[0])
	require.Equal(t, "Bash", block["name"])
}

func TestTranslateClaudeCLIStreamPassesThroughCLIWebToolUseStreamingEvents(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_tool","role":"assistant","model":"claude-opus-4-7","content":[],"usage":{"input_tokens":4,"output_tokens":0}}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_web","name":"WebSearch","input":{}}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"PCX160 价格\"}"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`,
		`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":3}}}`,
		`{"type":"stream_event","event":{"type":"message_stop"}}`,
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_text","role":"assistant","model":"claude-opus-4-7","content":[],"usage":{"input_tokens":4,"output_tokens":0}}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"PCX160 价格大约是..."}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`,
		`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":8}}}`,
		`{"type":"stream_event","event":{"type":"message_stop"}}`,
	}, "\n")

	var out bytes.Buffer
	_, err := translateClaudeCLIStream(strings.NewReader(input), &out, claudeCLITranslateOptions{Stream: true})
	require.NoError(t, err)
	require.Contains(t, out.String(), "PCX160")
	require.Contains(t, out.String(), "WebSearch")
	require.Contains(t, out.String(), "toolu_web")
	require.Contains(t, out.String(), `"stop_reason":"tool_use"`)
}

func TestTranslateClaudeCLIStreamPassesThroughCLIWebToolUseFromNonStreamingMessage(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_1","role":"assistant","model":"claude-opus-4-7","content":[],"usage":{"input_tokens":4,"output_tokens":0}}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_web","name":"WebFetch","input":{}}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"url\":\"https://example.com\"}"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"visible answer"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":1}}`,
		`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":8}}}`,
		`{"type":"stream_event","event":{"type":"message_stop"}}`,
	}, "\n")

	var out bytes.Buffer
	_, err := translateClaudeCLIStream(strings.NewReader(input), &out, claudeCLITranslateOptions{Stream: false})
	require.NoError(t, err)

	var message map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &message))
	content, ok := message["content"].([]any)
	require.True(t, ok)
	require.Len(t, content, 2)
	block, ok := content[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "tool_use", block["type"])
	require.Equal(t, "WebFetch", block["name"])
	require.Contains(t, out.String(), "toolu_web")
	require.Contains(t, out.String(), "https://example.com")
}

func TestTranslateClaudeCLIStreamPassesThroughStreamingEvents(t *testing.T) {
	input := `{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_1","role":"assistant","model":"claude-opus-4-7","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}}`

	var out bytes.Buffer
	_, err := translateClaudeCLIStream(strings.NewReader(input), &out, claudeCLITranslateOptions{Stream: true})
	require.NoError(t, err)
	require.Contains(t, out.String(), "event: message_start\n")
	require.Contains(t, out.String(), "data: ")
}

func TestTranslateClaudeCLIStreamUsesFinalAssistantMessageWhenPartialEventsMissing(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"system","subtype":"init","apiKeySource":"none","cwd":"/tmp","session_id":"sess_1","tools":[],"mcp_servers":[],"model":"claude-opus-4-7","permissionMode":"bypassPermissions","uuid":"00000000-0000-4000-8000-000000000001","claude_code_version":"2.0.0"}`,
		`{"type":"assistant","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4-7","content":[{"type":"text","text":"hello from final assistant"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":7,"output_tokens":5}},"parent_tool_use_id":null,"session_id":"sess_1","uuid":"00000000-0000-4000-8000-000000000002"}`,
		`{"type":"result","subtype":"success","duration_ms":100,"duration_api_ms":90,"is_error":false,"num_turns":1,"result":"hello from final assistant","stop_reason":"end_turn","total_cost_usd":0,"usage":{"input_tokens":7,"output_tokens":5},"modelUsage":{},"permission_denials":[],"session_id":"sess_1","uuid":"00000000-0000-4000-8000-000000000003"}`,
	}, "\n")

	var out bytes.Buffer
	usage, err := translateClaudeCLIStream(strings.NewReader(input), &out, claudeCLITranslateOptions{Stream: false})
	require.NoError(t, err)
	require.Equal(t, 7, usage.InputTokens)
	require.Equal(t, 5, usage.OutputTokens)

	var message map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &message))
	require.Equal(t, "msg_1", message["id"])
	require.Equal(t, "end_turn", message["stop_reason"])

	content, ok := message["content"].([]any)
	require.True(t, ok)
	require.Len(t, content, 1)
	block, ok := content[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "text", block["type"])
	require.Equal(t, "hello from final assistant", block["text"])
}

func TestTranslateClaudeCLIStreamPassesThroughServerWebToolBlocksFromFinalAssistant(t *testing.T) {
	input := `{"type":"assistant","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4-7","content":[{"type":"server_tool_use","id":"srvu_1","name":"web_search","input":{"query":"sub2api"}},{"type":"web_search_tool_result","tool_use_id":"srvu_1","content":[{"title":"Result","url":"https://example.com"}]},{"type":"text","text":"visible answer"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":7,"output_tokens":5}},"parent_tool_use_id":null,"session_id":"sess_1","uuid":"00000000-0000-4000-8000-000000000002"}`

	var out bytes.Buffer
	_, err := translateClaudeCLIStream(strings.NewReader(input), &out, claudeCLITranslateOptions{Stream: false})
	require.NoError(t, err)

	var message map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &message))
	content, ok := message["content"].([]any)
	require.True(t, ok)
	require.Len(t, content, 3)
	block, ok := content[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "server_tool_use", block["type"])
	require.Contains(t, out.String(), "web_search_tool_result")
	require.Contains(t, out.String(), "https://example.com")
}

func TestTranslateClaudeCLIStreamPassesThroughCLIWebToolUseFromFinalAssistant(t *testing.T) {
	input := `{"type":"assistant","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4-7","content":[{"type":"tool_use","id":"toolu_web","name":"WebFetch","input":{"url":"https://example.com"}},{"type":"text","text":"visible answer"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":7,"output_tokens":5}},"parent_tool_use_id":null,"session_id":"sess_1","uuid":"00000000-0000-4000-8000-000000000002"}`

	var out bytes.Buffer
	_, err := translateClaudeCLIStream(strings.NewReader(input), &out, claudeCLITranslateOptions{Stream: false})
	require.NoError(t, err)

	var message map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &message))
	content, ok := message["content"].([]any)
	require.True(t, ok)
	require.Len(t, content, 2)
	block, ok := content[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "tool_use", block["type"])
	require.Equal(t, "WebFetch", block["name"])
	require.Contains(t, out.String(), "toolu_web")
	require.Contains(t, out.String(), "https://example.com")
}

func TestTranslateClaudeCLIStreamSynthesizesStreamingEventsFromFinalAssistant(t *testing.T) {
	input := `{"type":"assistant","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4-7","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":7,"output_tokens":5}},"parent_tool_use_id":null,"session_id":"sess_1","uuid":"00000000-0000-4000-8000-000000000002"}`

	var out bytes.Buffer
	usage, err := translateClaudeCLIStream(strings.NewReader(input), &out, claudeCLITranslateOptions{Stream: true})
	require.NoError(t, err)
	require.Equal(t, 7, usage.InputTokens)
	require.Equal(t, 5, usage.OutputTokens)
	require.Contains(t, out.String(), "event: message_start\n")
	require.Contains(t, out.String(), "event: content_block_start\n")
	require.Contains(t, out.String(), "event: content_block_delta\n")
	require.Contains(t, out.String(), "event: content_block_stop\n")
	require.Contains(t, out.String(), "event: message_delta\n")
	require.Contains(t, out.String(), "event: message_stop\n")
	require.Contains(t, out.String(), `"text":"hello"`)
}

func TestTranslateClaudeCLIStreamRejectsMissingAssistantOutput(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"system","subtype":"init","apiKeySource":"none","cwd":"/tmp","session_id":"sess_1","tools":[],"mcp_servers":[],"model":"claude-opus-4-7","permissionMode":"bypassPermissions","uuid":"00000000-0000-4000-8000-000000000001","claude_code_version":"2.0.0"}`,
		`{"type":"result","subtype":"success","duration_ms":100,"duration_api_ms":90,"is_error":false,"num_turns":1,"result":"","stop_reason":"end_turn","total_cost_usd":0,"usage":{"input_tokens":0,"output_tokens":0},"modelUsage":{},"permission_denials":[],"session_id":"sess_1","uuid":"00000000-0000-4000-8000-000000000003"}`,
	}, "\n")

	var out bytes.Buffer
	_, err := translateClaudeCLIStream(strings.NewReader(input), &out, claudeCLITranslateOptions{Stream: false})
	require.ErrorContains(t, err, "no assistant output")
	require.Empty(t, out.String())
}

func TestTranslateClaudeCLIStreamStreamingDoesNotValidateToolInput(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_1","role":"assistant","model":"claude-opus-4-7","content":[],"usage":{"input_tokens":4,"output_tokens":0}}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`,
	}, "\n")

	var out bytes.Buffer
	_, err := translateClaudeCLIStream(strings.NewReader(input), &out, claudeCLITranslateOptions{Stream: true})
	require.NoError(t, err)
	require.Contains(t, out.String(), "event: content_block_start\n")
	require.Contains(t, out.String(), "event: content_block_delta\n")
	require.Contains(t, out.String(), "event: content_block_stop\n")
	require.Contains(t, out.String(), "data: ")
}

func TestTranslateClaudeCLIStreamUsageDoesNotLeakInternalFields(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_1","role":"assistant","model":"claude-opus-4-7","content":[],"usage":{"input_tokens":10,"cache_creation_input_tokens":5,"cache_read_input_tokens":0,"cached_tokens":7,"cache_creation":{"ephemeral_5m_input_tokens":2,"ephemeral_1h_input_tokens":3}}}}}`,
		`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}}`,
		`{"type":"stream_event","event":{"type":"message_stop"}}`,
	}, "\n")

	var out bytes.Buffer
	usage, err := translateClaudeCLIStream(strings.NewReader(input), &out, claudeCLITranslateOptions{Stream: false})
	require.NoError(t, err)
	require.Equal(t, 10, usage.InputTokens)
	require.Equal(t, 4, usage.OutputTokens)
	require.Equal(t, 5, usage.CacheCreationInputTokens)
	require.Equal(t, 7, usage.CacheReadInputTokens)
	require.Equal(t, 2, usage.CacheCreation5mTokens)
	require.Equal(t, 3, usage.CacheCreation1hTokens)

	var message map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &message))
	outputUsage, ok := message["usage"].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, outputUsage, "CacheCreation5mTokens")
	require.NotContains(t, outputUsage, "CacheCreation1hTokens")
	cacheCreation, ok := outputUsage["cache_creation"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, float64(2), cacheCreation["ephemeral_5m_input_tokens"])
	require.Equal(t, float64(3), cacheCreation["ephemeral_1h_input_tokens"])
}

func TestFilterClaudeCLIOutputForCompletedToolUsesDropsNonMCPToolUseMessages(t *testing.T) {
	input := claudeCLIStreamJSON(
		`{"type":"message_start","message":{"id":"msg_bad","role":"assistant","model":"claude-opus-4-7","content":[],"usage":{"input_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_bad","name":"Bash","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"date\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
		`{"type":"message_start","message":{"id":"msg_ok","role":"assistant","model":"claude-opus-4-7","content":[],"usage":{"input_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	)

	filtered, err := filterClaudeCLIOutputForCompletedToolUses([]byte(input), nil)
	require.NoError(t, err)
	require.NotContains(t, string(filtered), `"name":"Bash"`)
	require.NotContains(t, string(filtered), `"toolu_bad"`)
	require.Contains(t, string(filtered), `"text":"done"`)
}
