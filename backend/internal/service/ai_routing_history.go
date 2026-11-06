package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

const AIRoutingHistoryProjectionVersion = "messages-history-v1"

// AIRoutingContextLimits bounds the whole evidence-selection operation. Input
// byte budgets include the caller's reserved prompt, options and wire overhead.
// They are conservative byte limits, not tokenizer or monetary guarantees.
type AIRoutingContextLimits struct {
	MaxHistoryBytes     int `json:"max_history_bytes"`
	MaxHistoryTurns     int `json:"max_history_turns"`
	MaxSegments         int `json:"max_segments"`
	MaxDecisionCalls    int `json:"max_decision_calls"`
	MaxParallelCalls    int `json:"max_parallel_calls"`
	MaxInputBytes       int `json:"max_input_bytes"`
	MaxTotalInputBytes  int `json:"max_total_input_bytes"`
	MaxOutputBytes      int `json:"max_output_bytes"`
	MaxTotalOutputBytes int `json:"max_total_output_bytes"`
	MaxRefinementPasses int `json:"max_refinement_passes"`
}

func NormalizeAIRoutingContextLimits(v AIRoutingContextLimits) (AIRoutingContextLimits, error) {
	fields := []struct {
		value                      *int
		fallback, minimum, maximum int
	}{
		{&v.MaxHistoryBytes, 128 << 10, 1, 4 << 20},
		{&v.MaxHistoryTurns, 256, 1, 4096},
		{&v.MaxSegments, 32, 1, 256},
		{&v.MaxDecisionCalls, 33, 1, 1024},
		{&v.MaxParallelCalls, 1, 1, 8},
		{&v.MaxInputBytes, 24 << 10, 1024, 256 << 10},
		{&v.MaxTotalInputBytes, 512 << 10, 1024, 16 << 20},
		{&v.MaxOutputBytes, 64 << 10, 1024, aiDecisionMaxResponse},
		{&v.MaxTotalOutputBytes, 2 << 20, 1024, 16 << 20},
	}
	for _, f := range fields {
		if *f.value == 0 {
			*f.value = f.fallback
		}
		if *f.value < f.minimum || *f.value > f.maximum {
			return v, &AIRoutingHistoryError{Reason: "invalid_context_budget"}
		}
	}
	// This first implementation is serial and performs no recursive semantic
	// refinement. A larger parallel maximum is still respected by serial work.
	if v.MaxRefinementPasses != 0 || v.MaxTotalInputBytes < v.MaxInputBytes || v.MaxTotalOutputBytes < v.MaxOutputBytes {
		return v, &AIRoutingHistoryError{Reason: "invalid_context_budget"}
	}
	return v, nil
}

// Offsets refer to UTF-8 bytes of the redacted source block. They are never
// offsets into secret-bearing input. SourceID identifies the original block.
type AIRoutingHistorySpan struct {
	ID        string `json:"id"`
	SourceID  string `json:"source_id"`
	TurnID    string `json:"turn_id"`
	Role      string `json:"role"`
	StartByte int    `json:"start_byte"`
	EndByte   int    `json:"end_byte"`
	Redacted  bool   `json:"redacted,omitempty"`
	Text      string `json:"text"`
}

// AIRoutingHistory is request-local. It must not be written to traces or caches.
type AIRoutingHistory struct {
	ProjectionVersion string                 `json:"-"`
	Spans             []AIRoutingHistorySpan `json:"-"`
	CurrentTurnID     string                 `json:"-"`
	HistoryTurns      int                    `json:"-"`
	HistoryBytes      int                    `json:"-"`
	RedactedSpans     int                    `json:"-"`
	ExcludedBlocks    int                    `json:"-"`
}

type AIRoutingHistoryDiagnostics struct {
	ProjectionVersion string   `json:"projection_version"`
	Source            string   `json:"source"`
	Mode              string   `json:"mode"`
	Coverage          string   `json:"coverage"`
	HistoryTurns      int      `json:"history_turns"`
	HistoryBytes      int      `json:"history_bytes"`
	TotalSpans        int      `json:"total_spans"`
	ScannedSpans      int      `json:"scanned_spans"`
	SelectedSpanIDs   []string `json:"selected_span_ids"`
	Segments          int      `json:"segments"`
	ScannedSegments   int      `json:"scanned_segments"`
	ClassifierCalls   int      `json:"classifier_calls"`
	InputBytes        int      `json:"input_bytes"`
	RedactedSpans     int      `json:"redacted_spans"`
	ExcludedBlocks    int      `json:"excluded_blocks"`
	FailureReason     string   `json:"failure_reason,omitempty"`
}

