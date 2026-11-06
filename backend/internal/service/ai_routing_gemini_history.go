package service

import (
	"fmt"
	"strings"
)

const AIRoutingGeminiHistoryProjectionVersion = "gemini-history-v1"

func ParseAIRoutingGeminiHistory(body []byte, limits AIRoutingContextLimits) (*AIRoutingHistory, error) {
	limits, err := NormalizeAIRoutingContextLimits(limits)
	if err != nil {
		return nil, err
	}
	parsed, err := parseAIRoutingGemini(body, "", "generateContent", "metadata")
	if err != nil {
		return nil, &AIRoutingHistoryError{Reason: "history_invalid_request"}
	}
	history := &AIRoutingHistory{ProjectionVersion: AIRoutingGeminiHistoryProjectionVersion}
	lastRole := ""
	for _, item := range parsed.Items {
		history.ExcludedBlocks += item.Excluded
		if item.Role != lastRole {
			history.HistoryTurns++
			lastRole = item.Role
		}
		if history.HistoryTurns > limits.MaxHistoryTurns {
			return nil, &AIRoutingHistoryError{Reason: "history_turn_limit"}
		}
		turn := fmt.Sprintf("t%d", history.HistoryTurns)
		if item.Role == "user" {
			history.CurrentTurnID = turn
		}
		for _, part := range item.Parts {
			history.HistoryBytes += len(part.Text)
			if history.HistoryBytes > limits.MaxHistoryBytes {
				return nil, &AIRoutingHistoryError{Reason: "history_byte_limit"}
			}
			if strings.TrimSpace(part.Text) == "" {
				continue
			}
			clean := aiHistoryRedact(part.Text)
			redacted := clean != part.Text
			if redacted {
				history.RedactedSpans++
			}
			source := fmt.Sprintf("g%db%d", item.Index+1, part.Block)
			history.Spans = append(history.Spans, aiHistorySpan(source, turn, item.Role, 0, clean, redacted))
		}
	}
	return history, nil
}
