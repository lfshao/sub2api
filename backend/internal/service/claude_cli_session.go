package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type claudeCLIWorkspace struct {
	Dir              string
	SystemPromptPath string
	SessionPath      string
	MCPConfigPath    string
	Stdin            []byte
	Args             []string
	Cleanup          func()
}

type claudeCLIWorkspaceOptions struct {
	SessionID    string
	Now          func() time.Time
	MCPConfig    string
	EnabledTools []string
	UserID       string
}

func prepareClaudeCLIWorkspace(baseDir string, input *claudeCLIInput, opts claudeCLIWorkspaceOptions) (*claudeCLIWorkspace, error) {
	if input == nil {
		return nil, errors.New("claude cli workspace: input is nil")
	}
	if opts.SessionID == "" {
		return nil, errors.New("claude cli workspace: session id is empty")
	}
	if err := validateClaudeCLISessionIDForPath(opts.SessionID); err != nil {
		return nil, err
	}
	if baseDir == "" {
		baseDir = os.TempDir()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	dir, err := os.MkdirTemp(baseDir, "claude-code-workspace-")
	if err != nil {
		return nil, err
	}

	ws := &claudeCLIWorkspace{
		Dir:              dir,
		SystemPromptPath: filepath.Join(dir, "system-prompt.txt"),
		MCPConfigPath:    filepath.Join(dir, "mcp-config.json"),
		Cleanup: func() {
			_ = os.RemoveAll(dir)
		},
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		realDir = dir
	}
	if opts.UserID != "" {
		if err := writeClaudeCLIGlobalConfig(dir, opts.UserID); err != nil {
			ws.Cleanup()
			return nil, err
		}
	}
	projectDir := filepath.Join(dir, ".claude", "projects", sanitizeClaudeCLIProjectPath(realDir))
	ws.SessionPath = filepath.Join(projectDir, opts.SessionID+".jsonl")

	if input.SystemPrompt != "" {
		if err := os.WriteFile(ws.SystemPromptPath, []byte(input.SystemPrompt), 0600); err != nil {
			ws.Cleanup()
			return nil, err
		}
	}
	if len(input.History) > 0 {
		if err := os.MkdirAll(projectDir, 0700); err != nil {
			ws.Cleanup()
			return nil, err
		}
		if err := writeClaudeCLISessionFile(ws.SessionPath, opts.SessionID, input.Model, input.History, realDir, opts.Now); err != nil {
			ws.Cleanup()
			return nil, err
		}
	}
	stdin, err := buildClaudeCLIStdin(input.Prompt)
	if err != nil {
		ws.Cleanup()
		return nil, err
	}
	ws.Stdin = stdin
	if opts.MCPConfig != "" {
		if err := os.WriteFile(ws.MCPConfigPath, []byte(opts.MCPConfig), 0600); err != nil {
			ws.Cleanup()
			return nil, err
		}
	}

	ws.Args = []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--no-session-persistence",
		"--include-partial-messages",
		"--replay-user-messages",
	}
	if len(input.History) > 0 {
		ws.Args = append(ws.Args, "--resume", opts.SessionID)
	} else {
		ws.Args = append(ws.Args, "--session-id", opts.SessionID)
	}
	ws.Args = append(ws.Args,
		"--model", input.Model,
	)
	if input.Effort != "" {
		ws.Args = append(ws.Args, "--effort", input.Effort)
	}
	if input.SystemPrompt != "" {
		ws.Args = append(ws.Args, "--system-prompt-file", ws.SystemPromptPath)
	}
	if opts.MCPConfig != "" {
		ws.Args = append(ws.Args, "--mcp-config", ws.MCPConfigPath, "--strict-mcp-config")
	}
	toolsArg := ""
	if len(opts.EnabledTools) > 0 {
		toolsArg = strings.Join(opts.EnabledTools, ",")
	}
	ws.Args = append(ws.Args,
		"--tools", toolsArg,
	)
	ws.Args = append(ws.Args,
		"--allow-dangerously-skip-permissions",
		"--dangerously-skip-permissions",
	)

	return ws, nil
}

func writeClaudeCLIGlobalConfig(workspaceDir, userID string) error {
	if !isStandardClaudeCLIUserID(userID) {
		return fmt.Errorf("claude cli workspace: invalid userID")
	}
	configDir := filepath.Join(workspaceDir, ".claude")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return err
	}
	data, err := json.Marshal(map[string]string{"userID": userID})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(configDir, ".claude.json"), data, 0600)
}

