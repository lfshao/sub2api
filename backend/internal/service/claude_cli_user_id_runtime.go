package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

type claudeCLIGlobalConfigFile struct {
	UserID string `json:"userID"`
}

func readClaudeCLIWorkspaceUserIDForResult(ws *claudeCLIWorkspace) string {
	if ws == nil || ws.Dir == "" {
		return ""
	}
	userID, err := readClaudeCLIWorkspaceUserID(ws.Dir)
	if err != nil {
		logClaudeCLIMCPDebug("read workspace userID failed dir=%s err=%v", ws.Dir, err)
		return ""
	}
	return userID
}

func readClaudeCLIWorkspaceUserID(workspaceDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(workspaceDir, ".claude", ".claude.json"))
	if err != nil {
		return "", err
	}
	var config claudeCLIGlobalConfigFile
	if err := json.Unmarshal(data, &config); err != nil {
		return "", err
	}
	if !isStandardClaudeCLIUserID(config.UserID) {
		return "", nil
	}
	return config.UserID, nil
}

func (s *GatewayService) persistClaudeCLIUserIDFromResult(ctx context.Context, account *Account, result *ForwardResult) {
	if s == nil || account == nil || result == nil || result.ClaudeCLIUserID == "" {
		return
	}
	userID := result.ClaudeCLIUserID
	if !isStandardClaudeCLIUserID(userID) || account.GetClaudeCLIUserID() == userID {
		return
	}
	if account.Extra == nil {
		account.Extra = make(map[string]any)
	}
	account.Extra[claudeCLIUserIDExtraKey] = userID
	if s.accountRepo == nil || account.ID == 0 {
		return
	}
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{claudeCLIUserIDExtraKey: userID}); err != nil {
		logger.LegacyPrintf("service.claude_cli", "persist claude cli userID failed account_id=%d err=%v", account.ID, err)
	}
}
