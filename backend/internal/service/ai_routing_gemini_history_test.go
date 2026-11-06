//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAIRoutingGeminiHistoryVisibleTextAndDispatch(t *testing.T) {
	id := GeminiResponsesToolCallPrefix + "history"
	body := aiGeminiTestBody(t, []any{
		aiGeminiTestContent("user", map[string]any{"text": "最初需要翻译"}),
		aiGeminiTestContent("model", map[string]any{"thought": true, "text": "private-thought"}, map[string]any{"text": "第二项是推理"}, map[string]any{"functionCall": map[string]any{"id": id, "name": "f", "args": map[string]any{"secret": "private-args"}}}),
		aiGeminiTestContent("user", map[string]any{"functionResponse": map[string]any{"id": id, "name": "f", "response": map[string]any{"secret": "private-result"}}}, map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": "private-image"}}),
		aiGeminiTestContent("model", map[string]any{"text": "现在有了结果"}),
		aiGeminiTestContent("user", map[string]any{"text": "纠正：请证明定理 password=private-password"}, map[string]any{"text": "继续"}),
	}, map[string]any{"systemInstruction": map[string]any{"parts": []any{map[string]any{"text": "private-system"}}}, "tools": []any{map[string]any{"functionDeclarations": []any{map[string]any{"name": "f", "description": "private-schema"}}}}})
	original := append([]byte(nil), body...)
	r, err := NewAIRoutingGeminiRequest(body, "alias", "generateContent", "bounded_history")
	require.NoError(t, err)
	history, err := ParseAIRoutingRequestHistory(r, AIRoutingContextLimits{})
	require.NoError(t, err)
	require.Equal(t, original, body)
	require.Equal(t, AIRoutingGeminiHistoryProjectionVersion, history.ProjectionVersion)
	require.Len(t, history.Spans, 5)
	require.Equal(t, "g1b0", history.Spans[0].SourceID)
	require.Equal(t, "g2b1", history.Spans[1].SourceID)
	require.Equal(t, history.CurrentTurnID, history.Spans[3].TurnID)
	require.Equal(t, history.CurrentTurnID, history.Spans[4].TurnID)
	require.Equal(t, 1, history.RedactedSpans)
	prepared, err := BuildAIRoutingHistoryTask(t.Context(), history, AIRoutingContextLimits{}, 100, nil)
	require.NoError(t, err)
	for _, secret := range []string{"private-thought", "private-args", "private-result", "private-image", "private-system", "private-schema", "private-password"} {
		require.NotContains(t, prepared.Task, secret)
	}
	require.Contains(t, prepared.Task, "最初需要翻译")
	require.Contains(t, prepared.Task, "请证明定理")
	require.Contains(t, prepared.Task, "第二项是推理")
	repeated, err := ParseAIRoutingGeminiHistory(body, AIRoutingContextLimits{})
	require.NoError(t, err)
	require.Equal(t, history, repeated)
}

func TestAIRoutingGeminiHistorySameLastInstructionRetainsDifferentHistory(t *testing.T) {
	var tasks []string
	for _, first := range []string{"只翻译成中文", "之前纠正：需要严格数学证明"} {
		body := aiGeminiTestBody(t, []any{aiGeminiTestContent("user", map[string]any{"text": first}), aiGeminiTestContent("model", map[string]any{"text": "好的"}), aiGeminiTestContent("user", map[string]any{"text": "继续"})}, nil)
		history, err := ParseAIRoutingGeminiHistory(body, AIRoutingContextLimits{})
		require.NoError(t, err)
		prepared, err := BuildAIRoutingHistoryTask(t.Context(), history, AIRoutingContextLimits{}, 50, nil)
		require.NoError(t, err)
		require.Contains(t, prepared.Task, first)
		tasks = append(tasks, prepared.Task)
	}
	require.NotEqual(t, tasks[0], tasks[1], "this asserts projection fidelity, not model semantic accuracy")
}

func TestAIRoutingGeminiHistoryLimitsAndHiddenBoundaries(t *testing.T) {
	body := aiGeminiTestBody(t, []any{aiGeminiTestContent("user", map[string]any{"text": "old task"}), aiGeminiTestContent("model", map[string]any{"thought": true, "text": "private"}), aiGeminiTestContent("user", map[string]any{"text": "new task"})}, nil)
	history, err := ParseAIRoutingGeminiHistory(body, AIRoutingContextLimits{})
	require.NoError(t, err)
	require.Equal(t, 3, history.HistoryTurns)
	require.Equal(t, "t3", history.CurrentTurnID)
	_, err = ParseAIRoutingGeminiHistory(body, AIRoutingContextLimits{MaxHistoryTurns: 2})
	aiHistoryTestReason(t, err, "history_turn_limit")
	_, err = ParseAIRoutingGeminiHistory(body, AIRoutingContextLimits{MaxHistoryBytes: 1})
	aiHistoryTestReason(t, err, "history_byte_limit")
}
