package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPrepareClaudeCLIWorkspaceWritesPromptSessionAndArgs(t *testing.T) {
	fixedNow := time.Date(2026, 4, 27, 1, 2, 3, 4, time.UTC)
	input := &claudeCLIInput{
		Model:        "claude-opus-4-7",
		SystemPrompt: "Project policy",
		History: []claudeCLISDKMessage{
			{
				Type: "user",
				Message: claudeCLIMessageObject{
					Role:    "user",
					Content: []claudeCLIContentBlock{{Type: "text", Text: "old"}},
				},
			},
			{
				Type: "assistant",
				Message: claudeCLIMessageObject{
					Role:    "assistant",
					Content: []claudeCLIContentBlock{{Type: "text", Text: "older answer"}},
				},
			},
		},
		Prompt: claudeCLISDKMessage{
			Type: "user",
			Message: claudeCLIMessageObject{
				Role:    "user",
				Content: []claudeCLIContentBlock{{Type: "text", Text: "new"}},
			},
		},
	}

	ws, err := prepareClaudeCLIWorkspace(t.TempDir(), input, claudeCLIWorkspaceOptions{
		SessionID: "11111111-1111-4111-8111-111111111111",
		Now: func() time.Time {
			return fixedNow
		},
	})
	require.NoError(t, err)
	t.Cleanup(ws.Cleanup)

	require.True(t, strings.HasPrefix(filepath.Base(ws.Dir), "claude-code-workspace-"))
	require.Equal(t, filepath.Join(ws.Dir, "mcp-config.json"), ws.MCPConfigPath)
	realDir, err := filepath.EvalSymlinks(ws.Dir)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(ws.Dir, ".claude", "projects", sanitizeClaudeCLIProjectPath(realDir), "11111111-1111-4111-8111-111111111111.jsonl"), ws.SessionPath)

	systemPrompt, err := os.ReadFile(ws.SystemPromptPath)
	require.NoError(t, err)
	require.Equal(t, "Project policy", string(systemPrompt))
	requireFileMode(t, ws.SystemPromptPath, 0600)

	sessionBytes, err := os.ReadFile(ws.SessionPath)
	require.NoError(t, err)
	sessionLines := splitNonEmptyJSONLLines(string(sessionBytes))
	require.Len(t, sessionLines, 2)
	sessionLine := decodeJSONLMap(t, sessionLines[0])
	require.Equal(t, "user", sessionLine["type"])
	require.Equal(t, "11111111-1111-4111-8111-111111111111", sessionLine["sessionId"])
	requireUUIDString(t, sessionLine["uuid"])
	require.Equal(t, fixedNow.UTC().Format(time.RFC3339Nano), sessionLine["timestamp"])
	require.Equal(t, realDir, sessionLine["cwd"])
	require.Equal(t, "sub2api", sessionLine["version"])
	require.Equal(t, "external", sessionLine["userType"])
	require.Equal(t, "cli", sessionLine["entrypoint"])
	message, ok := sessionLine["message"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "user", message["role"])
	requireFileMode(t, ws.SessionPath, 0600)

	secondSessionLine := decodeJSONLMap(t, sessionLines[1])
	requireUUIDString(t, secondSessionLine["uuid"])
	require.Equal(t, sessionLine["uuid"], secondSessionLine["parentUuid"])

	require.NotEmpty(t, ws.Stdin)
	require.Contains(t, string(ws.Stdin), "new")
	stdinLine := decodeJSONLMap(t, strings.TrimSpace(string(ws.Stdin)))
	require.Equal(t, "", stdinLine["session_id"])
	require.Contains(t, stdinLine, "parent_tool_use_id")
	require.Nil(t, stdinLine["parent_tool_use_id"])

	require.Equal(t, []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--no-session-persistence",
		"--include-partial-messages",
		"--replay-user-messages",
		"--resume", "11111111-1111-4111-8111-111111111111",
		"--model", input.Model,
		"--system-prompt-file", ws.SystemPromptPath,
		"--tools", "",
		"--allow-dangerously-skip-permissions",
		"--dangerously-skip-permissions",
	}, ws.Args)
}

func TestPrepareClaudeCLIWorkspaceWritesMCPConfigAndArgs(t *testing.T) {
	input := minimalClaudeCLIInput()

	ws, err := prepareClaudeCLIWorkspace(t.TempDir(), input, claudeCLIWorkspaceOptions{
		SessionID: "11111111-1111-4111-8111-111111111111",
		MCPConfig: "{}",
	})
	require.NoError(t, err)
	t.Cleanup(ws.Cleanup)

	require.Equal(t, filepath.Join(ws.Dir, "mcp-config.json"), ws.MCPConfigPath)
	mcpConfig, err := os.ReadFile(ws.MCPConfigPath)
	require.NoError(t, err)
	require.Equal(t, "{}", string(mcpConfig))
	requireArgFollowedBy(t, ws.Args, "--session-id", "11111111-1111-4111-8111-111111111111")
	requireArgSequence(t, ws.Args, "--mcp-config", ws.MCPConfigPath, "--strict-mcp-config", "--tools")
}

