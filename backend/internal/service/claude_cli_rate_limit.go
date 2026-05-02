package service

import (
	"bufio"
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type claudeCLIRateLimitSignal struct {
	ResetAt time.Time
}

var claudeCLIResetPattern = regexp.MustCompile(`(?i)reset(?:s)?(?:\s+at)?\s+(\d{1,2})(?::(\d{2}))?\s*(am|pm)?(?:\s*\(([^)]+)\))?`)

func detectClaudeCLIRateLimit(output []byte, now time.Time) *claudeCLIRateLimitSignal {
	if len(output) == 0 {
		return nil
	}

	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 0, 4096), 1024*1024)
	for scanner.Scan() {
		if signal := claudeCLIRateLimitSignalFromJSONLine(scanner.Bytes(), now); signal != nil {
			return signal
		}
	}
	return nil
}

func claudeCLIRateLimitSignalFromJSONLine(line []byte, now time.Time) *claudeCLIRateLimitSignal {
	var payload struct {
		Type           string `json:"type"`
		IsError        bool   `json:"is_error"`
		APIErrorStatus int    `json:"api_error_status"`
		Result         string `json:"result"`
	}
	if err := json.Unmarshal(line, &payload); err != nil {
		return nil
	}
	if payload.Type != "result" || !payload.IsError || payload.APIErrorStatus != 429 {
		return nil
	}
	resetAt, ok := parseClaudeCLIResetAt(payload.Result, now)
	if !ok {
		return nil
	}
	return &claudeCLIRateLimitSignal{ResetAt: resetAt}
}

func parseClaudeCLIResetAt(text string, now time.Time) (time.Time, bool) {
	matches := claudeCLIResetPattern.FindStringSubmatch(text)
	if len(matches) == 0 {
		return time.Time{}, false
	}

	hour, err := strconv.Atoi(matches[1])
	if err != nil {
		return time.Time{}, false
	}
	minute := 0
	if matches[2] != "" {
		minute, err = strconv.Atoi(matches[2])
		if err != nil {
			return time.Time{}, false
		}
	}

	meridiem := strings.ToLower(matches[3])
	switch meridiem {
	case "pm":
		if hour < 12 {
			hour += 12
		}
	case "am":
		if hour == 12 {
			hour = 0
		}
	}

	location := now.Location()
	if matches[4] != "" {
		if loaded, err := time.LoadLocation(matches[4]); err == nil {
			location = loaded
		}
	}

	localNow := now.In(location)
	resetAt := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), hour, minute, 0, 0, location)
	if !resetAt.After(localNow) {
		resetAt = resetAt.Add(24 * time.Hour)
	}
	return resetAt, true
}
