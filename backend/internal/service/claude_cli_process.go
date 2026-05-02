package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

type claudeCLIProcessRequest struct {
	Command  string
	Args     []string
	Auth     claudeCLIProcessAuth
	ProxyURL string
	Dir      string
	Stdin    io.Reader
	Stdout   io.Writer
	Stderr   io.Writer
}

type claudeCLIProcessAuth struct {
	OAuthToken string
	AuthToken  string
	BaseURL    string
}

type claudeCLIProcessRunner struct{}

func (r claudeCLIProcessRunner) Run(ctx context.Context, req claudeCLIProcessRequest) error {
	if req.Command == "" {
		return errors.New("claude cli process: empty command")
	}
	if req.Auth.OAuthToken == "" && req.Auth.AuthToken == "" {
		return errors.New("claude cli process: empty account auth")
	}

	configDir := ""
	if req.Dir != "" {
		configDir = filepath.Join(req.Dir, ".claude")
		if err := os.MkdirAll(configDir, 0o700); err != nil {
			return fmt.Errorf("claude cli process: create config dir: %w", err)
		}
	}

	cmd := exec.CommandContext(ctx, req.Command, req.Args...)
	cmd.Dir = req.Dir
	cmd.Stdin = req.Stdin
	cmd.Stdout = req.Stdout
	cmd.Stderr = req.Stderr
	cmd.Env = append(os.Environ(),
		"CLAUDE_CODE_OAUTH_TOKEN="+req.Auth.OAuthToken,
		"ANTHROPIC_API_KEY=",
		"ANTHROPIC_AUTH_TOKEN="+req.Auth.AuthToken,
		"ANTHROPIC_BASE_URL="+req.Auth.BaseURL,
		"CLAUDE_CONFIG_DIR="+configDir,
		"CLAUDE_CODE_SIMPLE=",
		"CLAUDE_CODE_MAX_RETRIES=0",
	)
	cmd.Env = appendClaudeCLIProxyEnv(cmd.Env, req.ProxyURL)

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := 0
	if cmd.Process != nil {
		pid = cmd.Process.Pid
	}
	logger.LegacyPrintf("service.claude_cli", "started claude cli process pid=%d command=%s dir=%s args=%v", pid, req.Command, req.Dir, req.Args)
	err := cmd.Wait()
	logger.LegacyPrintf("service.claude_cli", "finished claude cli process pid=%d duration=%s err=%v", pid, time.Since(start), err)
	return err
}

func appendClaudeCLIProxyEnv(env []string, proxyURL string) []string {
	if proxyURL == "" {
		return env
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return env
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return env
	}
	return append(env,
		"HTTPS_PROXY="+proxyURL,
		"HTTP_PROXY="+proxyURL,
		"https_proxy="+proxyURL,
		"http_proxy="+proxyURL,
	)
}
