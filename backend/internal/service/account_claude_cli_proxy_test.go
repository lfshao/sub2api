package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccount_IsClaudeCLIProxyEnabled(t *testing.T) {
	tests := []struct {
		name    string
		account *Account
		want    bool
	}{
		{
			name: "Anthropic OAuth enabled",
			account: &Account{
				Platform: PlatformAnthropic,
				Type:     AccountTypeOAuth,
				Extra:    map[string]any{"claude_cli_proxy_enabled": true},
			},
			want: true,
		},
		{
			name: "Anthropic SetupToken enabled",
			account: &Account{
				Platform: PlatformAnthropic,
				Type:     AccountTypeSetupToken,
				Extra:    map[string]any{"claude_cli_proxy_enabled": true},
			},
			want: true,
		},
		{
			name: "Anthropic APIKey enabled",
			account: &Account{
				Platform: PlatformAnthropic,
				Type:     AccountTypeAPIKey,
				Extra:    map[string]any{"claude_cli_proxy_enabled": true},
			},
			want: true,
		},
		{
			name: "OpenAI OAuth ignored",
			account: &Account{
				Platform: PlatformOpenAI,
				Type:     AccountTypeOAuth,
				Extra:    map[string]any{"claude_cli_proxy_enabled": true},
			},
			want: false,
		},
		{
			name: "missing flag disabled",
			account: &Account{
				Platform: PlatformAnthropic,
				Type:     AccountTypeOAuth,
				Extra:    map[string]any{},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.account.IsClaudeCLIProxyEnabled())
		})
	}
}

func TestAccount_GetClaudeCLIAuth(t *testing.T) {
	t.Run("OAuth uses access token", func(t *testing.T) {
		account := &Account{
			Platform:    PlatformAnthropic,
			Type:        AccountTypeOAuth,
			Credentials: map[string]any{"access_token": "oauth-token"},
		}
		auth, err := account.GetClaudeCLIAuth()
		require.NoError(t, err)
		require.Equal(t, claudeCLIProcessAuth{OAuthToken: "oauth-token"}, auth)
	})

	t.Run("OAuth uses custom base url when enabled", func(t *testing.T) {
		account := &Account{
			Platform:    PlatformAnthropic,
			Type:        AccountTypeOAuth,
			Credentials: map[string]any{"access_token": "oauth-token"},
			Extra: map[string]any{
				"custom_base_url_enabled": true,
				"custom_base_url":         " https://relay.example.com ",
			},
		}
		auth, err := account.GetClaudeCLIAuth()
		require.NoError(t, err)
		require.Equal(t, claudeCLIProcessAuth{
			OAuthToken: "oauth-token",
			BaseURL:    "https://relay.example.com",
		}, auth)
	})

	t.Run("SetupToken uses custom base url when enabled", func(t *testing.T) {
		account := &Account{
			Platform:    PlatformAnthropic,
			Type:        AccountTypeSetupToken,
			Credentials: map[string]any{"access_token": "setup-token"},
			Extra: map[string]any{
				"custom_base_url_enabled": true,
				"custom_base_url":         "https://relay.example.com",
			},
		}
		auth, err := account.GetClaudeCLIAuth()
		require.NoError(t, err)
		require.Equal(t, claudeCLIProcessAuth{
			OAuthToken: "setup-token",
			BaseURL:    "https://relay.example.com",
		}, auth)
	})

	t.Run("OAuth custom base url falls back to default when enabled without value", func(t *testing.T) {
		account := &Account{
			Platform:    PlatformAnthropic,
			Type:        AccountTypeOAuth,
			Credentials: map[string]any{"access_token": "oauth-token"},
			Extra:       map[string]any{"custom_base_url_enabled": true},
		}
		auth, err := account.GetClaudeCLIAuth()
		require.NoError(t, err)
		require.Equal(t, claudeCLIProcessAuth{OAuthToken: "oauth-token"}, auth)
	})

	t.Run("API key uses auth token env and base url", func(t *testing.T) {
		account := &Account{
			Platform: PlatformAnthropic,
			Type:     AccountTypeAPIKey,
			Credentials: map[string]any{
				"api_key":  "sk-ant-test",
				"base_url": "https://api.anthropic.com",
			},
		}
		auth, err := account.GetClaudeCLIAuth()
		require.NoError(t, err)
		require.Equal(t, claudeCLIProcessAuth{
			AuthToken: "sk-ant-test",
			BaseURL:   "https://api.anthropic.com",
		}, auth)
	})

	t.Run("API key requires credential", func(t *testing.T) {
		account := &Account{Platform: PlatformAnthropic, Type: AccountTypeAPIKey}
		auth, err := account.GetClaudeCLIAuth()
		require.ErrorContains(t, err, "missing api key")
		require.Equal(t, claudeCLIProcessAuth{}, auth)
	})
}

func TestAccount_GetClaudeCLICommand(t *testing.T) {
	t.Run("default empty extra", func(t *testing.T) {
		account := &Account{Extra: map[string]any{}}
		require.Equal(t, "claude", account.GetClaudeCLICommand())
	})

	t.Run("trims configured command", func(t *testing.T) {
		account := &Account{
			Extra: map[string]any{"claude_cli_command": " /opt/bin/claude "},
		}
		require.Equal(t, "/opt/bin/claude", account.GetClaudeCLICommand())
	})
}

func TestAccount_GetClaudeCLIUserID(t *testing.T) {
	t.Run("reads standard userID", func(t *testing.T) {
		account := &Account{
			Extra: map[string]any{
				"claude_cli_userID": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			},
		}
		require.Equal(t, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", account.GetClaudeCLIUserID())
	})

	t.Run("rejects non-standard userID", func(t *testing.T) {
		account := &Account{
			Extra: map[string]any{"claude_cli_userID": "not-standard"},
		}
		require.Empty(t, account.GetClaudeCLIUserID())
	})
}
