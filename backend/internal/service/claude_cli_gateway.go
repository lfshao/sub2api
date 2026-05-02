package service

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

type claudeCLIForwarder interface {
	Forward(ctx context.Context, c *gin.Context, account *Account, parsed *ParsedRequest, startTime time.Time) (*ForwardResult, error)
}

func (s *GatewayService) forwardClaudeCLIWebToolsRequest(ctx context.Context, c *gin.Context, account *Account, parsed *ParsedRequest, startTime time.Time) (*ForwardResult, error) {
	target := account
	if groupID := account.GetClaudeCLIWebToolsForwardGroupID(); groupID != nil {
		selected, err := s.SelectAccountForModelWithExclusions(ctx, groupID, "", parsed.Model, nil)
		if err != nil {
			return nil, fmt.Errorf("claude cli web tools forward: select group %d: %w", *groupID, err)
		}
		target = selected
	}
	if target == nil || target.Platform != PlatformAnthropic || target.Type != AccountTypeAPIKey {
		return s.rejectClaudeCLIWebToolsForward(c, target)
	}

	body := parsed.Body
	requestModel := parsed.Model
	if requestModel != "" {
		if mappedModel := target.GetMappedModel(requestModel); mappedModel != requestModel {
			body = s.replaceModelInBody(body, mappedModel)
			requestModel = mappedModel
		}
	}
	return s.forwardAnthropicAPIKeyPassthroughWithInput(ctx, c, target, anthropicPassthroughForwardInput{
		Body:          body,
		RequestModel:  requestModel,
		OriginalModel: parsed.Model,
		RequestStream: parsed.Stream,
		StartTime:     startTime,
	})
}

func (s *GatewayService) rejectClaudeCLIWebToolsForward(c *gin.Context, account *Account) (*ForwardResult, error) {
	accountType := ""
	if account != nil {
		accountType = string(account.Type)
	}
	err := fmt.Errorf("claude cli web tools forward: account type %q is not supported", accountType)
	if c != nil && !c.Writer.Written() {
		c.JSON(http.StatusBadRequest, gin.H{
			"type": "error",
			"error": gin.H{
				"type":    "invalid_request_error",
				"message": "Claude CLI web tools forwarding is not supported for this account type",
			},
		})
	}
	return nil, err
}
