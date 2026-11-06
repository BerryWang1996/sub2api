package service

import (
	"fmt"
	"strings"
)

const AIRoutingResponsesHistoryProjectionVersion = "responses-history-v1"

// ParseAIRoutingRequestHistory is shared by runtime and preview. An empty
// endpoint preserves existing Messages callers; no protocol is inferred from
// overlapping field names or by rewriting the execution body.
func ParseAIRoutingRequestHistory(request AIRoutingRequest, limits AIRoutingContextLimits) (*AIRoutingHistory, error) {
	switch request.Endpoint {
	case "", CompositeRouteEndpointMessages:
		return ParseAIRoutingMessagesHistory(request.HistoryBody, limits)
	case CompositeRouteEndpointResponses:
		return ParseAIRoutingResponsesHistory(request.HistoryBody, limits)
	case AIRoutingEndpointResponsesWS:
		return ParseAIRoutingWSHistory(request.HistoryBody, limits)
	case CompositeRouteEndpointChatCompletions:
		return ParseAIRoutingChatHistory(request.HistoryBody, limits)
	case CompositeRouteEndpointGemini:
		return ParseAIRoutingGeminiHistory(request.HistoryBody, limits)
	default:
		return nil, &AIRoutingHistoryError{Reason: "history_protocol_unsupported"}
	}
}

func aiRoutingRequestHistoryVersion(request AIRoutingRequest) string {
	if request.Endpoint == CompositeRouteEndpointResponses || request.Endpoint == AIRoutingEndpointResponsesWS {
		return AIRoutingResponsesHistoryProjectionVersion
	}
	if request.Endpoint == CompositeRouteEndpointChatCompletions {
		return AIRoutingChatHistoryProjectionVersion
	}
	if request.Endpoint == CompositeRouteEndpointGemini {
		return AIRoutingGeminiHistoryProjectionVersion
	}
	return AIRoutingHistoryProjectionVersion
}

// ParseAIRoutingResponsesHistory retains only visible user/assistant text.
// Non-text assistant actions still delimit turns; excluding their bodies must
// not merge two separate user tasks into one current instruction.
func ParseAIRoutingResponsesHistory(body []byte, limits AIRoutingContextLimits) (*AIRoutingHistory, error) {
	limits, err := NormalizeAIRoutingContextLimits(limits)
	if err != nil {
		return nil, err
	}
	parsed, err := parseAIRoutingResponses(body, "metadata")
	if err != nil {
		return nil, &AIRoutingHistoryError{Reason: "history_invalid_request"}
	}
	history := &AIRoutingHistory{ProjectionVersion: AIRoutingResponsesHistoryProjectionVersion}
	lastRole := ""
	for _, item := range parsed.Items {
		history.ExcludedBlocks += item.Excluded
		if item.Role != "user" && item.Role != "assistant" {
			lastRole = ""
			continue
		}
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
			source := fmt.Sprintf("r%db%d", item.Index+1, part.Block)
			history.Spans = append(history.Spans, aiHistorySpan(source, turn, item.Role, 0, clean, redacted))
		}
	}
	return history, nil
}
