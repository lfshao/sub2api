package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadClaudeCLIWorkspaceUserID(t *testing.T) {
	const userID = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".claude"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".claude", ".claude.json"), []byte(`{"userID":"`+userID+`"}`), 0600))

	got, err := readClaudeCLIWorkspaceUserID(dir)
	require.NoError(t, err)
	require.Equal(t, userID, got)
}

func TestPersistClaudeCLIUserIDFromResultPersistsGeneratedUserID(t *testing.T) {
	const userID = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	account := &Account{
		ID:       123,
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{"claude_cli_proxy_enabled": true},
	}
	repo := &sessionWindowMockRepo{}
	svc := &GatewayService{accountRepo: repo}

	svc.persistClaudeCLIUserIDFromResult(context.Background(), account, &ForwardResult{ClaudeCLIUserID: userID})

	require.Equal(t, userID, account.Extra["claude_cli_userID"])
	require.Len(t, repo.updateExtraCalls, 1)
	require.Equal(t, int64(123), repo.updateExtraCalls[0].ID)
	require.Equal(t, map[string]any{"claude_cli_userID": userID}, repo.updateExtraCalls[0].Updates)
}

func TestPersistClaudeCLIUserIDFromResultKeepsExistingUserID(t *testing.T) {
	const userID = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	account := &Account{
		ID:       123,
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"claude_cli_proxy_enabled": true,
			"claude_cli_userID":        userID,
		},
	}
	repo := &sessionWindowMockRepo{}
	svc := &GatewayService{accountRepo: repo}

	svc.persistClaudeCLIUserIDFromResult(context.Background(), account, &ForwardResult{ClaudeCLIUserID: userID})

	require.Empty(t, repo.updateExtraCalls)
}

func TestPersistClaudeCLIUserIDFromResultIgnoresInvalidUserID(t *testing.T) {
	account := &Account{
		ID:       123,
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{"claude_cli_proxy_enabled": true},
	}
	repo := &sessionWindowMockRepo{}
	svc := &GatewayService{accountRepo: repo}

	svc.persistClaudeCLIUserIDFromResult(context.Background(), account, &ForwardResult{ClaudeCLIUserID: "short"})

	require.Empty(t, account.GetClaudeCLIUserID())
	require.Empty(t, repo.updateExtraCalls)
}
