package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDetectClaudeCLIRateLimitParsesResetTime(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)

	now := time.Date(2026, 5, 1, 15, 0, 0, 0, location)
	output := []byte(`{"type":"system","subtype":"api_retry","attempt":1,"max_retries":10,"retry_delay_ms":594,"error_status":429,"error":"rate_limit","session_id":"s1"}` + "\n" +
		`{"type":"assistant","message":{"id":"msg_1","model":"<synthetic>","role":"assistant","type":"message","content":[{"type":"text","text":"API Error: Request rejected (429) · You've hit your limit · resets 3:50pm (Asia/Shanghai)"}]},"session_id":"s1","error":"rate_limit"}` + "\n" +
		`{"type":"result","subtype":"success","is_error":true,"api_error_status":429,"result":"API Error: Request rejected (429) · You've hit your limit · resets 3:50pm (Asia/Shanghai)","session_id":"s1"}`)

	signal := detectClaudeCLIRateLimit(output, now)

	require.NotNil(t, signal)
	require.Equal(t, time.Date(2026, 5, 1, 15, 50, 0, 0, location), signal.ResetAt)
}

func TestDetectClaudeCLIRateLimitMovesPastResetToNextDay(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)

	now := time.Date(2026, 5, 1, 16, 0, 0, 0, location)
	output := []byte(`{"type":"result","subtype":"success","is_error":true,"api_error_status":429,"result":"You've hit your limit · resets 3:50pm (Asia/Shanghai)"}`)

	signal := detectClaudeCLIRateLimit(output, now)

	require.NotNil(t, signal)
	require.Equal(t, time.Date(2026, 5, 2, 15, 50, 0, 0, location), signal.ResetAt)
}

func TestDetectClaudeCLIRateLimitIgnoresUnstructuredMatchingText(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)

	now := time.Date(2026, 5, 1, 15, 0, 0, 0, location)
	output := []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"A user pasted: You've hit your limit · resets 3:50pm (Asia/Shanghai)"}]}}`)

	signal := detectClaudeCLIRateLimit(output, now)

	require.Nil(t, signal)
}

func TestDetectClaudeCLIRateLimitIgnoresAPIRetryWithoutFinalResult(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)

	now := time.Date(2026, 5, 1, 15, 0, 0, 0, location)
	output := []byte(`{"type":"system","subtype":"api_retry","attempt":1,"max_retries":10,"retry_delay_ms":594,"error_status":429,"error":"rate_limit","session_id":"s1"}`)

	signal := detectClaudeCLIRateLimit(output, now)

	require.Nil(t, signal)
}
