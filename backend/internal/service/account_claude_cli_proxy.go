package service

import (
	"errors"
	"fmt"
	"strings"
)

func (a *Account) IsClaudeCLIProxyEnabled() bool {
	if a == nil || a.Platform != PlatformAnthropic || (a.Type != AccountTypeOAuth && a.Type != AccountTypeSetupToken && a.Type != AccountTypeAPIKey) || a.Extra == nil {
		return false
	}
	enabled, ok := a.Extra["claude_cli_proxy_enabled"].(bool)
	return ok && enabled
}

func (a *Account) GetClaudeCLIAuth() (claudeCLIProcessAuth, error) {
	if a == nil {
		return claudeCLIProcessAuth{}, errors.New("claude cli proxy: account is nil")
	}
	switch a.Type {
	case AccountTypeOAuth, AccountTypeSetupToken:
		token := strings.TrimSpace(a.GetCredential("access_token"))
		if token == "" {
			return claudeCLIProcessAuth{}, errors.New("claude cli proxy: missing access token")
		}
		baseURL := ""
		if a.IsCustomBaseURLEnabled() {
			baseURL = strings.TrimSpace(a.GetCustomBaseURL())
		}
		return claudeCLIProcessAuth{OAuthToken: token, BaseURL: baseURL}, nil
	case AccountTypeAPIKey:
		apiKey := strings.TrimSpace(a.GetCredential("api_key"))
		if apiKey == "" {
			return claudeCLIProcessAuth{}, errors.New("claude cli proxy: missing api key")
		}
		return claudeCLIProcessAuth{
			AuthToken: apiKey,
			BaseURL:   strings.TrimSpace(a.GetCredential("base_url")),
		}, nil
	default:
		return claudeCLIProcessAuth{}, fmt.Errorf("claude cli proxy: unsupported account type %q", a.Type)
	}
}

func (a *Account) GetClaudeCLICommand() string {
	if a == nil || a.Extra == nil {
		return "claude"
	}
	command := strings.TrimSpace(a.GetExtraString("claude_cli_command"))
	if command == "" {
		return "claude"
	}
	return command
}

func (a *Account) GetClaudeCLIWebToolsForwardGroupID() *int64 {
	if a == nil || a.Extra == nil {
		return nil
	}
	raw, ok := a.Extra["claude_cli_web_tools_forward_group_id"]
	if !ok {
		return nil
	}
	groupID := int64(parseExtraInt(raw))
	if groupID <= 0 {
		return nil
	}
	return &groupID
}
