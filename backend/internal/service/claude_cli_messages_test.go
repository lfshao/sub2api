package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildClaudeCLIInputPreservesClientOnlySystem(t *testing.T) {
	body := mustMarshalClaudeCLITestBody(t, map[string]any{
		"model": "claude-opus-4-7",
		"system": []any{
			map[string]any{"type": "text", "text": "You are Claude Code, Anthropic's official CLI for Claude."},
			map[string]any{"type": "text", "text": "Project policy: answer in Chinese."},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": "first question"},
			map[string]any{"role": "assistant", "content": "prior answer"},
			map[string]any{"role": "user", "content": "current question"},
		},
		"tools": []any{
			map[string]any{
				"name":        "lookup",
				"description": "lookup data",
				"input_schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"q": map[string]any{"type": "string"},
					},
				},
			},
		},
	})

	input, err := buildClaudeCLIInput(&ParsedRequest{
		Body:   body,
		Model:  "claude-opus-4-7",
		System: []any{map[string]any{"type": "text", "text": "You are Claude Code, Anthropic's official CLI for Claude."}, map[string]any{"type": "text", "text": "Project policy: answer in Chinese."}},
		Messages: []any{
			map[string]any{"role": "user", "content": "first question"},
			map[string]any{"role": "assistant", "content": "prior answer"},
			map[string]any{"role": "user", "content": "current question"},
		},
	})

	require.NoError(t, err)
	require.Equal(t, "Project policy: answer in Chinese.", input.SystemPrompt)
	require.Len(t, input.History, 2)
	require.Equal(t, "user", input.Prompt.Type)
	require.Len(t, input.Tools, 1)
	require.Equal(t, "lookup", input.Tools[0].Name)
	require.Equal(t, "lookup data", input.Tools[0].Description)
	require.Equal(t, "object", input.Tools[0].InputSchema["type"])
}

func TestBuildClaudeCLIInputFiltersClaudeGeneratedSystemVariants(t *testing.T) {
	input, err := buildClaudeCLIInput(&ParsedRequest{
		Model: "claude-opus-4-7",
		System: []any{
			map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.119; cc_entrypoint=sdk-cli; cch=00000;"},
			map[string]any{"type": "text", "text": "You are a Claude agent, built on Anthropic's Claude Agent SDK."},
			map[string]any{"type": "text", "text": "You are Claude Code, Anthropic's official CLI for Claude."},
			map[string]any{"type": "text", "text": "You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK."},
			map[string]any{"type": "text", "text": "IMPORTANT: Assist with authorized security testing, defensive security, CTF challenges, and educational contexts."},
			map[string]any{"type": "text", "text": "<system-reminder>Today's date is 2026/04/27.</system-reminder>"},
			map[string]any{"type": "text", "text": "Project policy: keep this."},
		},
		Messages: []any{
			map[string]any{"role": "user", "content": "current"},
		},
	})

	require.NoError(t, err)
	require.Equal(t, "IMPORTANT: Assist with authorized security testing, defensive security, CTF challenges, and educational contexts.\n\n<system-reminder>Today's date is 2026/04/27.</system-reminder>\n\nProject policy: keep this.", input.SystemPrompt)
}

func TestBuildClaudeCLIInputDoesNotInjectMCPToolPromptWithoutTools(t *testing.T) {
	input, err := buildClaudeCLIInput(&ParsedRequest{
		Model:  "claude-opus-4-7",
		System: []any{map[string]any{"type": "text", "text": "Project policy: keep this."}},
		Messages: []any{
			map[string]any{"role": "user", "content": "current"},
		},
	})

	require.NoError(t, err)
	require.Equal(t, "Project policy: keep this.", input.SystemPrompt)
}

func TestBuildClaudeCLIInputPreservesMessageSystemReminders(t *testing.T) {
	currentDateReminder := `<system-reminder>
As you answer the user's questions, you can use the following context:
# currentDate
Today's date is 2026/04/27.

      IMPORTANT: this context may or may not be relevant to your tasks. You should not respond to this context unless it is highly relevant to your task.
</system-reminder>
`
	input, err := buildClaudeCLIInput(&ParsedRequest{
		Model: "claude-opus-4-7",
		Messages: []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": currentDateReminder},
					map[string]any{"type": "text", "text": "<system-reminder>client generated reminder</system-reminder>"},
					map[string]any{"type": "text", "text": "history user keep"},
				},
			},
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "text", "text": "history assistant keep"},
				},
			},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": currentDateReminder},
					map[string]any{"type": "text", "text": "<system-reminder>current generated reminder</system-reminder>"},
					map[string]any{"type": "text", "text": "current user keep"},
				},
			},
		},
	})

	require.NoError(t, err)
	require.Len(t, input.History, 2)
	require.Len(t, input.History[0].Message.Content, 3)
	require.Equal(t, currentDateReminder, input.History[0].Message.Content[0].Text)
	require.Equal(t, "<system-reminder>client generated reminder</system-reminder>", input.History[0].Message.Content[1].Text)
	require.Equal(t, "history user keep", input.History[0].Message.Content[2].Text)
	require.Equal(t, "history assistant keep", input.History[1].Message.Content[0].Text)
	require.Len(t, input.Prompt.Message.Content, 3)
	require.Equal(t, currentDateReminder, input.Prompt.Message.Content[0].Text)
	require.Equal(t, "<system-reminder>current generated reminder</system-reminder>", input.Prompt.Message.Content[1].Text)
	require.Equal(t, "current user keep", input.Prompt.Message.Content[2].Text)
}