type AIRoutingHistoryResult struct {
	Task                string                      `json:"-"`
	Diagnostics         AIRoutingHistoryDiagnostics `json:"diagnostics"`
	RemainingCalls      int                         `json:"-"`
	RemainingInputBytes int                         `json:"-"`
}

type AIRoutingHistoryError struct{ Reason string }

func (e *AIRoutingHistoryError) Error() string { return e.Reason }

type AIRoutingHistoryRelevance string

const (
	AIRoutingHistoryRelevant   AIRoutingHistoryRelevance = "relevant"
	AIRoutingHistoryIrrelevant AIRoutingHistoryRelevance = "irrelevant"
	AIRoutingHistoryUncertain  AIRoutingHistoryRelevance = "uncertain"
)

type AIRoutingHistoryClassificationRequest struct {
	SegmentID string
	Task      string
}

// One callback means one upstream attempt. Adapters must not hide retries or
// launch detached work, and must obey ctx. Only the finite result is accepted;
// an upstream never gets to generate evidence IDs or replacement text.
type AIRoutingHistoryClassifier func(context.Context, AIRoutingHistoryClassificationRequest) (AIRoutingHistoryRelevance, error)

// ParseAIRoutingMessagesHistory reuses the Messages protocol and ambiguity
// validation. Only user/assistant visible text is retained. No system, tool,
// image, document or hidden-thinking body is copied into the history view.
func ParseAIRoutingMessagesHistory(body []byte, limits AIRoutingContextLimits) (*AIRoutingHistory, error) {
	limits, err := NormalizeAIRoutingContextLimits(limits)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(body) {
		return nil, &AIRoutingHistoryError{Reason: "history_invalid_request"}
	}
	if _, err = NewAIRoutingRequest(body, "metadata"); err != nil {
		return nil, &AIRoutingHistoryError{Reason: "history_invalid_request"}
	}
	h := &AIRoutingHistory{ProjectionVersion: AIRoutingHistoryProjectionVersion}
	lastRole := ""
	var parseErr error
	gjson.ParseBytes(body).Get("messages").ForEach(func(index, message gjson.Result) bool {
		role := message.Get("role").String()
		if role != lastRole {
			h.HistoryTurns++
			lastRole = role
		}
		if h.HistoryTurns > limits.MaxHistoryTurns {
			parseErr = &AIRoutingHistoryError{Reason: "history_turn_limit"}
			return false
		}
		turnID := fmt.Sprintf("t%d", h.HistoryTurns)
		if role == "user" {
			h.CurrentTurnID = turnID
		}
		add := func(text string, block int) bool {
			h.HistoryBytes += len(text)
			if h.HistoryBytes > limits.MaxHistoryBytes {
				parseErr = &AIRoutingHistoryError{Reason: "history_byte_limit"}
				return false
			}
			if strings.TrimSpace(text) == "" {
				return true
			}
			clean := aiHistoryRedact(text)
			redacted := clean != text
			if redacted {
				h.RedactedSpans++
			}
			sourceID := fmt.Sprintf("m%db%d", index.Int()+1, block)
			h.Spans = append(h.Spans, aiHistorySpan(sourceID, turnID, role, 0, clean, redacted))
			return true
		}
		content := message.Get("content")
		if content.Type == gjson.String {
			return add(content.Str, 0)
		}
		content.ForEach(func(block, part gjson.Result) bool {
			if part.Get("type").Str != "text" {
				h.ExcludedBlocks++
				return true
			}
			text := part.Get("text")
			if text.Type != gjson.String {
				parseErr = &AIRoutingHistoryError{Reason: "history_invalid_request"}
				return false
			}
			return add(text.Str, int(block.Int()))
		})
		return parseErr == nil
	})
	if parseErr != nil {
		return nil, parseErr
	}
	return h, nil
}

func aiHistoryRedact(text string) string {
	if strings.Contains(text, "PRIVATE KEY-----") {
		return "[redacted]"
	}
	// The shared redactor trims its input. Preserve the surrounding whitespace
	// here so code indentation and source span boundaries remain meaningful.
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return text
	}
	start := strings.Index(text, trimmed)
	clean := text[:start] + redactContentModerationSecrets(trimmed) + text[start+len(trimmed):]
	return aiSecret.ReplaceAllString(clean, "[redacted]")
}

func aiHistorySpan(source, turn, role string, start int, text string, redacted bool) AIRoutingHistorySpan {
	return AIRoutingHistorySpan{ID: fmt.Sprintf("%s:%d-%d", source, start, start+len(text)), SourceID: source, TurnID: turn, Role: role, StartByte: start, EndByte: start + len(text), Text: text, Redacted: redacted}
}