func TestWriteClaudeCLISessionFileUsesClaudeCodeTranscriptShape(t *testing.T) {
	fixedNow := time.Date(2026, 4, 27, 1, 2, 3, 4, time.UTC)
	input := &claudeCLIInput{
		Model: "qwen3.6-plus",
		History: []claudeCLISDKMessage{
			{
				Type: "user",
				Message: claudeCLIMessageObject{
					Role:    "user",
					Content: []claudeCLIContentBlock{{Type: "text", Text: "list files"}},
				},
			},
			{
				Type: "assistant",
				Message: claudeCLIMessageObject{
					Role:    "assistant",
					Content: []claudeCLIContentBlock{{Type: "tool_use", ID: "toolu_list", Name: "Bash", Input: map[string]any{"command": "ls"}}},
				},
			},
			{
				Type: "user",
				Message: claudeCLIMessageObject{
					Role:    "user",
					Content: []claudeCLIContentBlock{{Type: "tool_result", ToolUseID: "toolu_list", Content: "a.txt"}},
				},
			},
			{
				Type: "assistant",
				Message: claudeCLIMessageObject{
					Role:    "assistant",
					Content: []claudeCLIContentBlock{{Type: "text", Text: "a.txt exists"}},
				},
			},
		},
		Prompt: claudeCLISDKMessage{
			Type: "user",
			Message: claudeCLIMessageObject{
				Role:    "user",
				Content: []claudeCLIContentBlock{{Type: "text", Text: "next"}},
			},
		},
	}

	ws, err := prepareClaudeCLIWorkspace(t.TempDir(), input, claudeCLIWorkspaceOptions{
		SessionID: "11111111-1111-4111-8111-111111111111",
		Now: func() time.Time {
			return fixedNow
		},
	})
	require.NoError(t, err)
	t.Cleanup(ws.Cleanup)

	sessionBytes, err := os.ReadFile(ws.SessionPath)
	require.NoError(t, err)
	sessionLines := splitNonEmptyJSONLLines(string(sessionBytes))
	require.Len(t, sessionLines, 4)

	first := decodeJSONLMap(t, sessionLines[0])
	require.Contains(t, first, "parentUuid")
	require.Nil(t, first["parentUuid"])
	require.Equal(t, false, first["isSidechain"])

	assistantTool := decodeJSONLMap(t, sessionLines[1])
	assistantToolMessage := assistantTool["message"].(map[string]any)
	require.NotEmpty(t, assistantToolMessage["id"])
	require.Equal(t, "message", assistantToolMessage["type"])
	require.Equal(t, "qwen3.6-plus", assistantToolMessage["model"])
	require.Equal(t, "tool_use", assistantToolMessage["stop_reason"])
	require.NotNil(t, assistantToolMessage["usage"])

	toolResult := decodeJSONLMap(t, sessionLines[2])
	require.Equal(t, assistantTool["uuid"], toolResult["parentUuid"])
	require.Equal(t, assistantTool["uuid"], toolResult["sourceToolAssistantUUID"])

	assistantText := decodeJSONLMap(t, sessionLines[3])
	assistantTextMessage := assistantText["message"].(map[string]any)
	require.NotEqual(t, assistantToolMessage["id"], assistantTextMessage["id"])
	require.Equal(t, "end_turn", assistantTextMessage["stop_reason"])
}

func TestPrepareClaudeCLIWorkspaceOmitsEmptySystemPromptFile(t *testing.T) {
	input := minimalClaudeCLIInput()
	input.SystemPrompt = ""

	ws, err := prepareClaudeCLIWorkspace(t.TempDir(), input, claudeCLIWorkspaceOptions{
		SessionID: "11111111-1111-4111-8111-111111111111",
	})
	require.NoError(t, err)
	t.Cleanup(ws.Cleanup)

	require.NotContains(t, ws.Args, "--system-prompt-file")
	_, err = os.Stat(ws.SystemPromptPath)
	require.True(t, os.IsNotExist(err), "empty system prompt file should not be written, stat err: %v", err)
}

func TestPrepareClaudeCLIWorkspacePassesEnabledTools(t *testing.T) {
	input := minimalClaudeCLIInput()

	ws, err := prepareClaudeCLIWorkspace(t.TempDir(), input, claudeCLIWorkspaceOptions{
		SessionID:    "11111111-1111-4111-8111-111111111111",
		EnabledTools: []string{"WebSearch", "WebFetch"},
	})
	require.NoError(t, err)
	t.Cleanup(ws.Cleanup)

	requireArgFollowedBy(t, ws.Args, "--tools", "WebSearch,WebFetch")
}

func TestPrepareClaudeCLIWorkspaceRejectsUnsafeSessionID(t *testing.T) {
	for _, sessionID := range []string{"../escape", "a/b", `a\b`, ".", ".."} {
		t.Run(sessionID, func(t *testing.T) {
			ws, err := prepareClaudeCLIWorkspace(t.TempDir(), minimalClaudeCLIInput(), claudeCLIWorkspaceOptions{
				SessionID: sessionID,
			})
			require.Error(t, err)
			require.Nil(t, ws)
		})
	}
}

func requireArgSequence(t *testing.T, args []string, sequence ...string) {
	t.Helper()
	for i := 0; i <= len(args)-len(sequence); i++ {
		matched := true
		for j, arg := range sequence {
			if args[i+j] != arg {
				matched = false
				break
			}
		}
		if matched {
			return
		}
	}
	t.Fatalf("args %v do not contain sequence %v", args, sequence)
}

func requireFileMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, mode, info.Mode().Perm())
}

func decodeJSONLMap(t *testing.T, line string) map[string]any {
	t.Helper()
	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(line), &decoded))
	return decoded
}

func requireUUIDString(t *testing.T, value any) {
	t.Helper()
	uuid, ok := value.(string)
	require.True(t, ok)
	require.Regexp(t, regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`), uuid)
}

func splitNonEmptyJSONLLines(content string) []string {
	lines := make([]string, 0)
	for _, line := range strings.Split(content, "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func minimalClaudeCLIInput() *claudeCLIInput {
	return &claudeCLIInput{
		Model:        "claude-opus-4-7",
		SystemPrompt: "Project policy",
		Prompt: claudeCLISDKMessage{
			Type: "user",
			Message: claudeCLIMessageObject{
				Role:    "user",
				Content: []claudeCLIContentBlock{{Type: "text", Text: "new"}},
			},
		},
	}
}