func writeClaudeCLISessionFile(path, sessionID string, model string, history []claudeCLISDKMessage, cwd string, now func() time.Time) (err error) {
	if now == nil {
		now = time.Now
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}()

	var parentUUID string
	assistantUUIDByToolUseID := make(map[string]string)
	for i, message := range history {
		uuid := deriveClaudeCLIMessageUUID(sessionID, i+1)
		lineParentUUID := parentUUID
		sourceToolAssistantUUID := ""
		if isClaudeCLIPureToolResultMessage(message) {
			for _, block := range message.Message.Content {
				if block.ToolUseID == "" {
					continue
				}
				if assistantUUID := assistantUUIDByToolUseID[block.ToolUseID]; assistantUUID != "" {
					sourceToolAssistantUUID = assistantUUID
					lineParentUUID = assistantUUID
					break
				}
			}
		}
		sessionMessage := completeClaudeCLISessionMessage(message, uuid, model)
		line := claudeCLISessionLine{
			ParentUUID:              optionalClaudeCLIParentUUID(lineParentUUID),
			IsSidechain:             false,
			Type:                    message.Type,
			SessionID:               sessionID,
			UUID:                    uuid,
			Timestamp:               now().UTC().Format(time.RFC3339Nano),
			Message:                 sessionMessage,
			CWD:                     cwd,
			Version:                 "sub2api",
			UserType:                "external",
			Entrypoint:              "cli",
			SourceToolAssistantUUID: optionalClaudeCLIParentUUID(sourceToolAssistantUUID),
		}

		encoded, err := json.Marshal(line)
		if err != nil {
			return err
		}
		if _, err := file.Write(append(encoded, '\n')); err != nil {
			return err
		}
		if message.Type == "assistant" {
			for _, block := range sessionMessage.Content {
				if block.Type == "tool_use" && block.ID != "" {
					assistantUUIDByToolUseID[block.ID] = uuid
				}
			}
		}
		parentUUID = uuid
	}

	return nil
}

func completeClaudeCLISessionMessage(message claudeCLISDKMessage, uuid string, model string) claudeCLIMessageObject {
	out := message.Message
	if message.Type != "assistant" {
		return out
	}
	if out.ID == "" {
		out.ID = "msg_" + strings.ReplaceAll(uuid, "-", "")
	}
	if out.Type == "" {
		out.Type = "message"
	}
	if out.Model == "" {
		out.Model = model
	}
	if out.StopReason == "" {
		out.StopReason = claudeCLIStopReasonForAssistantContent(out.Content)
	}
	if out.StopSequence == nil {
		out.StopSequence = nil
	}
	if out.Usage == nil {
		out.Usage = map[string]any{
			"input_tokens":                 0,
			"output_tokens":                0,
			"cache_creation_input_tokens":  0,
			"cache_read_input_tokens":      0,
			"cache_creation":               map[string]any{"ephemeral_1h_input_tokens": 0, "ephemeral_5m_input_tokens": 0},
			"server_tool_use":              map[string]any{"web_search_requests": 0, "web_fetch_requests": 0},
			"service_tier":                 nil,
			"cache_creation_input_tokens_": nil,
		}
		delete(out.Usage, "cache_creation_input_tokens_")
	}
	return out
}

func claudeCLIStopReasonForAssistantContent(content []claudeCLIContentBlock) string {
	for _, block := range content {
		if block.Type == "tool_use" {
			return "tool_use"
		}
	}
	return "end_turn"
}

func optionalClaudeCLIParentUUID(uuid string) *string {
	if uuid == "" {
		return nil
	}
	return &uuid
}

func buildClaudeCLIStdin(prompt claudeCLISDKMessage) ([]byte, error) {
	prompt.SessionID = ""
	prompt.ParentToolUseID = nil
	encoded, err := json.Marshal(prompt)
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func sanitizeClaudeCLIProjectPath(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('-')
	}
	sanitized := b.String()
	if len(sanitized) <= 200 {
		return sanitized
	}
	sum := sha256.Sum256([]byte(name))
	return sanitized[:200] + "-" + hex.EncodeToString(sum[:6])
}

type claudeCLISessionLine struct {
	ParentUUID              *string                `json:"parentUuid"`
	IsSidechain             bool                   `json:"isSidechain"`
	Type                    string                 `json:"type"`
	SessionID               string                 `json:"sessionId"`
	UUID                    string                 `json:"uuid"`
	Timestamp               string                 `json:"timestamp"`
	Message                 claudeCLIMessageObject `json:"message"`
	CWD                     string                 `json:"cwd"`
	Version                 string                 `json:"version"`
	UserType                string                 `json:"userType"`
	Entrypoint              string                 `json:"entrypoint"`
	SourceToolAssistantUUID *string                `json:"sourceToolAssistantUUID,omitempty"`
}

func validateClaudeCLISessionIDForPath(sessionID string) error {
	if sessionID == "" {
		return errors.New("claude cli workspace: session id is empty")
	}
	if sessionID == "." || sessionID == ".." {
		return fmt.Errorf("claude cli workspace: unsafe session id %q", sessionID)
	}
	if filepath.Base(sessionID) != sessionID {
		return fmt.Errorf("claude cli workspace: unsafe session id %q", sessionID)
	}
	for _, char := range sessionID {
		if char == '/' || char == '\\' {
			return fmt.Errorf("claude cli workspace: unsafe session id %q", sessionID)
		}
	}
	return nil
}

func deriveClaudeCLIMessageUUID(sessionID string, index int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", sessionID, index)))
	bytes := append([]byte(nil), sum[:16]...)
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80

	hexValue := hex.EncodeToString(bytes)
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hexValue[0:8],
		hexValue[8:12],
		hexValue[12:16],
		hexValue[16:20],
		hexValue[20:32],
	)
}