type aiHistoryTask struct {
	ProjectionVersion string                 `json:"projection_version"`
	Kind              string                 `json:"kind"`
	CurrentTurnID     string                 `json:"current_turn_id"`
	CurrentRequest    string                 `json:"current_request,omitempty"`
	Coverage          string                 `json:"coverage"`
	Recent            []AIRoutingHistorySpan `json:"recent,omitempty"`
	Spans             []AIRoutingHistorySpan `json:"spans"`
}

func aiHistoryEncode(task aiHistoryTask) string {
	raw, _ := json.Marshal(task)
	return string(raw)
}

func aiHistoryFailure(r AIRoutingHistoryResult, reason string) (AIRoutingHistoryResult, error) {
	r.Task = ""
	r.Diagnostics.FailureReason = reason
	return r, &AIRoutingHistoryError{Reason: reason}
}

// BuildAIRoutingHistoryTask either supplies complete ordered text without any
// classification call, or scans every segment before building final evidence.
// It reserves one call and a full per-request byte budget for the final Choice.
// An optional pure size function measures the complete encoded upstream request;
// it must be monotonic as span text grows and return a positive byte count. The
// default is len(task)+reservedInputBytes. The caller still checks actual wire
// size immediately before the final call. reservedInputBytes also reserves space
// for the current-task anchor when a custom size function is present.
func BuildAIRoutingHistoryTask(ctx context.Context, history *AIRoutingHistory, limits AIRoutingContextLimits, reservedInputBytes int, classify AIRoutingHistoryClassifier, size ...func(string) int) (AIRoutingHistoryResult, error) {
	r := AIRoutingHistoryResult{Diagnostics: AIRoutingHistoryDiagnostics{ProjectionVersion: AIRoutingHistoryProjectionVersion, Source: "request", Mode: "full", Coverage: "none", SelectedSpanIDs: []string{}}}
	limits, err := NormalizeAIRoutingContextLimits(limits)
	if err != nil {
		return r, err
	}
	r.RemainingCalls, r.RemainingInputBytes = limits.MaxDecisionCalls, limits.MaxTotalInputBytes
	if history == nil {
		return aiHistoryFailure(r, "history_invalid_request")
	}
	if history.ProjectionVersion != "" {
		r.Diagnostics.ProjectionVersion = history.ProjectionVersion
	}
	r.Diagnostics.HistoryTurns, r.Diagnostics.HistoryBytes = history.HistoryTurns, history.HistoryBytes
	r.Diagnostics.RedactedSpans, r.Diagnostics.ExcludedBlocks = history.RedactedSpans, history.ExcludedBlocks
	r.Diagnostics.TotalSpans = len(history.Spans)
	if err = ctx.Err(); err != nil {
		r.Diagnostics.FailureReason = "cancelled"
		return r, err
	}
	if reservedInputBytes < 0 || reservedInputBytes >= limits.MaxInputBytes {
		return aiHistoryFailure(r, "history_input_budget")
	}
	if len(size) > 1 || len(size) == 1 && size[0] == nil {
		return aiHistoryFailure(r, "history_input_budget")
	}
	measure := func(task string) int { return len(task) + reservedInputBytes }
	if len(size) == 1 {
		measure = func(task string) int {
			value := size[0](task)
			if value <= 0 || value > limits.MaxTotalInputBytes {
				// Keep failed sizing and integer overflow away from fit/budget
				// arithmetic. An unavailable envelope is never free input.
				return limits.MaxTotalInputBytes + 1
			}
			return value
		}
	}
	if history.HistoryBytes > limits.MaxHistoryBytes || history.HistoryTurns > limits.MaxHistoryTurns {
		return aiHistoryFailure(r, "history_limit")
	}
	if history.CurrentTurnID == "" || len(history.Spans) == 0 {
		return aiHistoryFailure(r, "context_insufficient")
	}
	hasCurrentText := false
	for _, span := range history.Spans {
		if span.TurnID == history.CurrentTurnID {
			hasCurrentText = true
			break
		}
	}
	if !hasCurrentText {
		return aiHistoryFailure(r, "context_insufficient")
	}
	task := aiHistoryTask{ProjectionVersion: r.Diagnostics.ProjectionVersion, Kind: "route", CurrentTurnID: history.CurrentTurnID, Coverage: "complete", Spans: history.Spans}
	encoded := aiHistoryEncode(task)
	if measure(encoded) <= limits.MaxInputBytes {
		r.Task = encoded
		r.Diagnostics.Coverage = "complete"
		r.Diagnostics.ScannedSpans = len(history.Spans)
		for _, span := range history.Spans {
			r.Diagnostics.SelectedSpanIDs = append(r.Diagnostics.SelectedSpanIDs, span.ID)
		}
		return r, nil
	}
	r.Diagnostics.Mode = "evidence"
	anchor, recent, ok := aiHistoryAnchors(history, limits.MaxInputBytes-reservedInputBytes)
	if !ok {
		return aiHistoryFailure(r, "history_anchor_unavailable")
	}
	task.CurrentRequest, task.Recent, task.Kind, task.Coverage = anchor, recent, "history_relevance", "segment"
	task.Spans = nil
	segments, err := aiHistorySegments(history.Spans, task, limits.MaxInputBytes, limits.MaxSegments, measure)
	if err != nil {
		return aiHistoryFailure(r, "history_segment_limit")
	}
	r.Diagnostics.Segments = len(segments)
	r.Diagnostics.TotalSpans = 0
	needed := limits.MaxInputBytes // Reserve the final Choice even if no evidence fits.
	for _, segment := range segments {
		cost := measure(segment.Task)
		if cost > limits.MaxTotalInputBytes-needed {
			return aiHistoryFailure(r, "history_budget_exhausted")
		}
		needed += cost
		r.Diagnostics.TotalSpans += len(segment.Spans)
	}
	if len(segments)+1 > limits.MaxDecisionCalls || needed > limits.MaxTotalInputBytes {
		return aiHistoryFailure(r, "history_budget_exhausted")
	}
	if classify == nil {
		return aiHistoryFailure(r, "history_classifier_unavailable")
	}
	selected := make(map[string]bool)
	allSpans := make([]AIRoutingHistorySpan, 0, r.Diagnostics.TotalSpans)
	for i, segment := range segments {
		if err = ctx.Err(); err != nil {
			r.Diagnostics.FailureReason = "cancelled"
			return r, err
		}
		inputBytes := measure(segment.Task)
		r.Diagnostics.ClassifierCalls++
		r.Diagnostics.InputBytes += inputBytes
		r.RemainingCalls--
		r.RemainingInputBytes -= inputBytes
		relevance, callErr := classify(ctx, AIRoutingHistoryClassificationRequest{SegmentID: fmt.Sprintf("s%d", i+1), Task: segment.Task})
		if err = ctx.Err(); err != nil {
			r.Diagnostics.FailureReason = "cancelled"
			return r, err
		}
		if callErr != nil {
			return aiHistoryFailure(r, "history_classification_failed")
		}
		if relevance != AIRoutingHistoryRelevant && relevance != AIRoutingHistoryIrrelevant && relevance != AIRoutingHistoryUncertain {
			return aiHistoryFailure(r, "history_invalid_classification")
		}
		r.Diagnostics.ScannedSegments++
		r.Diagnostics.ScannedSpans += len(segment.Spans)
		r.Diagnostics.Coverage = "partial"
		for _, span := range segment.Spans {
			allSpans = append(allSpans, span)
			if relevance != AIRoutingHistoryIrrelevant {
				selected[span.ID] = true
			}
		}
	}
	r.Diagnostics.Coverage = "complete"
	// Include immediate source neighbors, once, preserving original order. This
	// is evidence context, not a claim that semantic references are resolved.
	withNeighbors := make(map[string]bool, len(selected)*3)
	for i, span := range allSpans {
		if !selected[span.ID] {
			continue
		}
		for j := max(0, i-1); j <= min(len(allSpans)-1, i+1); j++ {
			withNeighbors[allSpans[j].ID] = true
		}
	}
	if len(withNeighbors) == 0 {
		return aiHistoryFailure(r, "context_insufficient")
	}
	task.Kind, task.Coverage, task.Recent, task.Spans = "route", "complete", nil, nil
	for _, span := range allSpans {
		if withNeighbors[span.ID] {
			task.Spans = append(task.Spans, span)
			r.Diagnostics.SelectedSpanIDs = append(r.Diagnostics.SelectedSpanIDs, span.ID)
		}
	}
	encoded = aiHistoryEncode(task)
	if measure(encoded) > limits.MaxInputBytes || measure(encoded) > r.RemainingInputBytes {
		return aiHistoryFailure(r, "history_evidence_overflow")
	}
	r.Task = encoded
	return r, nil
}

