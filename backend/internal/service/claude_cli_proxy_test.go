package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type fakeClaudeCLIProcessRunner struct {
	req            claudeCLIProcessRequest
	run            func(req claudeCLIProcessRequest) error
	runWithContext func(ctx context.Context, req claudeCLIProcessRequest) error
	stdin          []byte
}

type fakeClaudeCLIForwarder struct {
	called bool
	result *ForwardResult
	err    error
}

func (f *fakeClaudeCLIForwarder) Forward(ctx context.Context, c *gin.Context, account *Account, parsed *ParsedRequest, startTime time.Time) (*ForwardResult, error) {
	f.called = true
	if f.result != nil || f.err != nil {
		return f.result, f.err
	}
	return &ForwardResult{Model: parsed.Model, Stream: parsed.Stream}, nil
}

type claudeCLIWebToolsForwardGroupRepo struct {
	GroupRepository
	groups map[int64]*Group
}

func (r *claudeCLIWebToolsForwardGroupRepo) GetByID(ctx context.Context, id int64) (*Group, error) {
	if group, ok := r.groups[id]; ok {
		return group, nil
	}
	return nil, ErrGroupNotFound
}

func (r *claudeCLIWebToolsForwardGroupRepo) GetByIDLite(ctx context.Context, id int64) (*Group, error) {
	if group, ok := r.groups[id]; ok {
		return group, nil
	}
	return nil, ErrGroupNotFound
}

type claudeCLIWebToolsForwardAccountRepo struct {
	AccountRepository
	accounts         []Account
	requestedGroupID int64
}

func (r *claudeCLIWebToolsForwardAccountRepo) ListSchedulableByGroupIDAndPlatforms(ctx context.Context, groupID int64, platforms []string) ([]Account, error) {
	r.requestedGroupID = groupID
	allowed := make(map[string]struct{}, len(platforms))
	for _, platform := range platforms {
		allowed[platform] = struct{}{}
	}
	var out []Account
	for _, account := range r.accounts {
		if _, ok := allowed[account.Platform]; ok && account.IsSchedulable() {
			out = append(out, account)
		}
	}
	return out, nil
}

func (r *claudeCLIWebToolsForwardAccountRepo) ListSchedulableByGroupIDAndPlatform(ctx context.Context, groupID int64, platform string) ([]Account, error) {
	return r.ListSchedulableByGroupIDAndPlatforms(ctx, groupID, []string{platform})
}

func (r *fakeClaudeCLIProcessRunner) Run(ctx context.Context, req claudeCLIProcessRequest) error {
	r.req = req
	if req.Stdin != nil {
		data, err := io.ReadAll(req.Stdin)
		if err != nil {
			return err
		}
		r.stdin = data
	}
	if r.runWithContext != nil {
		return r.runWithContext(ctx, req)
	}
	if r.run != nil {
		return r.run(req)
	}
	return nil
}

