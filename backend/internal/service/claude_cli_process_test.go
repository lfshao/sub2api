package service

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClaudeCLIProcessRunnerValidatesCommandAndToken(t *testing.T) {
	runner := claudeCLIProcessRunner{}

	err := runner.Run(context.Background(), claudeCLIProcessRequest{Auth: claudeCLIProcessAuth{OAuthToken: "token"}})
	require.EqualError(t, err, "claude cli process: empty command")

	err = runner.Run(context.Background(), claudeCLIProcessRequest{Command: "unused"})
	require.EqualError(t, err, "claude cli process: empty account auth")
}

func TestClaudeCLIProcessRunnerPassesTokenAndEmptyToolsArg(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-claude-cli.sh")
	tokenFile := filepath.Join(dir, "token.txt")
	argsFile := filepath.Join(dir, "args.txt")

	script := `#!/bin/sh
printf '%s' "$CLAUDE_CODE_OAUTH_TOKEN" > "$1"
shift
args_file="$1"
shift
: > "$args_file"
for arg in "$@"; do
	printf '%s\n' "$arg" >> "$args_file"
done
cat >/dev/null
`
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0700))

	var stdout strings.Builder
	var stderr strings.Builder
	runner := claudeCLIProcessRunner{}
	err := runner.Run(context.Background(), claudeCLIProcessRequest{
		Command: scriptPath,
		Args:    []string{tokenFile, argsFile, "--tools", ""},
		Auth:    claudeCLIProcessAuth{OAuthToken: "account-token"},
		Stdin:   strings.NewReader("{}\n"),
		Stdout:  &stdout,
		Stderr:  &stderr,
	})
	require.NoError(t, err)

	token, err := os.ReadFile(tokenFile)
	require.NoError(t, err)
	require.Equal(t, "account-token", string(token))

	args, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	require.Equal(t, "--tools\n\n", string(args))
}

func TestClaudeCLIProcessRunnerPassesAuthToken(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-claude-cli-auth-token.sh")
	apiKeyFile := filepath.Join(dir, "api-key.txt")
	authTokenFile := filepath.Join(dir, "auth-token.txt")
	oauthTokenFile := filepath.Join(dir, "oauth-token.txt")
	baseURLFile := filepath.Join(dir, "base-url.txt")

	script := `#!/bin/sh
printf '%s' "$ANTHROPIC_API_KEY" > "$1"
printf '%s' "$ANTHROPIC_AUTH_TOKEN" > "$2"
printf '%s' "$CLAUDE_CODE_OAUTH_TOKEN" > "$3"
printf '%s' "$ANTHROPIC_BASE_URL" > "$4"
cat >/dev/null
`
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0700))
	t.Setenv("ANTHROPIC_API_KEY", "parent-key")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "parent-oauth-token")

	runner := claudeCLIProcessRunner{}
	err := runner.Run(context.Background(), claudeCLIProcessRequest{
		Command: scriptPath,
		Args:    []string{apiKeyFile, authTokenFile, oauthTokenFile, baseURLFile},
		Auth: claudeCLIProcessAuth{
			AuthToken: "account-auth-token",
			BaseURL:   "https://api.anthropic.com",
		},
		Stdin:  strings.NewReader("{}\n"),
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	require.NoError(t, err)

	apiKey, err := os.ReadFile(apiKeyFile)
	require.NoError(t, err)
	require.Empty(t, string(apiKey))

	authToken, err := os.ReadFile(authTokenFile)
	require.NoError(t, err)
	require.Equal(t, "account-auth-token", string(authToken))

	oauthToken, err := os.ReadFile(oauthTokenFile)
	require.NoError(t, err)
	require.Empty(t, string(oauthToken))

	baseURL, err := os.ReadFile(baseURLFile)
	require.NoError(t, err)
	require.Equal(t, "https://api.anthropic.com", string(baseURL))
}

func TestClaudeCLIProcessRunnerPassesHTTPProxyEnv(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-claude-cli-proxy.sh")
	httpsProxyFile := filepath.Join(dir, "https-proxy.txt")
	httpProxyFile := filepath.Join(dir, "http-proxy.txt")
	lowerHTTPSProxyFile := filepath.Join(dir, "lower-https-proxy.txt")
	lowerHTTPProxyFile := filepath.Join(dir, "lower-http-proxy.txt")
	noProxyFile := filepath.Join(dir, "no-proxy.txt")
	lowerNoProxyFile := filepath.Join(dir, "lower-no-proxy.txt")

	script := `#!/bin/sh
printf '%s' "$HTTPS_PROXY" > "$1"
printf '%s' "$HTTP_PROXY" > "$2"
printf '%s' "$https_proxy" > "$3"
printf '%s' "$http_proxy" > "$4"
printf '%s' "$NO_PROXY" > "$5"
printf '%s' "$no_proxy" > "$6"
cat >/dev/null
`
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0700))
	t.Setenv("HTTPS_PROXY", "http://parent-https-proxy:8080")
	t.Setenv("HTTP_PROXY", "http://parent-http-proxy:8080")
	t.Setenv("https_proxy", "http://parent-lower-https-proxy:8080")
	t.Setenv("http_proxy", "http://parent-lower-http-proxy:8080")
	t.Setenv("NO_PROXY", "example.com .example.com")

	runner := claudeCLIProcessRunner{}
	err := runner.Run(context.Background(), claudeCLIProcessRequest{
		Command:  scriptPath,
		Args:     []string{httpsProxyFile, httpProxyFile, lowerHTTPSProxyFile, lowerHTTPProxyFile, noProxyFile, lowerNoProxyFile},
		Auth:     claudeCLIProcessAuth{OAuthToken: "account-token"},
		ProxyURL: "http://user:pass@proxy.example.com:8080",
		Stdin:    strings.NewReader("{}\n"),
		Stdout:   io.Discard,
		Stderr:   io.Discard,
	})
	require.NoError(t, err)

	for _, path := range []string{httpsProxyFile, httpProxyFile, lowerHTTPSProxyFile, lowerHTTPProxyFile} {
		value, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, "http://user:pass@proxy.example.com:8080", string(value))
	}
	for _, path := range []string{noProxyFile, lowerNoProxyFile} {
		value, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, "example.com,.example.com,127.0.0.1,localhost,::1", string(value))
	}
}