type aiHistorySegment struct {
	Task  string
	Spans []AIRoutingHistorySpan
}

func aiHistorySegments(spans []AIRoutingHistorySpan, template aiHistoryTask, budget, maximum int, measure func(string) int) ([]aiHistorySegment, error) {
	var segments []aiHistorySegment
	var current []AIRoutingHistorySpan
	encode := func(candidate []AIRoutingHistorySpan) string {
		template.Spans = candidate
		return aiHistoryEncode(template)
	}
	// A relevant segment and its immediate neighbors should have room in the
	// final input. Filling every classification request to its limit would make
	// even one selected long segment plus neighbors routinely overflow.
	overhead := measure(encode(nil))
	budget = min(budget, overhead+max(256, (budget-overhead)/4))
	flush := func() bool {
		if len(current) == 0 {
			return true
		}
		segments = append(segments, aiHistorySegment{Task: encode(current), Spans: current})
		current = nil
		return len(segments) <= maximum
	}
	for _, source := range spans {
		rest, start := source.Text, source.StartByte
		for rest != "" {
			span := aiHistorySpan(source.SourceID, source.TurnID, source.Role, start, rest, source.Redacted)
			candidate := append(append([]AIRoutingHistorySpan(nil), current...), span)
			if measure(encode(candidate)) <= budget {
				current = candidate
				break
			}
			if len(current) > 0 {
				if !flush() {
					return nil, errors.New("segment limit")
				}
				continue
			}
			// Find an exact encoded-size fit without cutting a UTF-8 codepoint.
			low, high, fit := 1, len(rest), 0
			for low <= high {
				middle := low + (high-low)/2
				cut := middle
				for cut > 0 && !utf8.ValidString(rest[:cut]) {
					cut--
				}
				part := aiHistorySpan(source.SourceID, source.TurnID, source.Role, start, rest[:cut], source.Redacted)
				if cut > 0 && measure(encode([]AIRoutingHistorySpan{part})) <= budget {
					fit = cut
					low = middle + 1
				} else {
					high = middle - 1
				}
			}
			if fit == 0 {
				return nil, errors.New("segment cannot fit")
			}
			current = []AIRoutingHistorySpan{aiHistorySpan(source.SourceID, source.TurnID, source.Role, start, rest[:fit], source.Redacted)}
			if !flush() {
				return nil, errors.New("segment limit")
			}
			start += fit
			rest = rest[fit:]
		}
	}
	if !flush() {
		return nil, errors.New("segment limit")
	}
	return segments, nil
}

