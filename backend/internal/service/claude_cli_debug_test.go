package service

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/stretchr/testify/require"
)

func TestClaudeCLIDebugStringRedactsTokens(t *testing.T) {
	got := claudeCLIDebugString(map[string]any{
		"authorization": "Bearer secret-token",
		"env":           "CLAUDE_CODE_OAUTH_TOKEN=oauth-token ANTHROPIC_AUTH_TOKEN=auth-token",
	})

	require.NotContains(t, got, "secret-token")
	require.NotContains(t, got, "oauth-token")
	require.NotContains(t, got, "auth-token")
	require.Contains(t, got, "Bearer <redacted>")
	require.Contains(t, got, "CLAUDE_CODE_OAUTH_TOKEN=<redacted>")
	require.Contains(t, got, "ANTHROPIC_AUTH_TOKEN=<redacted>")
}

func TestClaudeCLIDebugStringTruncatesPayload(t *testing.T) {
	got := claudeCLIDebugString(strings.Repeat("x", claudeCLIDebugPayloadMaxBytes+16))

	require.Len(t, got, claudeCLIDebugPayloadMaxBytes)
}

func TestLogClaudeCLIDebugUsesInfoLevel(t *testing.T) {
	t.Setenv("SUB2API_CLAUDE_CLI_DEBUG", "1")

	origStdout := os.Stdout
	origStderr := os.Stderr
	stdoutR, stdoutW, err := os.Pipe()
	require.NoError(t, err)
	stderrR, stderrW, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = stdoutW
	os.Stderr = stderrW
	t.Cleanup(func() {
		os.Stdout = origStdout
		os.Stderr = origStderr
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = stderrR.Close()
		_ = stderrW.Close()
	})
	require.NoError(t, logger.Init(logger.InitOptions{
		Level:       "debug",
		Format:      "json",
		ServiceName: "sub2api",
		Environment: "test",
		Output:      logger.OutputOptions{ToStdout: true},
		Sampling:    logger.SamplingOptions{Enabled: false},
	}))

	logClaudeCLIDebug("payload contains error and failed text")

	_ = stdoutW.Close()
	_ = stderrW.Close()
	stdout, _ := io.ReadAll(stdoutR)
	stderr, _ := io.ReadAll(stderrR)
	require.Contains(t, string(stdout), "[ClaudeCLIDebug] payload contains error and failed text")
	require.Empty(t, strings.TrimSpace(string(stderr)))
}
