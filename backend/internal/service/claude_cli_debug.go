package service

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

const claudeCLIDebugPayloadMaxBytes = 64 * 1024

var (
	claudeCLIBearerTokenPattern = regexp.MustCompile(`(?i)(Bearer\s+)[A-Za-z0-9._~+/=-]+`)
	claudeCLIOAuthTokenPattern  = regexp.MustCompile(`(CLAUDE_CODE_OAUTH_TOKEN=)[^\s"]+`)
	claudeCLIAuthTokenPattern   = regexp.MustCompile(`(ANTHROPIC_AUTH_TOKEN=)[^\s"]+`)
)

func isClaudeCLIDebugEnabled() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv("SUB2API_CLAUDE_CLI_DEBUG")))
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

func logClaudeCLIDebug(format string, args ...any) {
	logClaudeCLIInfoDebug("[ClaudeCLIDebug] "+format, args...)
}

func logClaudeCLIMCPDebug(format string, args ...any) {
	logClaudeCLIInfoDebug("[ClaudeCLIMCPDebug] "+format, args...)
}

func logClaudeCLIInfoDebug(format string, args ...any) {
	if !isClaudeCLIDebugEnabled() {
		return
	}
	logger.L().WithOptions(zap.AddCallerSkip(1)).
		With(zap.String("component", "service.claude_cli")).
		Info(fmt.Sprintf(format, args...))
}

func logClaudeCLIDebugPayload(label string, value any) {
	if !isClaudeCLIDebugEnabled() {
		return
	}
	logClaudeCLIDebug("%s=%s", label, claudeCLIDebugString(value))
}

func claudeCLIDebugString(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return claudeCLIDebugSanitize(truncateString(v, claudeCLIDebugPayloadMaxBytes))
	case []byte:
		return claudeCLIDebugSanitize(truncateString(string(v), claudeCLIDebugPayloadMaxBytes))
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return claudeCLIDebugSanitize(truncateString(err.Error(), claudeCLIDebugPayloadMaxBytes))
		}
		return claudeCLIDebugSanitize(truncateString(string(encoded), claudeCLIDebugPayloadMaxBytes))
	}
}

func claudeCLIDebugSanitize(value string) string {
	value = claudeCLIBearerTokenPattern.ReplaceAllString(value, "${1}<redacted>")
	value = claudeCLIOAuthTokenPattern.ReplaceAllString(value, "${1}<redacted>")
	value = claudeCLIAuthTokenPattern.ReplaceAllString(value, "${1}<redacted>")
	return value
}
