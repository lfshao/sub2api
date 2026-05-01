package service

import (
	"strings"
)

const claudeCLIUserIDExtraKey = "claude_cli_userID"

func (a *Account) GetClaudeCLIUserID() string {
	if a == nil {
		return ""
	}
	userID := strings.TrimSpace(a.GetExtraString(claudeCLIUserIDExtraKey))
	if !isStandardClaudeCLIUserID(userID) {
		return ""
	}
	return userID
}

func isStandardClaudeCLIUserID(userID string) bool {
	if len(userID) != 64 {
		return false
	}
	for _, ch := range userID {
		if (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') {
			continue
		}
		return false
	}
	return true
}