func TestGatewayServiceClaudeCLIWebFetchUsesNormalCLIProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"qwen3.6-plus","messages":[{"role":"user","content":"Fetch https://example.com"}],"tools":[{"type":"web_fetch_20250305","name":"web_fetch"}]}`)
	parsed, err := ParseGatewayRequest(body, PlatformAnthropic)
	require.NoError(t, err)

	account := &Account{
		ID:          17,
		Name:        "oauth-cli",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Extra:       map[string]any{"claude_cli_proxy_enabled": true},
	}
	cli := &fakeClaudeCLIForwarder{}
	svc := &GatewayService{claudeCLIProxy: cli}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	_, err = svc.Forward(context.Background(), c, account, parsed)
	require.NoError(t, err)
	require.True(t, cli.called)
}

func TestGatewayServiceClaudeCLIMarksRateLimitedFromProxyResult(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"qwen3.6-plus","messages":[{"role":"user","content":"hi"}]}`)
	parsed, err := ParseGatewayRequest(body, PlatformAnthropic)
	require.NoError(t, err)

	resetAt := time.Date(2026, 5, 1, 15, 50, 0, 0, time.UTC)
	account := &Account{
		ID:          17,
		Name:        "oauth-cli",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Extra: map[string]any{
			"claude_cli_proxy_enabled": true,
			"claude_cli_userID":        "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
	}
	repo := &sessionWindowMockRepo{}
	cli := &fakeClaudeCLIForwarder{
		result: &ForwardResult{
			Model:            parsed.Model,
			Stream:           parsed.Stream,
			RateLimitResetAt: &resetAt,
		},
	}
	svc := &GatewayService{
		accountRepo:      repo,
		rateLimitService: newRateLimitServiceForTest(repo),
		claudeCLIProxy:   cli,
		modelsListCache:  nil,
		httpUpstream:     nil,
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	_, err = svc.Forward(context.Background(), c, account, parsed)
	require.NoError(t, err)
	require.True(t, cli.called)
	require.Len(t, repo.rateLimitCalls, 1)
	require.Equal(t, int64(17), repo.rateLimitCalls[0].ID)
	require.Equal(t, resetAt, repo.rateLimitCalls[0].ResetAt)
	require.Len(t, repo.sessionWindowCalls, 1)
	require.Equal(t, "rejected", repo.sessionWindowCalls[0].Status)
	require.Equal(t, resetAt, *repo.sessionWindowCalls[0].End)
}

func TestGatewayServiceClaudeCLIWebToolsForwardRejectsOAuthCurrentRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"qwen3.6-plus","messages":[{"role":"user","content":"Perform a web search for the query: pcx160"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`)
	parsed, err := ParseGatewayRequest(body, PlatformAnthropic)
	require.NoError(t, err)

	account := &Account{
		ID:          17,
		Name:        "oauth-cli",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Extra:       map[string]any{"claude_cli_proxy_enabled": true},
	}
	cli := &fakeClaudeCLIForwarder{}
	svc := &GatewayService{claudeCLIProxy: cli}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	_, err = svc.Forward(context.Background(), c, account, parsed)
	require.ErrorContains(t, err, "not supported")
	require.False(t, cli.called)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "not supported")
}

func TestGatewayServiceClaudeCLIWebToolsForwardUsesConfiguredGroupAPIKeyRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const groupID int64 = 44
	body := []byte(`{"model":"qwen3.6-plus","messages":[{"role":"user","content":"Perform a web search for the query: pcx160"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`)
	parsed, err := ParseGatewayRequest(body, PlatformAnthropic)
	require.NoError(t, err)

	source := &Account{
		ID:          17,
		Name:        "oauth-cli",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Extra: map[string]any{
			"claude_cli_proxy_enabled":              true,
			"claude_cli_web_tools_forward_group_id": float64(groupID),
		},
	}
	target := Account{
		ID:          201,
		Name:        "apikey-target",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key": "group-api-key",
		},
		Status:      StatusActive,
		Schedulable: true,
	}

	upstream := &anthropicHTTPUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"id":"msg_1","type":"message","role":"assistant","model":"qwen3.6-plus","content":[{"type":"text","text":"searched"}],"usage":{"input_tokens":1,"output_tokens":1}}`,
			)),
		},
	}
	accountRepo := &claudeCLIWebToolsForwardAccountRepo{accounts: []Account{target}}
	cli := &fakeClaudeCLIForwarder{}
	svc := &GatewayService{
		accountRepo:     accountRepo,
		groupRepo:       &claudeCLIWebToolsForwardGroupRepo{groups: map[int64]*Group{groupID: {ID: groupID, Platform: PlatformAnthropic}}},
		cfg:             &config.Config{},
		httpUpstream:    upstream,
		claudeCLIProxy:  cli,
		modelsListCache: nil,
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	_, err = svc.Forward(context.Background(), c, source, parsed)
	require.NoError(t, err)
	require.False(t, cli.called)
	require.Equal(t, groupID, accountRepo.requestedGroupID)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, "group-api-key", getHeaderRaw(upstream.lastReq.Header, "x-api-key"))
	require.JSONEq(t, string(body), string(upstream.lastBody))
	require.Contains(t, rec.Body.String(), "searched")
}

func TestClaudeCLIProxyForwardNonStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const userID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	runner := &fakeClaudeCLIProcessRunner{
		run: func(req claudeCLIProcessRequest) error {
			configBytes, err := os.ReadFile(filepath.Join(req.Dir, ".claude", ".claude.json"))
			require.NoError(t, err)
			var config map[string]any
			require.NoError(t, json.Unmarshal(configBytes, &config))
			require.Equal(t, userID, config["userID"])
			_, err = io.WriteString(req.Stdout, claudeCLIStreamJSON(
				`{"type":"message_start","message":{"id":"msg_1","role":"assistant","model":"claude-opus-4-7","usage":{"input_tokens":2}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			))
			return err
		},
	}
	proxy := &ClaudeCLIProxy{
		runner:       runner,
		tempDir:      t.TempDir(),
		now:          func() time.Time { return time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC) },
		newSessionID: func() string { return "11111111-1111-4111-8111-111111111111" },
	}

	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`)
	parsed, err := ParseGatewayRequest(body, PlatformAnthropic)
	require.NoError(t, err)
	account := &Account{
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Extra: map[string]any{
			"claude_cli_proxy_enabled": true,
			"claude_cli_command":       "fake-claude",
			"claude_cli_userID":        userID,
		},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	result, err := proxy.Forward(context.Background(), c, account, parsed, time.Now())
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	var response map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	content, ok := response["content"].([]any)
	require.True(t, ok)
	require.Len(t, content, 1)
	block, ok := content[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "ok", block["text"])

	require.Equal(t, 2, result.Usage.InputTokens)
	require.Equal(t, 1, result.Usage.OutputTokens)
	require.Equal(t, "claude-opus-4-7", result.Model)
	require.Equal(t, "claude-opus-4-7", result.UpstreamModel)
	require.False(t, result.Stream)
	require.Equal(t, "fake-claude", runner.req.Command)
	require.Equal(t, claudeCLIProcessAuth{OAuthToken: "oauth-token"}, runner.req.Auth)
	require.NotContains(t, runner.req.Args, "--mcp-config")
	require.NotContains(t, runner.req.Args, "--strict-mcp-config")
	requireArgFollowedBy(t, runner.req.Args, "--tools", "")
	require.NotEmpty(t, runner.stdin)

	entries, err := os.ReadDir(proxy.tempDir)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestClaudeCLIProxyForwardReturnsClaudeGeneratedUserID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const userID = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	runner := &fakeClaudeCLIProcessRunner{
		run: func(req claudeCLIProcessRequest) error {
			_, err := os.ReadFile(filepath.Join(req.Dir, ".claude", ".claude.json"))
			require.ErrorIs(t, err, os.ErrNotExist)
			require.NoError(t, os.MkdirAll(filepath.Join(req.Dir, ".claude"), 0700))
			require.NoError(t, os.WriteFile(filepath.Join(req.Dir, ".claude", ".claude.json"), []byte(`{"userID":"`+userID+`"}`), 0600))
			_, err = io.WriteString(req.Stdout, claudeCLIStreamJSON(
				`{"type":"message_start","message":{"id":"msg_1","role":"assistant","model":"claude-opus-4-7","usage":{"input_tokens":2}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			))
			return err
		},
	}
	proxy := &ClaudeCLIProxy{
		runner:       runner,
		tempDir:      t.TempDir(),
		now:          func() time.Time { return time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC) },
		newSessionID: func() string { return "11111111-1111-4111-8111-111111111111" },
	}

	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`)
	parsed, err := ParseGatewayRequest(body, PlatformAnthropic)
	require.NoError(t, err)
	account := &Account{
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Extra: map[string]any{
			"claude_cli_proxy_enabled": true,
			"claude_cli_command":       "fake-claude",
		},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	result, err := proxy.Forward(context.Background(), c, account, parsed, time.Now())
	require.NoError(t, err)

	require.Equal(t, userID, result.ClaudeCLIUserID)
}

func TestClaudeCLIProxyForwardReturnsRateLimitSignalFromCLIOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)

	location, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	now := time.Date(2026, 5, 1, 15, 0, 0, 0, location)

	runner := &fakeClaudeCLIProcessRunner{
		run: func(req claudeCLIProcessRequest) error {
			_, err := req.Stdout.Write([]byte(`{"type":"assistant","message":{"id":"msg_1","model":"qwen3.6-plus","role":"assistant","type":"message","usage":{"input_tokens":0,"output_tokens":0},"content":[{"type":"text","text":"You've hit your limit · resets 3:50pm (Asia/Shanghai)"}]},"session_id":"s1"}` + "\n" +
				`{"type":"result","subtype":"success","is_error":true,"api_error_status":429,"result":"You've hit your limit · resets 3:50pm (Asia/Shanghai)","session_id":"s1"}` + "\n"))
			if err != nil {
				return err
			}
			return errors.New("exit status 1")
		},
	}
	proxy := &ClaudeCLIProxy{
		runner:       runner,
		tempDir:      t.TempDir(),
		now:          func() time.Time { return now },
		newSessionID: func() string { return "session-1" },
		mcpServer:    newClaudeCLIHTTPMCPServer(func() time.Time { return now }),
	}
	account := &Account{
		ID:          17,
		Name:        "oauth-cli",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Extra: map[string]any{
			"claude_cli_proxy_enabled": true,
			"claude_cli_userID":        "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
	}
	parsed, err := ParseGatewayRequest([]byte(`{"model":"qwen3.6-plus","messages":[{"role":"user","content":"hi"}]}`), PlatformAnthropic)
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	result, err := proxy.Forward(context.Background(), c, account, parsed, now)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.RateLimitResetAt)
	require.Equal(t, time.Date(2026, 5, 1, 15, 50, 0, 0, location), *result.RateLimitResetAt)
}

func TestClaudeCLIProxyForwardReusesClientSessionIDHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const clientSessionID = "22222222-2222-4222-8222-222222222222"
	runner := &fakeClaudeCLIProcessRunner{
		run: func(req claudeCLIProcessRequest) error {
			_, err := io.WriteString(req.Stdout, claudeCLIStreamJSON(
				`{"type":"message_start","message":{"id":"msg_1","role":"assistant","model":"claude-opus-4-7","usage":{"input_tokens":1}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			))
			return err
		},
	}
	proxy := &ClaudeCLIProxy{
		runner:       runner,
		tempDir:      t.TempDir(),
		now:          func() time.Time { return time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC) },
		newSessionID: func() string { return "11111111-1111-4111-8111-111111111111" },
	}

	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`)
	parsed, err := ParseGatewayRequest(body, PlatformAnthropic)
	require.NoError(t, err)
	account := &Account{
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Extra: map[string]any{
			"claude_cli_proxy_enabled": true,
			"claude_cli_command":       "fake-claude",
		},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("X-Claude-Code-Session-Id", clientSessionID)

	_, err = proxy.Forward(context.Background(), c, account, parsed, time.Now())
	require.NoError(t, err)

	requireArgFollowedBy(t, runner.req.Args, "--session-id", clientSessionID)
}

func TestResolveClaudeCLISessionIDReusesClientRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const fallbackSessionID = "11111111-1111-4111-8111-111111111111"
	const headerSessionID = "22222222-2222-4222-8222-222222222222"
	const metadataSessionID = "33333333-3333-4333-8333-333333333333"
	const deviceID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	newContext := func(header string) *gin.Context {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		if header != "" {
			c.Request.Header.Set("X-Claude-Code-Session-Id", header)
		}
		return c
	}
	metadataParsed := &ParsedRequest{
		MetadataUserID: fmt.Sprintf(`{"device_id":"%s","account_uuid":"","session_id":"%s"}`, deviceID, metadataSessionID),
	}

	tests := []struct {
		name   string
		c      *gin.Context
		parsed *ParsedRequest
		want   string
	}{
		{
			name:   "header wins",
			c:      newContext(headerSessionID),
			parsed: metadataParsed,
			want:   headerSessionID,
		},
		{
			name:   "metadata session is reused without header",
			c:      newContext(""),
			parsed: metadataParsed,
			want:   metadataSessionID,
		},
		{
			name:   "invalid header falls back to metadata",
			c:      newContext("../bad"),
			parsed: metadataParsed,
			want:   metadataSessionID,
		},
		{
			name:   "fallback generates when no client session is valid",
			c:      newContext("../bad"),
			parsed: &ParsedRequest{MetadataUserID: "not-a-claude-code-user-id"},
			want:   fallbackSessionID,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveClaudeCLISessionID(tt.c, tt.parsed, func() string { return fallbackSessionID })
			require.Equal(t, tt.want, got)
		})
	}
}

func TestClaudeCLIProxyForwardAppliesAccountModelMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)

	runner := &fakeClaudeCLIProcessRunner{
		run: func(req claudeCLIProcessRequest) error {
			_, err := io.WriteString(req.Stdout, claudeCLIStreamJSON(
				`{"type":"message_start","message":{"id":"msg_1","role":"assistant","model":"claude-sonnet-4-5","usage":{"input_tokens":2}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			))
			return err
		},
	}
	proxy := &ClaudeCLIProxy{
		runner:       runner,
		tempDir:      t.TempDir(),
		now:          func() time.Time { return time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC) },
		newSessionID: func() string { return "11111111-1111-4111-8111-111111111111" },
	}

	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`)
	parsed, err := ParseGatewayRequest(body, PlatformAnthropic)
	require.NoError(t, err)
	account := &Account{
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key": "token",
			"model_mapping": map[string]any{
				"claude-opus-4-7": "claude-sonnet-4-5",
			},
		},
		Extra: map[string]any{
			"claude_cli_proxy_enabled": true,
			"claude_cli_command":       "fake-claude",
		},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	result, err := proxy.Forward(context.Background(), c, account, parsed, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "claude-opus-4-7", result.Model)
	require.Equal(t, "claude-sonnet-4-5", result.UpstreamModel)
	requireArgFollowedBy(t, runner.req.Args, "--model", "claude-sonnet-4-5")

	var response map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	require.Equal(t, "claude-opus-4-7", response["model"])
}

func TestClaudeCLIProxyPassesOutputEffortToCLICommand(t *testing.T) {
	gin.SetMode(gin.TestMode)

	runner := &fakeClaudeCLIProcessRunner{
		run: func(req claudeCLIProcessRequest) error {
			_, err := io.WriteString(req.Stdout, claudeCLIStreamJSON(
				`{"type":"message_start","message":{"id":"msg_1","role":"assistant","model":"claude-opus-4-7","usage":{"input_tokens":1}}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			))
			return err
		},
	}
	proxy := &ClaudeCLIProxy{
		runner:       runner,
		tempDir:      t.TempDir(),
		now:          func() time.Time { return time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC) },
		newSessionID: func() string { return "11111111-1111-4111-8111-111111111111" },
	}

	body := []byte(`{"model":"claude-opus-4-7","output_config":{"effort":"xhigh"},"messages":[{"role":"user","content":"hi"}]}`)
	parsed, err := ParseGatewayRequest(body, PlatformAnthropic)
	require.NoError(t, err)
	account := &Account{
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "token"},
		Extra:       map[string]any{"claude_cli_proxy_enabled": true, "claude_cli_command": "fake-claude"},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	result, err := proxy.Forward(context.Background(), c, account, parsed, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	requireArgFollowedBy(t, runner.req.Args, "--effort", "xhigh")
}

func TestClaudeCLIProxyForwardAnthropicAPIKeyAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	runner := &fakeClaudeCLIProcessRunner{
		run: func(req claudeCLIProcessRequest) error {
			_, err := io.WriteString(req.Stdout, claudeCLIStreamJSON(
				`{"type":"message_start","message":{"id":"msg_1","role":"assistant","model":"claude-opus-4-7","usage":{"input_tokens":1}}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			))
			return err
		},
	}
	proxy := &ClaudeCLIProxy{
		runner:       runner,
		tempDir:      t.TempDir(),
		now:          func() time.Time { return time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC) },
		newSessionID: func() string { return "11111111-1111-4111-8111-111111111111" },
	}

	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`)
	parsed, err := ParseGatewayRequest(body, PlatformAnthropic)
	require.NoError(t, err)
	account := &Account{
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-ant-test",
			"base_url": "https://api.anthropic.com",
		},
		Extra: map[string]any{"claude_cli_proxy_enabled": true, "claude_cli_command": "fake-claude"},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	result, err := proxy.Forward(context.Background(), c, account, parsed, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, claudeCLIProcessAuth{
		AuthToken: "sk-ant-test",
		BaseURL:   "https://api.anthropic.com",
	}, runner.req.Auth)
}

func TestClaudeCLIProxyForwardPassesAccountProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)

	runner := &fakeClaudeCLIProcessRunner{
		run: func(req claudeCLIProcessRequest) error {
			_, err := io.WriteString(req.Stdout, claudeCLIStreamJSON(
				`{"type":"message_start","message":{"id":"msg_1","role":"assistant","model":"claude-opus-4-7","usage":{"input_tokens":1}}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			))
			return err
		},
	}
	proxy := &ClaudeCLIProxy{
		runner:       runner,
		tempDir:      t.TempDir(),
		now:          func() time.Time { return time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC) },
		newSessionID: func() string { return "11111111-1111-4111-8111-111111111111" },
	}

	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`)
	parsed, err := ParseGatewayRequest(body, PlatformAnthropic)
	require.NoError(t, err)
	account := &Account{
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Extra:       map[string]any{"claude_cli_proxy_enabled": true, "claude_cli_command": "fake-claude"},
		Proxy: &Proxy{
			Protocol: "http",
			Host:     "proxy.example.com",
			Port:     8080,
			Username: "user",
			Password: "pass",
		},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	_, err = proxy.Forward(context.Background(), c, account, parsed, time.Now())
	require.NoError(t, err)
	require.Equal(t, "http://user:pass@proxy.example.com:8080", runner.req.ProxyURL)
}

func TestClaudeCLIProxyForwardWithToolsAddsMCPConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)

	runner := &fakeClaudeCLIProcessRunner{
		run: func(req claudeCLIProcessRequest) error {
			_, err := io.WriteString(req.Stdout, claudeCLIStreamJSON(
				`{"type":"message_start","message":{"id":"msg_1","role":"assistant","model":"claude-opus-4-7","usage":{"input_tokens":1}}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			))
			return err
		},
	}
	proxy := &ClaudeCLIProxy{
		runner:       runner,
		tempDir:      t.TempDir(),
		now:          func() time.Time { return time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC) },
		newSessionID: func() string { return "11111111-1111-4111-8111-111111111111" },
	}

	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"lookup","description":"Lookup a thing","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}]}`)
	parsed, err := ParseGatewayRequest(body, PlatformAnthropic)
	require.NoError(t, err)
	account := &Account{
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Extra:       map[string]any{"claude_cli_proxy_enabled": true, "claude_cli_command": "fake-claude"},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	_, err = proxy.Forward(context.Background(), c, account, parsed, time.Now())
	require.NoError(t, err)

	mcpConfigPath := requireArgFollowedBy(t, runner.req.Args, "--mcp-config", "")
	requireArgPresentAfter(t, runner.req.Args, "--strict-mcp-config", "--mcp-config")
	requireArgPresentAfter(t, runner.req.Args, "--tools", "--strict-mcp-config")
	require.True(t, filepath.IsAbs(mcpConfigPath))
	_, err = os.Stat(mcpConfigPath)
	require.True(t, os.IsNotExist(err), "mcp config should be cleaned up, stat err: %v", err)
}

func TestClaudeCLIProxyForwardRegistersWebToolsInMCPBridge(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var bridgeTools []claudeCLITool
	runner := &fakeClaudeCLIProcessRunner{
		run: func(req claudeCLIProcessRequest) error {
			bridgeTools = readMCPHTTPToolsFromRequest(t, req)
			_, err := io.WriteString(req.Stdout, claudeCLIStreamJSON(
				`{"type":"message_start","message":{"id":"msg_1","role":"assistant","model":"claude-opus-4-7","usage":{"input_tokens":1}}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			))
			return err
		},
	}
	proxy := &ClaudeCLIProxy{
		runner:       runner,
		tempDir:      t.TempDir(),
		now:          func() time.Time { return time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC) },
		newSessionID: func() string { return "11111111-1111-4111-8111-111111111111" },
	}

	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"lookup","description":"Lookup a thing","input_schema":{"type":"object"}},{"name":"WebSearch","description":"Search the web","input_schema":{"type":"object"}},{"name":"WebFetch","description":"Fetch a URL","input_schema":{"type":"object"}}]}`)
	parsed, err := ParseGatewayRequest(body, PlatformAnthropic)
	require.NoError(t, err)
	account := &Account{
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Extra: map[string]any{
			"claude_cli_proxy_enabled": true,
			"claude_cli_command":       "fake-claude",
		},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	_, err = proxy.Forward(context.Background(), c, account, parsed, time.Now())
	require.NoError(t, err)

	requireArgFollowedBy(t, runner.req.Args, "--tools", "")
	requireClaudeCLIToolsByName(t, bridgeTools, map[string]claudeCLITool{
		"lookup":    {Name: "lookup", Description: "Lookup a thing", InputSchema: map[string]any{"type": "object"}},
		"WebSearch": {Name: "WebSearch", Description: "Search the web", InputSchema: map[string]any{"type": "object"}},
		"WebFetch":  {Name: "WebFetch", Description: "Fetch a URL", InputSchema: map[string]any{"type": "object"}},
	})
}

func TestClaudeCLIProxyForwardRegistersWebToolsInMCPBridgeWithoutAccountEnabledTools(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var bridgeTools []claudeCLITool
	runner := &fakeClaudeCLIProcessRunner{
		run: func(req claudeCLIProcessRequest) error {
			bridgeTools = readMCPHTTPToolsFromRequest(t, req)
			_, err := io.WriteString(req.Stdout, claudeCLIStreamJSON(
				`{"type":"message_start","message":{"id":"msg_1","role":"assistant","model":"claude-opus-4-7","usage":{"input_tokens":1}}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			))
			return err
		},
	}
	proxy := &ClaudeCLIProxy{
		runner:       runner,
		tempDir:      t.TempDir(),
		now:          func() time.Time { return time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC) },
		newSessionID: func() string { return "11111111-1111-4111-8111-111111111111" },
	}

	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"lookup","description":"Lookup a thing","input_schema":{"type":"object"}},{"name":"WebSearch","description":"Search the web","input_schema":{"type":"object"}},{"name":"WebFetch","description":"Fetch a URL","input_schema":{"type":"object"}}]}`)
	parsed, err := ParseGatewayRequest(body, PlatformAnthropic)
	require.NoError(t, err)
	account := &Account{
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Extra:       map[string]any{"claude_cli_proxy_enabled": true, "claude_cli_command": "fake-claude"},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	_, err = proxy.Forward(context.Background(), c, account, parsed, time.Now())
	require.NoError(t, err)

	requireArgFollowedBy(t, runner.req.Args, "--tools", "")
	requireClaudeCLIToolsByName(t, bridgeTools, map[string]claudeCLITool{
		"lookup":    {Name: "lookup", Description: "Lookup a thing", InputSchema: map[string]any{"type": "object"}},
		"WebSearch": {Name: "WebSearch", Description: "Search the web", InputSchema: map[string]any{"type": "object"}},
		"WebFetch":  {Name: "WebFetch", Description: "Fetch a URL", InputSchema: map[string]any{"type": "object"}},
	})
}

func TestClaudeCLIProxyForwardWithToolsFiltersRawToolUseWhenProcessCompletes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	runner := &fakeClaudeCLIProcessRunner{
		run: func(req claudeCLIProcessRequest) error {
			_, err := io.WriteString(req.Stdout, claudeCLIStreamJSON(
				`{"type":"message_start","message":{"id":"msg_bad","role":"assistant","model":"claude-opus-4-7","usage":{"input_tokens":1}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_bad","name":"Bash","input":{}}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"date\"}"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
				`{"type":"message_start","message":{"id":"msg_final","role":"assistant","model":"claude-opus-4-7","usage":{"input_tokens":1}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"final answer"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			))
			return err
		},
	}
	proxy := &ClaudeCLIProxy{
		runner:       runner,
		tempDir:      t.TempDir(),
		now:          func() time.Time { return time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC) },
		newSessionID: func() string { return "11111111-1111-4111-8111-111111111111" },
	}

	body := []byte(`{"model":"claude-opus-4-7","stream":true,"messages":[{"role":"user","content":"time?"}],"tools":[{"name":"Bash","description":"Run shell commands","input_schema":{"type":"object"}}]}`)
	parsed, err := ParseGatewayRequest(body, PlatformAnthropic)
	require.NoError(t, err)
	account := &Account{
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Extra:       map[string]any{"claude_cli_proxy_enabled": true, "claude_cli_command": "fake-claude"},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	_, err = proxy.Forward(context.Background(), c, account, parsed, time.Now())
	require.NoError(t, err)
	require.NotContains(t, rec.Body.String(), `"name":"Bash"`)
	require.NotContains(t, rec.Body.String(), "toolu_bad")
	require.Contains(t, rec.Body.String(), "final answer")
}

func TestClaudeCLIProxyForwardCompletesClientToolCallAcrossRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)

	toolCallStarted := make(chan struct{})
	var runCount int
	runner := &fakeClaudeCLIProcessRunner{
		runWithContext: func(ctx context.Context, req claudeCLIProcessRequest) error {
			runCount++
			require.Equal(t, 1, runCount)
			_, err := io.WriteString(req.Stdout, claudeCLIStreamJSON(
				`{"type":"message_start","message":{"id":"msg_tool","role":"assistant","model":"claude-opus-4-7","usage":{"input_tokens":2}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"I should call lookup."}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig_1"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_cli","name":"lookup","input":{}}}`,
				`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"hello\"}"}}`,
				`{"type":"content_block_stop","index":1}`,
				`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			))
			require.NoError(t, err)
			resultCh := make(chan string, 1)
			go func() {
				resultCh <- callMCPToolFromRequestWithParams(t, req, `{"name":"lookup","arguments":{"q":"hello"},"_meta":{"claudecode/toolUseId":"toolu_cli"}}`, toolCallStarted)
			}()
			select {
			case result := <-resultCh:
				require.Equal(t, "client result", result)
			case <-ctx.Done():
				return ctx.Err()
			}
			_, err = io.WriteString(req.Stdout, claudeCLIStreamJSON(
				`{"type":"message_start","message":{"id":"msg_final","role":"assistant","model":"claude-opus-4-7","usage":{"input_tokens":2}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			))
			return err
		},
	}
	proxy := &ClaudeCLIProxy{
		runner:       runner,
		tempDir:      t.TempDir(),
		now:          func() time.Time { return time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC) },
		newSessionID: func() string { return "11111111-1111-4111-8111-111111111111" },
	}
	account := &Account{
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Extra:       map[string]any{"claude_cli_proxy_enabled": true, "claude_cli_command": "fake-claude"},
	}

	firstBody := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"lookup","description":"Lookup a thing","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}]}`)
	firstParsed, err := ParseGatewayRequest(firstBody, PlatformAnthropic)
	require.NoError(t, err)
	firstRec := httptest.NewRecorder()
	firstCtx, _ := gin.CreateTestContext(firstRec)
	firstCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	firstDone := make(chan error, 1)
	go func() {
		_, err := proxy.Forward(context.Background(), firstCtx, account, firstParsed, time.Now())
		firstDone <- err
	}()

	waitForChannel(t, toolCallStarted, "mcp tool call")
	select {
	case err := <-firstDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("first Forward did not return tool_use while MCP call was pending")
	}

	var firstMessage map[string]any
	require.NoError(t, json.Unmarshal(firstRec.Body.Bytes(), &firstMessage))
	require.Equal(t, "tool_use", firstMessage["stop_reason"])
	firstContent := firstMessage["content"].([]any)
	require.Len(t, firstContent, 2)
	thinking := firstContent[0].(map[string]any)
	require.Equal(t, "thinking", thinking["type"])
	require.Equal(t, "I should call lookup.", thinking["thinking"])
	require.Equal(t, "sig_1", thinking["signature"])
	firstToolUse := firstContent[1].(map[string]any)
	require.Equal(t, "lookup", firstToolUse["name"])
	toolUseID := firstToolUse["id"].(string)
	require.NotEmpty(t, toolUseID)

	secondBody := []byte(fmt.Sprintf(`{"model":"claude-opus-4-7","messages":[{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"lookup","input":{"q":"hello"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"client result"}]}],"tools":[{"name":"lookup","description":"Lookup a thing","input_schema":{"type":"object"}}]}`, toolUseID, toolUseID))
	secondParsed, err := ParseGatewayRequest(secondBody, PlatformAnthropic)
	require.NoError(t, err)
	secondRec := httptest.NewRecorder()
	secondCtx, _ := gin.CreateTestContext(secondRec)
	secondCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	result, err := proxy.Forward(context.Background(), secondCtx, account, secondParsed, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1, runCount)

	var secondMessage map[string]any
	require.NoError(t, json.Unmarshal(secondRec.Body.Bytes(), &secondMessage))
	require.Equal(t, "end_turn", secondMessage["stop_reason"])
	secondContent := secondMessage["content"].([]any)
	text := secondContent[0].(map[string]any)
	require.Equal(t, "done", text["text"])
}

func TestClaudeCLIProxyForwardKeepsRunnerPendingAfterReturningToolUse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	toolCallStarted := make(chan struct{})
	runnerDone := make(chan struct{})
	runner := &fakeClaudeCLIProcessRunner{
		runWithContext: func(ctx context.Context, req claudeCLIProcessRequest) error {
			defer close(runnerDone)
			_, err := io.WriteString(req.Stdout, claudeCLIStreamJSON(
				`{"type":"message_start","message":{"id":"msg_tool","role":"assistant","model":"claude-opus-4-7","usage":{"input_tokens":2}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_cli","name":"lookup","input":{}}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"hello\"}"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			))
			require.NoError(t, err)
			resultCh := make(chan string, 1)
			go func() {
				resultCh <- callMCPToolFromRequest(t, req, "lookup", map[string]any{"q": "hello"}, toolCallStarted)
			}()
			select {
			case <-resultCh:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
	proxy := &ClaudeCLIProxy{
		runner:       runner,
		tempDir:      t.TempDir(),
		now:          func() time.Time { return time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC) },
		newSessionID: func() string { return "11111111-1111-4111-8111-111111111111" },
	}
	account := &Account{
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Extra:       map[string]any{"claude_cli_proxy_enabled": true, "claude_cli_command": "fake-claude"},
	}

	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"lookup","description":"Lookup a thing","input_schema":{"type":"object"}}]}`)
	parsed, err := ParseGatewayRequest(body, PlatformAnthropic)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	done := make(chan error, 1)
	go func() {
		_, err := proxy.Forward(context.Background(), c, account, parsed, time.Now())
		done <- err
	}()

	waitForChannel(t, toolCallStarted, "mcp tool call")
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Forward did not return tool_use")
	}
	select {
	case <-runnerDone:
		t.Fatal("runner stopped before client returned tool_result")
	case <-time.After(50 * time.Millisecond):
	}

	var message map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &message))
	content := message["content"].([]any)
	toolUseID := content[0].(map[string]any)["id"].(string)
	proxy.pendingMu.Lock()
	pending := proxy.pendingByToolUseID[toolUseID]
	proxy.pendingMu.Unlock()
	proxy.closePendingToolRun(pending, true)
	waitForChannel(t, runnerDone, "runner shutdown")
}

func TestWriteClaudeCLIToolUseResponseIncludesThinkingInStream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	err := writeClaudeCLIToolUseResponse(
		c,
		&claudeCLIInput{Model: "claude-opus-4-7"},
		&ParsedRequest{Stream: true},
		[]claudeCLIToolCall{{ID: "toolu_1", Name: "lookup", Input: map[string]any{"q": "hello"}}},
		[]map[string]any{{"type": "thinking", "thinking": "Need lookup.", "signature": "sig_1"}},
	)
	require.NoError(t, err)
	body := rec.Body.String()
	require.Contains(t, body, `"type":"thinking"`)
	require.Contains(t, body, `"thinking":"Need lookup."`)
	require.Contains(t, body, `"signature":"sig_1"`)
	require.Contains(t, body, `"index":1`)
	require.Contains(t, body, `"type":"tool_use"`)
	require.Contains(t, body, `"id":"toolu_1"`)
}

func TestClaudeCLIProxyForwardCompletesMultipleClientToolCallsAcrossRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)

	firstToolStarted := make(chan struct{})
	secondToolStarted := make(chan struct{})
	var runCount int
	runner := &fakeClaudeCLIProcessRunner{
		runWithContext: func(ctx context.Context, req claudeCLIProcessRequest) error {
			runCount++
			require.Equal(t, 1, runCount)
			_, err := io.WriteString(req.Stdout, claudeCLIStreamJSON(
				`{"type":"message_start","message":{"id":"msg_tool","role":"assistant","model":"claude-opus-4-7","usage":{"input_tokens":2}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_cli_1","name":"lookup","input":{}}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"one\"}"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_cli_2","name":"fetch","input":{}}}`,
				`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"url\":\"https://example.com\"}"}}`,
				`{"type":"content_block_stop","index":1}`,
				`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			))
			require.NoError(t, err)
			firstResultCh := make(chan string, 1)
			secondResultCh := make(chan string, 1)
			go func() {
				firstResultCh <- callMCPToolFromRequest(t, req, "lookup", map[string]any{"q": "one"}, firstToolStarted)
			}()
			go func() {
				secondResultCh <- callMCPToolFromRequest(t, req, "fetch", map[string]any{"url": "https://example.com"}, secondToolStarted)
			}()
			select {
			case <-firstResultCh:
			case <-ctx.Done():
				return ctx.Err()
			}
			select {
			case <-secondResultCh:
			case <-ctx.Done():
				return ctx.Err()
			}
			_, err = io.WriteString(req.Stdout, claudeCLIStreamJSON(
				`{"type":"message_start","message":{"id":"msg_final","role":"assistant","model":"claude-opus-4-7","usage":{"input_tokens":2}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"multi done"}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			))
			return err
		},
	}
	proxy := &ClaudeCLIProxy{
		runner:       runner,
		tempDir:      t.TempDir(),
		now:          func() time.Time { return time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC) },
		newSessionID: func() string { return "11111111-1111-4111-8111-111111111111" },
	}
	account := &Account{
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Extra:       map[string]any{"claude_cli_proxy_enabled": true, "claude_cli_command": "fake-claude"},
	}

	firstBody := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"lookup","description":"Lookup a thing","input_schema":{"type":"object"}},{"name":"fetch","description":"Fetch a thing","input_schema":{"type":"object"}}]}`)
	firstParsed, err := ParseGatewayRequest(firstBody, PlatformAnthropic)
	require.NoError(t, err)
	firstRec := httptest.NewRecorder()
	firstCtx, _ := gin.CreateTestContext(firstRec)
	firstCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	firstDone := make(chan error, 1)
	go func() {
		_, err := proxy.Forward(context.Background(), firstCtx, account, firstParsed, time.Now())
		firstDone <- err
	}()
	waitForChannel(t, firstToolStarted, "first mcp tool call")
	waitForChannel(t, secondToolStarted, "second mcp tool call")
	select {
	case err := <-firstDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("first Forward did not return tool_use while MCP calls were pending")
	}

	var firstMessage map[string]any
	require.NoError(t, json.Unmarshal(firstRec.Body.Bytes(), &firstMessage))
	require.Equal(t, "tool_use", firstMessage["stop_reason"])
	firstContent := firstMessage["content"].([]any)
	require.Len(t, firstContent, 2)
	toolUseByName := make(map[string]map[string]any)
	for _, rawBlock := range firstContent {
		block := rawBlock.(map[string]any)
		toolUseByName[block["name"].(string)] = block
	}
	require.Contains(t, toolUseByName, "lookup")
	require.Contains(t, toolUseByName, "fetch")
	firstToolUseID := toolUseByName["lookup"]["id"].(string)
	secondToolUseID := toolUseByName["fetch"]["id"].(string)

	secondBody := []byte(fmt.Sprintf(`{"model":"claude-opus-4-7","messages":[{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"lookup","input":{"q":"one"}},{"type":"tool_use","id":%q,"name":"fetch","input":{"url":"https://example.com"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"lookup result"},{"type":"tool_result","tool_use_id":%q,"content":"fetch result"}]}],"tools":[{"name":"lookup","description":"Lookup a thing","input_schema":{"type":"object"}},{"name":"fetch","description":"Fetch a thing","input_schema":{"type":"object"}}]}`, firstToolUseID, secondToolUseID, firstToolUseID, secondToolUseID))
	secondParsed, err := ParseGatewayRequest(secondBody, PlatformAnthropic)
	require.NoError(t, err)
	secondRec := httptest.NewRecorder()
	secondCtx, _ := gin.CreateTestContext(secondRec)
	secondCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	result, err := proxy.Forward(context.Background(), secondCtx, account, secondParsed, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)

	var secondMessage map[string]any
	require.NoError(t, json.Unmarshal(secondRec.Body.Bytes(), &secondMessage))
	require.Equal(t, "end_turn", secondMessage["stop_reason"])
	text := secondMessage["content"].([]any)[0].(map[string]any)
	require.Equal(t, "multi done", text["text"])
}

func TestClaudeCLIProxyForwardCompletesClientToolCallInSameCLIProcess(t *testing.T) {
	gin.SetMode(gin.TestMode)

	toolCallStarted := make(chan struct{})
	var runCount int
	runner := &fakeClaudeCLIProcessRunner{
		runWithContext: func(ctx context.Context, req claudeCLIProcessRequest) error {
			runCount++
			require.Equal(t, 1, runCount)
			_, err := io.WriteString(req.Stdout, claudeCLIStreamJSON(
				`{"type":"message_start","message":{"id":"msg_tool","role":"assistant","model":"claude-opus-4-7","usage":{"input_tokens":2}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_cli_1","name":"lookup","input":{}}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"one\"}"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			))
			require.NoError(t, err)
			resultCh := make(chan string, 1)
			go func() {
				resultCh <- callMCPToolFromRequest(t, req, "lookup", map[string]any{"q": "one"}, toolCallStarted)
			}()
			select {
			case result := <-resultCh:
				require.Equal(t, "lookup result", result)
			case <-ctx.Done():
				return ctx.Err()
			}
			_, err = io.WriteString(req.Stdout, claudeCLIStreamJSON(
				`{"type":"message_start","message":{"id":"msg_final","role":"assistant","model":"claude-opus-4-7","usage":{"input_tokens":2}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"same process done"}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			))
			return err
		},
	}
	proxy := &ClaudeCLIProxy{
		runner:       runner,
		tempDir:      t.TempDir(),
		now:          func() time.Time { return time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC) },
		newSessionID: func() string { return "11111111-1111-4111-8111-111111111111" },
	}
	account := &Account{
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Extra:       map[string]any{"claude_cli_proxy_enabled": true, "claude_cli_command": "fake-claude"},
	}

	firstBody := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"lookup","description":"Lookup a thing","input_schema":{"type":"object"}}]}`)
	firstParsed, err := ParseGatewayRequest(firstBody, PlatformAnthropic)
	require.NoError(t, err)
	firstRec := httptest.NewRecorder()
	firstCtx, _ := gin.CreateTestContext(firstRec)
	firstCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	firstDone := make(chan error, 1)
	go func() {
		_, err := proxy.Forward(context.Background(), firstCtx, account, firstParsed, time.Now())
		firstDone <- err
	}()
	waitForChannel(t, toolCallStarted, "mcp tool call")
	select {
	case err := <-firstDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("first Forward did not return tool_use while MCP call was pending")
	}

	var firstMessage map[string]any
	require.NoError(t, json.Unmarshal(firstRec.Body.Bytes(), &firstMessage))
	firstToolUse := firstMessage["content"].([]any)[0].(map[string]any)
	firstToolUseID := firstToolUse["id"].(string)

	secondBody := []byte(fmt.Sprintf(`{"model":"claude-opus-4-7","messages":[{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"lookup","input":{"q":"one"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"lookup result"}]}],"tools":[{"name":"lookup","description":"Lookup a thing","input_schema":{"type":"object"}}]}`, firstToolUseID, firstToolUseID))
	secondParsed, err := ParseGatewayRequest(secondBody, PlatformAnthropic)
	require.NoError(t, err)
	secondRec := httptest.NewRecorder()
	secondCtx, _ := gin.CreateTestContext(secondRec)
	secondCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	result, err := proxy.Forward(context.Background(), secondCtx, account, secondParsed, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1, runCount)

	var secondMessage map[string]any
	require.NoError(t, json.Unmarshal(secondRec.Body.Bytes(), &secondMessage))
	require.Equal(t, "end_turn", secondMessage["stop_reason"])
	content := secondMessage["content"].([]any)
	require.Len(t, content, 1)
	text := content[0].(map[string]any)
	require.Equal(t, "same process done", text["text"])
}

func TestClaudeCLIProxyForwardReturnsNextToolCallAfterToolResult(t *testing.T) {
	gin.SetMode(gin.TestMode)

	firstToolStarted := make(chan struct{})
	secondToolStarted := make(chan struct{})
	var runCount int
	runner := &fakeClaudeCLIProcessRunner{
		runWithContext: func(ctx context.Context, req claudeCLIProcessRequest) error {
			runCount++
			require.Equal(t, 1, runCount)
			_, err := io.WriteString(req.Stdout, claudeCLIStreamJSON(
				`{"type":"message_start","message":{"id":"msg_tool_1","role":"assistant","model":"claude-opus-4-7","usage":{"input_tokens":2}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_cli_1","name":"lookup","input":{}}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"one\"}"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			))
			require.NoError(t, err)
			firstResultCh := make(chan string, 1)
			go func() {
				firstResultCh <- callMCPToolFromRequest(t, req, "lookup", map[string]any{"q": "one"}, firstToolStarted)
			}()
			select {
			case <-firstResultCh:
			case <-ctx.Done():
				return ctx.Err()
			}
			_, err = io.WriteString(req.Stdout, claudeCLIStreamJSON(
				`{"type":"message_start","message":{"id":"msg_tool_2","role":"assistant","model":"claude-opus-4-7","usage":{"input_tokens":2}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_cli_2","name":"fetch","input":{}}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"url\":\"https://example.com\"}"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			))
			require.NoError(t, err)
			secondResultCh := make(chan string, 1)
			go func() {
				secondResultCh <- callMCPToolFromRequest(t, req, "fetch", map[string]any{"url": "https://example.com"}, secondToolStarted)
			}()
			select {
			case <-secondResultCh:
			case <-ctx.Done():
				return ctx.Err()
			}
			_, err = io.WriteString(req.Stdout, claudeCLIStreamJSON(
				`{"type":"message_start","message":{"id":"msg_final","role":"assistant","model":"claude-opus-4-7","usage":{"input_tokens":2}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"chain done"}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			))
			return err
		},
	}
	proxy := &ClaudeCLIProxy{
		runner:       runner,
		tempDir:      t.TempDir(),
		now:          func() time.Time { return time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC) },
		newSessionID: func() string { return "11111111-1111-4111-8111-111111111111" },
	}
	account := &Account{
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Extra:       map[string]any{"claude_cli_proxy_enabled": true, "claude_cli_command": "fake-claude"},
	}

	firstBody := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"lookup","description":"Lookup a thing","input_schema":{"type":"object"}},{"name":"fetch","description":"Fetch a thing","input_schema":{"type":"object"}}]}`)
	firstParsed, err := ParseGatewayRequest(firstBody, PlatformAnthropic)
	require.NoError(t, err)
	firstRec := httptest.NewRecorder()
	firstCtx, _ := gin.CreateTestContext(firstRec)
	firstCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	firstDone := make(chan error, 1)
	go func() {
		_, err := proxy.Forward(context.Background(), firstCtx, account, firstParsed, time.Now())
		firstDone <- err
	}()
	waitForChannel(t, firstToolStarted, "first mcp tool call")
	select {
	case err := <-firstDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("first Forward did not return first tool_use")
	}
	var firstMessage map[string]any
	require.NoError(t, json.Unmarshal(firstRec.Body.Bytes(), &firstMessage))
	firstToolUse := firstMessage["content"].([]any)[0].(map[string]any)
	firstToolUseID := firstToolUse["id"].(string)

	secondBody := []byte(fmt.Sprintf(`{"model":"claude-opus-4-7","messages":[{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"lookup","input":{"q":"one"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"lookup result"}]}],"tools":[{"name":"lookup","description":"Lookup a thing","input_schema":{"type":"object"}},{"name":"fetch","description":"Fetch a thing","input_schema":{"type":"object"}}]}`, firstToolUseID, firstToolUseID))
	secondParsed, err := ParseGatewayRequest(secondBody, PlatformAnthropic)
	require.NoError(t, err)
	secondRec := httptest.NewRecorder()
	secondCtx, _ := gin.CreateTestContext(secondRec)
	secondCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	secondDone := make(chan error, 1)
	go func() {
		_, err := proxy.Forward(context.Background(), secondCtx, account, secondParsed, time.Now())
		secondDone <- err
	}()
	waitForChannel(t, secondToolStarted, "second mcp tool call")
	select {
	case err := <-secondDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("second Forward did not return next tool_use")
	}
	var secondMessage map[string]any
	require.NoError(t, json.Unmarshal(secondRec.Body.Bytes(), &secondMessage))
	secondToolUse := secondMessage["content"].([]any)[0].(map[string]any)
	require.Equal(t, "fetch", secondToolUse["name"])
	secondToolUseID := secondToolUse["id"].(string)

	thirdBody := []byte(fmt.Sprintf(`{"model":"claude-opus-4-7","messages":[{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"fetch","input":{"url":"https://example.com"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"fetch result"}]}],"tools":[{"name":"lookup","description":"Lookup a thing","input_schema":{"type":"object"}},{"name":"fetch","description":"Fetch a thing","input_schema":{"type":"object"}}]}`, secondToolUseID, secondToolUseID))
	thirdParsed, err := ParseGatewayRequest(thirdBody, PlatformAnthropic)
	require.NoError(t, err)
	thirdRec := httptest.NewRecorder()
	thirdCtx, _ := gin.CreateTestContext(thirdRec)
	thirdCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	result, err := proxy.Forward(context.Background(), thirdCtx, account, thirdParsed, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	var thirdMessage map[string]any
	require.NoError(t, json.Unmarshal(thirdRec.Body.Bytes(), &thirdMessage))
	text := thirdMessage["content"].([]any)[0].(map[string]any)
	require.Equal(t, "chain done", text["text"])
}

func TestClaudeCLIProxyForwardWritesLatestToolResultToSessionAndContinuesWithContinuePrompt(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var sessionLines []string
	runner := &fakeClaudeCLIProcessRunner{
		run: func(req claudeCLIProcessRequest) error {
			realDir, err := filepath.EvalSymlinks(req.Dir)
			if err != nil {
				realDir = req.Dir
			}
			sessionPath := filepath.Join(req.Dir, ".claude", "projects", sanitizeClaudeCLIProjectPath(realDir), "11111111-1111-4111-8111-111111111111.jsonl")
			data, err := os.ReadFile(sessionPath)
			require.NoError(t, err)
			sessionLines = splitNonEmptyJSONLLines(string(data))
			_, err = io.WriteString(req.Stdout, claudeCLIStreamJSON(
				`{"type":"message_start","message":{"id":"msg_final","role":"assistant","model":"claude-opus-4-7","usage":{"input_tokens":2}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"continued"}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			))
			return err
		},
	}
	proxy := &ClaudeCLIProxy{
		runner:       runner,
		tempDir:      t.TempDir(),
		now:          func() time.Time { return time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC) },
		newSessionID: func() string { return "11111111-1111-4111-8111-111111111111" },
	}
	account := &Account{
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Extra:       map[string]any{"claude_cli_proxy_enabled": true, "claude_cli_command": "fake-claude"},
	}

	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_expired","name":"lookup","input":{"q":"hello"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_expired","content":"late client result"}]}]}`)
	parsed, err := ParseGatewayRequest(body, PlatformAnthropic)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	result, err := proxy.Forward(context.Background(), c, account, parsed, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)

	requireArgFollowedBy(t, runner.req.Args, "--resume", "11111111-1111-4111-8111-111111111111")
	require.NotContains(t, string(runner.stdin), `"type":"tool_result"`)
	require.Contains(t, string(runner.stdin), `"text":"Continue from where you left off."`)
	require.Len(t, sessionLines, 2)
	assistantLine := decodeJSONLMap(t, sessionLines[0])
	require.Equal(t, "assistant", assistantLine["type"])
	userLine := decodeJSONLMap(t, sessionLines[1])
	require.Equal(t, "user", userLine["type"])
	userMessage := userLine["message"].(map[string]any)
	userContent := userMessage["content"].([]any)
	toolResult := userContent[0].(map[string]any)
	require.Equal(t, "tool_result", toolResult["type"])
	require.Equal(t, "toolu_expired", toolResult["tool_use_id"])

	var response map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	content := response["content"].([]any)
	text := content[0].(map[string]any)
	require.Equal(t, "continued", text["text"])
}

func TestClaudeCLIPendingToolCallTTLUsesAccountIdleTimeout(t *testing.T) {
	require.Equal(t, 5*time.Minute, claudeCLIPendingToolCallTTL(&Account{}))
	require.Equal(t, 7*time.Minute, claudeCLIPendingToolCallTTL(&Account{
		Extra: map[string]any{"session_idle_timeout_minutes": 7},
	}))
	require.Equal(t, 5*time.Minute, claudeCLIPendingToolCallTTL(&Account{
		Extra: map[string]any{"session_idle_timeout_minutes": 0},
	}))
}

func TestClaudeCLIPendingToolRunHoldTTLIsCappedBelowMCPHTTPTimeout(t *testing.T) {
	require.Equal(t, 55*time.Second, claudeCLIPendingToolRunHoldTTL(&Account{}))
	require.Equal(t, 55*time.Second, claudeCLIPendingToolRunHoldTTL(&Account{
		Extra: map[string]any{"session_idle_timeout_minutes": 7},
	}))
	require.Equal(t, 55*time.Second, claudeCLIPendingToolRunHoldTTL(&Account{
		Extra: map[string]any{"session_idle_timeout_minutes": 1},
	}))
}

func TestClaudeCLIProxyDoesNotAddDotWhenRawLatestMessageIsNotToolResult(t *testing.T) {
	gin.SetMode(gin.TestMode)

	runner := &fakeClaudeCLIProcessRunner{
		run: func(req claudeCLIProcessRequest) error {
			_, err := io.WriteString(req.Stdout, claudeCLIStreamJSON(
				`{"type":"message_start","message":{"id":"msg_final","role":"assistant","model":"claude-opus-4-7","usage":{"input_tokens":2}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			))
			return err
		},
	}
	proxy := &ClaudeCLIProxy{
		runner:       runner,
		tempDir:      t.TempDir(),
		now:          func() time.Time { return time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC) },
		newSessionID: func() string { return "11111111-1111-4111-8111-111111111111" },
	}
	account := &Account{
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Extra:       map[string]any{"claude_cli_proxy_enabled": true, "claude_cli_command": "fake-claude"},
	}

	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_done","name":"lookup","input":{"q":"hello"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_done","content":"late client result"}]},{"role":"user","content":""}]}`)
	parsed, err := ParseGatewayRequest(body, PlatformAnthropic)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	result, err := proxy.Forward(context.Background(), c, account, parsed, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotContains(t, string(runner.stdin), `"text":"."`)
	require.NotContains(t, string(runner.stdin), `"type":"tool_result"`)
	require.Contains(t, string(runner.stdin), `"text":"Continue from where you left off."`)
}

func TestClaudeCLIProxyForwardReturnsStructuredStdoutWhenProcessExitsNonZero(t *testing.T) {
	gin.SetMode(gin.TestMode)

	runner := &fakeClaudeCLIProcessRunner{
		run: func(req claudeCLIProcessRequest) error {
			_, err := io.WriteString(req.Stdout, strings.Join([]string{
				`{"type":"assistant","message":{"id":"msg_error","type":"message","role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"model unavailable"}],"stop_reason":"stop_sequence","stop_sequence":"","usage":{"input_tokens":0,"output_tokens":0}},"parent_tool_use_id":null,"session_id":"sess_1","uuid":"00000000-0000-4000-8000-000000000001","error":"invalid_request"}`,
				`{"type":"result","subtype":"success","duration_ms":1,"duration_api_ms":0,"is_error":true,"num_turns":1,"result":"model unavailable","stop_reason":"stop_sequence","total_cost_usd":0,"usage":{"input_tokens":0,"output_tokens":0},"modelUsage":{},"permission_denials":[],"session_id":"sess_1","uuid":"00000000-0000-4000-8000-000000000002"}`,
			}, "\n"))
			require.NoError(t, err)
			return errors.New("exit status 1")
		},
	}
	proxy := &ClaudeCLIProxy{
		runner:       runner,
		tempDir:      t.TempDir(),
		now:          func() time.Time { return time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC) },
		newSessionID: func() string { return "11111111-1111-4111-8111-111111111111" },
		mcpServer:    newClaudeCLIHTTPMCPServer(time.Now),
	}
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`)
	parsed, err := ParseGatewayRequest(body, PlatformAnthropic)
	require.NoError(t, err)
	account := &Account{
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "token"},
		Extra:       map[string]any{"claude_cli_proxy_enabled": true, "claude_cli_command": "fake-claude"},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	result, err := proxy.Forward(context.Background(), c, account, parsed, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "model unavailable")
}

func TestClaudeCLIProxyForwardStreamingReturnsStructuredStdoutWhenProcessExitsNonZero(t *testing.T) {
	gin.SetMode(gin.TestMode)

	runner := &fakeClaudeCLIProcessRunner{
		run: func(req claudeCLIProcessRequest) error {
			_, err := io.WriteString(req.Stdout, `{"type":"assistant","message":{"id":"msg_error","type":"message","role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"model unavailable"}],"stop_reason":"stop_sequence","stop_sequence":"","usage":{"input_tokens":0,"output_tokens":0}},"parent_tool_use_id":null,"session_id":"sess_1","uuid":"00000000-0000-4000-8000-000000000001","error":"invalid_request"}`+"\n")
			require.NoError(t, err)
			return errors.New("exit status 1")
		},
	}
	proxy := &ClaudeCLIProxy{
		runner:       runner,
		tempDir:      t.TempDir(),
		now:          func() time.Time { return time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC) },
		newSessionID: func() string { return "11111111-1111-4111-8111-111111111111" },
		mcpServer:    newClaudeCLIHTTPMCPServer(time.Now),
	}
	body := []byte(`{"model":"claude-opus-4-7","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	parsed, err := ParseGatewayRequest(body, PlatformAnthropic)
	require.NoError(t, err)
	account := &Account{
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "token"},
		Extra:       map[string]any{"claude_cli_proxy_enabled": true, "claude_cli_command": "fake-claude"},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	result, err := proxy.Forward(context.Background(), c, account, parsed, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "event: message_start")
	require.Contains(t, rec.Body.String(), "model unavailable")
}

func TestClaudeCLIProxyForwardStreamingStreamsBeforeRunnerExit(t *testing.T) {
	gin.SetMode(gin.TestMode)

	firstWrite := make(chan struct{})
	releaseRunner := make(chan struct{})
	runner := &fakeClaudeCLIProcessRunner{
		run: func(req claudeCLIProcessRequest) error {
			_, err := io.WriteString(req.Stdout, claudeCLIStreamJSON(
				`{"type":"message_start","message":{"id":"msg_1","role":"assistant","model":"claude-opus-4-7","usage":{"input_tokens":1}}}`,
			))
			if err != nil {
				return err
			}
			close(firstWrite)
			<-releaseRunner
			_, err = io.WriteString(req.Stdout, claudeCLIStreamJSON(
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			))
			return err
		},
	}
	proxy := &ClaudeCLIProxy{
		runner:       runner,
		tempDir:      t.TempDir(),
		now:          func() time.Time { return time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC) },
		newSessionID: func() string { return "11111111-1111-4111-8111-111111111111" },
	}

	body := []byte(`{"model":"claude-opus-4-7","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	parsed, err := ParseGatewayRequest(body, PlatformAnthropic)
	require.NoError(t, err)
	upstreamAccepted := make(chan struct{})
	parsed.OnUpstreamAccepted = func() {
		close(upstreamAccepted)
	}
	account := &Account{
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Extra:       map[string]any{"claude_cli_proxy_enabled": true, "claude_cli_command": "fake-claude"},
	}
	rec := newLockedResponseRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	forwardErr := make(chan error, 1)
	go func() {
		_, err := proxy.Forward(context.Background(), c, account, parsed, time.Now())
		forwardErr <- err
	}()

	waitForChannel(t, firstWrite, "runner first stdout write")
	waitForRecorderBodyContains(t, rec, "event: message_start")
	waitForChannel(t, upstreamAccepted, "upstream accepted callback")

	close(releaseRunner)
	select {
	case err := <-forwardErr:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Forward did not return after runner release")
	}
}

func TestClaudeCLIProxyForwardRejectsDisabledAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	called := 0
	runner := &fakeClaudeCLIProcessRunner{
		run: func(req claudeCLIProcessRequest) error {
			called++
			return nil
		},
	}
	proxy := &ClaudeCLIProxy{
		runner:       runner,
		tempDir:      t.TempDir(),
		now:          func() time.Time { return time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC) },
		newSessionID: func() string { return "11111111-1111-4111-8111-111111111111" },
	}

	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`)
	parsed, err := ParseGatewayRequest(body, PlatformAnthropic)
	require.NoError(t, err)
	account := &Account{
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	result, err := proxy.Forward(context.Background(), c, account, parsed, time.Now())
	require.ErrorContains(t, err, "account is not enabled")
	require.Nil(t, result)
	require.Zero(t, called)
}

func claudeCLIStreamJSON(events ...string) string {
	out := ""
	for _, event := range events {
		encoded, _ := json.Marshal(json.RawMessage(event))
		out += `{"type":"stream_event","event":` + string(encoded) + "}\n"
	}
	return out
}

func requireArgFollowedBy(t *testing.T, args []string, key string, expected string) string {
	t.Helper()
	for i, arg := range args {
		if arg != key || i+1 >= len(args) {
			continue
		}
		if expected != "" {
			require.Equal(t, expected, args[i+1])
		}
		return args[i+1]
	}
	require.Failf(t, "arg not found", "%s in %#v", key, args)
	return ""
}

func requireArgPresentAfter(t *testing.T, args []string, key string, after string) {
	t.Helper()
	afterIndex := -1
	for i, arg := range args {
		if arg == after {
			afterIndex = i
			break
		}
	}
	require.NotEqual(t, -1, afterIndex, "%s not found in %#v", after, args)
	for i := afterIndex + 1; i < len(args); i++ {
		if args[i] == key {
			return
		}
	}
	require.Failf(t, "arg not found after marker", "%s after %s in %#v", key, after, args)
}

func readMCPHTTPToolsFromRequest(t *testing.T, req claudeCLIProcessRequest) []claudeCLITool {
	t.Helper()
	mcpConfigPath := requireArgFollowedBy(t, req.Args, "--mcp-config", "")
	rawConfig, err := os.ReadFile(mcpConfigPath)
	require.NoError(t, err)

	var cfg claudeCLIMCPConfig
	require.NoError(t, json.Unmarshal(rawConfig, &cfg))
	server, ok := cfg.MCPServers["tools"]
	require.True(t, ok)
	require.Equal(t, "http", server.Type)
	require.NotEmpty(t, server.URL)

	requestBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	httpReq, err := http.NewRequest(http.MethodPost, server.URL, bytes.NewReader(requestBody))
	require.NoError(t, err)
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	httpReq.Header.Set("Content-Type", "application/json")
	for key, value := range server.Headers {
		httpReq.Header.Set(key, value)
	}

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(httpReq)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var rpcResp struct {
		Result struct {
			Tools []struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&rpcResp))
	tools := make([]claudeCLITool, 0, len(rpcResp.Result.Tools))
	for _, tool := range rpcResp.Result.Tools {
		tools = append(tools, claudeCLITool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.InputSchema,
		})
	}
	return tools
}

func requireClaudeCLIToolsByName(t *testing.T, tools []claudeCLITool, expected map[string]claudeCLITool) {
	t.Helper()
	require.Len(t, tools, len(expected))
	byName := make(map[string]claudeCLITool, len(tools))
	for _, tool := range tools {
		byName[tool.Name] = tool
	}
	require.Equal(t, expected, byName)
}

func callMCPToolFromRequest(t *testing.T, req claudeCLIProcessRequest, name string, input map[string]any, started chan<- struct{}) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      name,
			"arguments": input,
		},
	})
	require.NoError(t, err)
	return callMCPToolFromRequestWithBody(t, req, payload, started)
}

func callMCPToolFromRequestWithParams(t *testing.T, req claudeCLIProcessRequest, params string, started chan<- struct{}) string {
	t.Helper()
	return callMCPToolFromRequestWithBody(t, req, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+params+`}`), started)
}

func callMCPToolFromRequestWithBody(t *testing.T, req claudeCLIProcessRequest, payload []byte, started chan<- struct{}) string {
	t.Helper()
	mcpConfigPath := requireArgFollowedBy(t, req.Args, "--mcp-config", "")
	rawConfig, err := os.ReadFile(mcpConfigPath)
	require.NoError(t, err)

	var cfg claudeCLIMCPConfig
	require.NoError(t, json.Unmarshal(rawConfig, &cfg))
	server, ok := cfg.MCPServers["tools"]
	require.True(t, ok)

	httpReq, err := http.NewRequest(http.MethodPost, server.URL, bytes.NewReader(payload))
	require.NoError(t, err)
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	httpReq.Header.Set("Content-Type", "application/json")
	for key, value := range server.Headers {
		httpReq.Header.Set(key, value)
	}
	if started != nil {
		close(started)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(httpReq)
	require.NoError(t, err)
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", respBody)

	var rpcResp struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(respBody, &rpcResp), "body: %s", respBody)
	require.Len(t, rpcResp.Result.Content, 1, "body: %s", respBody)
	return rpcResp.Result.Content[0].Text
}

func waitForChannel(t *testing.T, ch <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func TestFormatClaudeCLIRunErrorIncludesStdoutWhenStderrEmpty(t *testing.T) {
	err := formatClaudeCLIRunError(errors.New("exit status 1"), "", `{"type":"result","subtype":"error_during_execution","errors":["bad input"]}`)

	require.ErrorContains(t, err, "claude cli proxy: run: exit status 1")
	require.ErrorContains(t, err, "stdout=")
	require.ErrorContains(t, err, "bad input")
}

type lockedResponseRecorder struct {
	mu sync.Mutex
	*httptest.ResponseRecorder
}

func newLockedResponseRecorder() *lockedResponseRecorder {
	return &lockedResponseRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (r *lockedResponseRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ResponseRecorder.Write(p)
}

func (r *lockedResponseRecorder) WriteString(s string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ResponseRecorder.WriteString(s)
}

func (r *lockedResponseRecorder) WriteHeader(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ResponseRecorder.WriteHeader(code)
}

func (r *lockedResponseRecorder) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ResponseRecorder.Flush()
}

func (r *lockedResponseRecorder) bodyString() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Body.String()
}

func waitForRecorderBodyContains(t *testing.T, rec *lockedResponseRecorder, want string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		body := rec.bodyString()
		if strings.Contains(body, want) {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for response body to contain %q; body: %q", want, body)
		case <-ticker.C:
		}
	}
}
