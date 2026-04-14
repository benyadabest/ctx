package pipeline

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var fencePattern = regexp.MustCompile("(?s)^```(?:json)?\\s*(.+?)\\s*```$")

// ParseJSONResponse parses JSON from an LLM response, stripping markdown fences if needed.
func ParseJSONResponse(raw string) (map[string]any, error) {
	raw = strings.TrimSpace(raw)

	// Try direct parse
	var result map[string]any
	if err := json.Unmarshal([]byte(raw), &result); err == nil {
		return result, nil
	}

	// Strip markdown fences and retry
	if m := fencePattern.FindStringSubmatch(raw); len(m) > 1 {
		if err := json.Unmarshal([]byte(m[1]), &result); err == nil {
			return result, nil
		}
	}

	return nil, fmt.Errorf("failed to parse JSON from LLM response: %.100s", raw)
}
