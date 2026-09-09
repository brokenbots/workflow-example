// Package events parses the Criteria events ndjson file produced by a run.
package events

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
)

// Outcome holds values extracted from a run events file.
type Outcome struct {
	PRNumber    string `json:"prNumber,omitempty"`
	TicketState string `json:"ticketState,omitempty"`
	Terminal    string `json:"terminal,omitempty"`
}

// Parse scans an ndjson events stream and extracts the best-known outcome.
func Parse(r *bufio.Reader) (*Outcome, error) {
	out := &Outcome{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev map[string]json.RawMessage
		if err := json.Unmarshal(line, &ev); err != nil {
			// Non-JSON lines are ignored; Criteria events are JSON objects.
			continue
		}
		// Extract terminal state if present.
		if raw, ok := ev["terminal"]; ok {
			var term string
			if err := json.Unmarshal(raw, &term); err == nil {
				out.Terminal = term
			}
		}
		if raw, ok := ev["state"]; ok {
			var state string
			if err := json.Unmarshal(raw, &state); err == nil {
				out.TicketState = state
			}
		}
		// Some events emit structured pr_number fields (string or number).
		for _, key := range []string{"pr_number", "prNumber", "pull_request_number"} {
			if raw, ok := ev[key]; ok {
				var num string
				if err := json.Unmarshal(raw, &num); err == nil && num != "" {
					out.PRNumber = num
				} else {
					var numInt int
					if err := json.Unmarshal(raw, &numInt); err == nil && numInt != 0 {
						out.PRNumber = strconv.Itoa(numInt)
					}
				}
			}
		}
		// Search stdout-like string payloads for pr_number=NN or PR #NN.
		for _, raw := range ev {
			out.PRNumber = firstNonEmpty(out.PRNumber, extractPRNumberFromRaw(raw))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning events: %w", err)
	}
	return out, nil
}

var prPatterns = []*regexp.Regexp{
	regexp.MustCompile(`pr_number\s*=\s*(\d+)`),
	regexp.MustCompile(`(?i)pr\s*#\s*(\d+)`),
	regexp.MustCompile(`(?i)pull\s+request\s*#?\s*(\d+)`),
	regexp.MustCompile(`github\.com/[^/]+/[^/]+/pull/(\d+)`),
}

func extractPRNumberFromRaw(raw json.RawMessage) string {
	// If the value is a JSON string, decode and scan it.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		for _, re := range prPatterns {
			if m := re.FindStringSubmatch(s); m != nil {
				if _, err := strconv.Atoi(m[1]); err == nil {
					return m[1]
				}
			}
		}
	}
	// For non-string JSON values, scan the raw bytes as text.
	for _, re := range prPatterns {
		if m := re.FindSubmatch(raw); m != nil {
			if _, err := strconv.Atoi(string(m[1])); err == nil {
				return string(m[1])
			}
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// ParseBytes is a convenience wrapper around Parse.
func ParseBytes(data []byte) (*Outcome, error) {
	return Parse(bufio.NewReader(bytes.NewReader(data)))
}