func aiHistoryAnchors(history *AIRoutingHistory, budget int) (string, []AIRoutingHistorySpan, bool) {
	var current []string
	for _, span := range history.Spans {
		if span.TurnID == history.CurrentTurnID {
			current = append(current, span.Text)
		}
	}
	anchor := strings.Join(current, "\n")
	anchorBudget := min(4096, budget/4)
	if len(anchor) > anchorBudget {
		// Fenced code/data stays in the coverage scan. Only complete prose
		// outside balanced fences may serve as a bounded current instruction.
		var ok bool
		anchor, ok = aiHistoryOutsideFences(anchor)
		if !ok {
			return "", nil, false
		}
	}
	if strings.TrimSpace(anchor) == "" || len(anchor) > anchorBudget {
		return "", nil, false
	}
	recentBudget := min(2048, budget/8)
	var recent []AIRoutingHistorySpan
	for i := len(history.Spans) - 1; i >= 0; i-- {
		span := history.Spans[i]
		if span.TurnID == history.CurrentTurnID {
			continue
		}
		// Do not substitute an arbitrary prefix for a complete recent block.
		if len(span.Text) > recentBudget {
			break
		}
		recent = append(recent, span)
		recentBudget -= len(span.Text)
		if len(recent) == 2 {
			break
		}
	}
	for i, j := 0, len(recent)-1; i < j; i, j = i+1, j-1 {
		recent[i], recent[j] = recent[j], recent[i]
	}
	return anchor, recent, true
}

func aiHistoryOutsideFences(text string) (string, bool) {
	var lines []string
	fence := byte(0)
	fenceLength := 0
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if fence == 0 {
			if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
				fence = trimmed[0]
				for fenceLength < len(trimmed) && trimmed[fenceLength] == fence {
					fenceLength++
				}
				continue
			}
			lines = append(lines, line)
		} else {
			closing := 0
			for closing < len(trimmed) && trimmed[closing] == fence {
				closing++
			}
			if closing >= fenceLength && strings.TrimSpace(trimmed[closing:]) == "" {
				fence = 0
				fenceLength = 0
			}
		}
	}
	return strings.Join(lines, "\n"), fence == 0
}