func TestBuildClaudeCLIInputDoesNotDropUserTextContainingClaudeCode(t *testing.T) {
	input, err := buildClaudeCLIInput(&ParsedRequest{
		Model: "claude-opus-4-7",
		Messages: []any{
			map[string]any{
				"role":    "user",
				"content": "My prompt mentions Claude Code as normal user text.",
			},
		},
	})

	require.NoError(t, err)
	require.Equal(t, "user", input.Prompt.Type)
	require.Len(t, input.Prompt.Message.Content, 1)
	require.Equal(t, "text", input.Prompt.Message.Content[0].Type)
	require.Equal(t, "My prompt mentions Claude Code as normal user text.", input.Prompt.Message.Content[0].Text)
}

func TestBuildClaudeCLIInputDropsMessagesWithNoSupportedContent(t *testing.T) {
	input, err := buildClaudeCLIInput(&ParsedRequest{
		Model: "claude-opus-4-7",
		Messages: []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "thinking", "thinking": "hidden"},
					map[string]any{"type": "text", "text": "   "},
				},
			},
			map[string]any{"role": "user", "content": "current"},
		},
	})

	require.NoError(t, err)
	require.Len(t, input.History, 0)
	require.Equal(t, "current", input.Prompt.Message.Content[0].Text)
}

func TestBuildClaudeCLIInputPreservesToolUseAndToolResult(t *testing.T) {
	input, err := buildClaudeCLIInput(&ParsedRequest{
		Model: "claude-opus-4-7",
		Messages: []any{
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{
						"type":      "thinking",
						"thinking":  "I need lookup.",
						"signature": "sig_1",
					},
					map[string]any{
						"type":  "tool_use",
						"id":    "toolu_1",
						"name":  "lookup",
						"input": map[string]any{"q": "hello", "n": float64(2)},
					},
				},
			},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type":        "tool_result",
						"tool_use_id": "toolu_1",
						"content":     "failed",
						"is_error":    true,
					},
				},
			},
		},
	})

	require.NoError(t, err)
	require.Len(t, input.History, 2)

	thinking := input.History[0].Message.Content[0]
	require.Equal(t, "thinking", thinking.Type)
	require.Equal(t, "I need lookup.", thinking.Thinking)
	require.Equal(t, "sig_1", thinking.Signature)
	toolUse := input.History[0].Message.Content[1]
	require.Equal(t, "tool_use", toolUse.Type)
	require.Equal(t, "toolu_1", toolUse.ID)
	require.Equal(t, "lookup", toolUse.Name)
	require.Equal(t, "hello", toolUse.Input["q"])
	require.Equal(t, float64(2), toolUse.Input["n"])

	toolResult := input.History[1].Message.Content[0]
	require.Equal(t, "tool_result", toolResult.Type)
	require.Equal(t, "toolu_1", toolResult.ToolUseID)
	require.Equal(t, "failed", toolResult.Content)
	require.True(t, toolResult.IsError)
	require.Equal(t, "text", input.Prompt.Message.Content[0].Type)
	require.Equal(t, "Continue from where you left off.", input.Prompt.Message.Content[0].Text)

	toolUseJSON := mustMarshalClaudeCLITestBody(t, toolUse)
	require.Contains(t, string(toolUseJSON), `"input"`)
	toolResultJSON := mustMarshalClaudeCLITestBody(t, toolResult)
	require.Contains(t, string(toolResultJSON), `"is_error":true`)
}