func TestMergeClaudeCLINoProxyKeepsWildcard(t *testing.T) {
	require.Equal(t, "*", mergeClaudeCLINoProxy("*", claudeCLILocalNoProxyHosts))
}

func TestClaudeCLIProcessRunnerIgnoresSOCKSProxyEnv(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-claude-cli-socks-proxy.sh")
	httpsProxyFile := filepath.Join(dir, "https-proxy.txt")
	httpProxyFile := filepath.Join(dir, "http-proxy.txt")

	script := `#!/bin/sh
printf '%s' "$HTTPS_PROXY" > "$1"
printf '%s' "$HTTP_PROXY" > "$2"
cat >/dev/null
`
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0700))
	t.Setenv("HTTPS_PROXY", "http://parent-https-proxy:8080")
	t.Setenv("HTTP_PROXY", "http://parent-http-proxy:8080")

	runner := claudeCLIProcessRunner{}
	err := runner.Run(context.Background(), claudeCLIProcessRequest{
		Command:  scriptPath,
		Args:     []string{httpsProxyFile, httpProxyFile},
		Auth:     claudeCLIProcessAuth{OAuthToken: "account-token"},
		ProxyURL: "socks5://proxy.example.com:1080",
		Stdin:    strings.NewReader("{}\n"),
		Stdout:   io.Discard,
		Stderr:   io.Discard,
	})
	require.NoError(t, err)

	httpsProxy, err := os.ReadFile(httpsProxyFile)
	require.NoError(t, err)
	require.Equal(t, "http://parent-https-proxy:8080", string(httpsProxy))
	httpProxy, err := os.ReadFile(httpProxyFile)
	require.NoError(t, err)
	require.Equal(t, "http://parent-http-proxy:8080", string(httpProxy))
}

func TestClaudeCLIProcessRunnerWiresProcessIOEnvAndDir(t *testing.T) {
	dir := t.TempDir()
	workDir := filepath.Join(dir, "work")
	require.NoError(t, os.Mkdir(workDir, 0700))

	scriptPath := filepath.Join(dir, "fake-claude-cli-wiring.sh")
	pwdFile := filepath.Join(dir, "pwd.txt")
	stdinFile := filepath.Join(dir, "stdin.txt")
	anthropicAPIKeyFile := filepath.Join(dir, "anthropic-api-key.txt")
	configDirFile := filepath.Join(dir, "config-dir.txt")
	simpleFile := filepath.Join(dir, "simple.txt")

	script := `#!/bin/sh
pwd > "$1"
cat > "$2"
printf '%s' "$ANTHROPIC_API_KEY" > "$3"
printf '%s' "$CLAUDE_CONFIG_DIR" > "$4"
printf '%s' "$CLAUDE_CODE_SIMPLE" > "$5"
printf 'stdout text'
printf 'stderr text' >&2
`
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0700))
	t.Setenv("ANTHROPIC_API_KEY", "parent-key")
	t.Setenv("CLAUDE_CONFIG_DIR", "/parent/claude")
	t.Setenv("CLAUDE_CODE_SIMPLE", "1")

	var stdout strings.Builder
	var stderr strings.Builder
	runner := claudeCLIProcessRunner{}
	err := runner.Run(context.Background(), claudeCLIProcessRequest{
		Command: scriptPath,
		Args:    []string{pwdFile, stdinFile, anthropicAPIKeyFile, configDirFile, simpleFile},
		Auth:    claudeCLIProcessAuth{OAuthToken: "account-token"},
		Dir:     workDir,
		Stdin:   strings.NewReader("stdin text\n"),
		Stdout:  &stdout,
		Stderr:  &stderr,
	})
	require.NoError(t, err)

	observedPWD, err := os.ReadFile(pwdFile)
	require.NoError(t, err)
	expectedPWD, err := filepath.EvalSymlinks(workDir)
	require.NoError(t, err)
	require.Equal(t, expectedPWD+"\n", string(observedPWD))

	stdin, err := os.ReadFile(stdinFile)
	require.NoError(t, err)
	require.Equal(t, "stdin text\n", string(stdin))

	anthropicAPIKey, err := os.ReadFile(anthropicAPIKeyFile)
	require.NoError(t, err)
	require.Empty(t, string(anthropicAPIKey))

	configDir, err := os.ReadFile(configDirFile)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(workDir, ".claude"), string(configDir))

	simple, err := os.ReadFile(simpleFile)
	require.NoError(t, err)
	require.Empty(t, string(simple))

	require.Equal(t, "stdout text", stdout.String())
	require.Equal(t, "stderr text", stderr.String())
}