func TestBuildClaudeCLIInputPreservesUserImageBlocksWithoutClientCacheControl(t *testing.T) {
	imageSource := map[string]any{
		"type":       "base64",
		"media_type": "image/png",
		"data":       "iVBORw0KGgo=",
	}
	cacheControl := map[string]any{"type": "ephemeral", "ttl": "1h"}
	input, err := buildClaudeCLIInput(&ParsedRequest{
		Model: "claude-opus-4-7",
		Messages: []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "这张图是什么？"},
					map[string]any{"type": "image", "source": imageSource, "cache_control": cacheControl},
				},
			},
		},
	})

	require.NoError(t, err)
	require.Len(t, input.Prompt.Message.Content, 2)
	require.Equal(t, "text", input.Prompt.Message.Content[0].Type)
	image := input.Prompt.Message.Content[1]
	require.Equal(t, "image", image.Type)
	require.Equal(t, imageSource, image.Source)
	require.Nil(t, image.CacheControl)

	imageJSON := mustMarshalClaudeCLITestBody(t, image)
	require.JSONEq(t, `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}`, string(imageJSON))
	require.NotContains(t, string(imageJSON), "cache_control")
}

func TestBuildClaudeCLIInputStripsClientMessageCacheControl(t *testing.T) {
	input, err := buildClaudeCLIInput(&ParsedRequest{
		Model: "claude-opus-4-7",
		Messages: []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "old", "cache_control": map[string]any{"type": "ephemeral", "ttl": "5m"}},
				},
			},
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "date"}, "cache_control": map[string]any{"type": "ephemeral"}},
				},
			},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok", "cache_control": map[string]any{"type": "ephemeral"}},
					map[string]any{"type": "text", "text": "next", "cache_control": map[string]any{"type": "ephemeral", "ttl": "1h"}},
				},
			},
		},
	})

	require.NoError(t, err)
	for _, message := range append(input.History, input.Prompt) {
		for _, block := range message.Message.Content {
			require.Nil(t, block.CacheControl)
			encoded := string(mustMarshalClaudeCLITestBody(t, block))
			require.NotContains(t, encoded, "cache_control")
		}
	}
}

func TestBuildClaudeCLIInputNormalizesMissingToolUseInput(t *testing.T) {
	input, err := buildClaudeCLIInput(&ParsedRequest{
		Model: "claude-opus-4-7",
		Messages: []any{
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{
						"type": "tool_use",
						"id":   "toolu_1",
						"name": "lookup",
					},
				},
			},
		},
	})

	require.NoError(t, err)
	toolUse := input.Prompt.Message.Content[0]
	require.NotNil(t, toolUse.Input)
	require.Empty(t, toolUse.Input)

	toolUseJSON := mustMarshalClaudeCLITestBody(t, toolUse)
	require.Contains(t, string(toolUseJSON), `"input":{}`)
}

func TestBuildClaudeCLIInputMovesLongBuiltinToolDescriptionsToFirstMessage(t *testing.T) {
	longDescription := "Run shell commands. " + strings.Repeat("Keep this full detail. ", 110)
	mcpDescription := strings.Repeat("MCP tool detail. ", 150)
	body := mustMarshalClaudeCLITestBody(t, map[string]any{
		"model": "claude-opus-4-7",
		"messages": []any{
			map[string]any{"role": "user", "content": "current"},
		},
		"tools": []any{
			map[string]any{
				"name":         "Bash",
				"description":  longDescription,
				"input_schema": map[string]any{"type": "object"},
			},
			map[string]any{
				"name":         "mcp__jetbrains__read_file",
				"description":  mcpDescription,
				"input_schema": map[string]any{"type": "object"},
			},
		},
	})

	input, err := buildClaudeCLIInput(&ParsedRequest{
		Body:  body,
		Model: "claude-opus-4-7",
		Messages: []any{
			map[string]any{"role": "user", "content": "current"},
		},
	})

	require.NoError(t, err)
	require.Len(t, input.Tools, 2)
	require.Equal(t, "Run shell commands.", input.Tools[0].Description)
	require.Equal(t, mcpDescription, input.Tools[1].Description)
	require.Len(t, input.Prompt.Message.Content, 2)
	reminder := input.Prompt.Message.Content[0].Text
	require.Contains(t, reminder, "<system-reminder>")
	require.Contains(t, reminder, "mcp__claude__Bash")
	require.Contains(t, reminder, strings.TrimSpace(longDescription))
	require.NotContains(t, reminder, "mcp__claude__mcp__jetbrains__read_file")
	require.Equal(t, "current", input.Prompt.Message.Content[1].Text)
}

func TestBuildClaudeCLIInputRejectsEmptyMessages(t *testing.T) {
	input, err := buildClaudeCLIInput(&ParsedRequest{
		Model:    "claude-opus-4-7",
		Messages: []any{},
	})

	require.Error(t, err)
	require.Nil(t, input)
}

func mustMarshalClaudeCLITestBody(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	require.NoError(t, err)
	return body
}
