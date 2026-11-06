//go:build unit

package service

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAIRoutingChatHistoryNativeVisibleProjectionAndStableSources(t *testing.T) {
	body := aiChatTestBody(t, []any{
		map[string]any{"role": "system", "content": "private-system"},
		map[string]any{"role": "developer", "content": "private-developer"},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "先前需求"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://synthetic.invalid/private-image"}}}},
		map[string]any{"role": "assistant", "content": "second option", "reasoning_content": "private-thinking"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{aiChatTestCall("call_x", `{"password":"private-argument"}`)}},
		map[string]any{"role": "tool", "tool_call_id": "call_x", "content": "private-result"},
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "refusal", "refusal": "visible refusal"}}},
		map[string]any{"role": "user", "content": "  继续第二项 password=private-password  "},
	}, nil)
	before := append([]byte(nil), body...)
	history, err := ParseAIRoutingChatHistory(body, AIRoutingContextLimits{})
	require.NoError(t, err)
	require.Equal(t, before, body)
	require.Equal(t, AIRoutingChatHistoryProjectionVersion, history.ProjectionVersion)
	require.Len(t, history.Spans, 4)
	require.Equal(t, "c3b0", history.Spans[0].SourceID)
	require.Equal(t, "c4b0", history.Spans[1].SourceID)
	require.Equal(t, "c7b0", history.Spans[2].SourceID)
	require.Equal(t, "c8b0", history.Spans[3].SourceID)
	require.Equal(t, "c3b0:0-12", history.Spans[0].ID)
	require.Equal(t, history.CurrentTurnID, history.Spans[3].TurnID)
	require.Equal(t, 1, history.RedactedSpans)
	require.Equal(t, 6, history.ExcludedBlocks)
	prepared, err := BuildAIRoutingHistoryTask(context.Background(), history, AIRoutingContextLimits{}, 100, nil)
	require.NoError(t, err)
	require.Equal(t, AIRoutingChatHistoryProjectionVersion, prepared.Diagnostics.ProjectionVersion)
	require.Equal(t, AIRoutingChatHistoryProjectionVersion, aiHistoryTestDecode(t, prepared.Task).ProjectionVersion)
	for _, secret := range []string{"private-system", "private-developer", "private-image", "private-thinking", "private-argument", "private-result", "private-password", "call_x", "same_function"} {
		require.NotContains(t, prepared.Task, secret)
	}
	repeated, err := ParseAIRoutingChatHistory(body, AIRoutingContextLimits{})
	require.NoError(t, err)
	require.Equal(t, history, repeated)
}

func TestAIRoutingChatHistoryHiddenActionsAndBudgets(t *testing.T) {
	body := aiChatTestBody(t, []any{
		map[string]any{"role": "user", "content": "old task"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{aiChatTestCall("call_x", "{}")}},
		map[string]any{"role": "tool", "tool_call_id": "call_x", "content": strings.Repeat("private-noise", 10000)},
		map[string]any{"role": "user", "content": "current task"},
	}, nil)
	history, err := ParseAIRoutingChatHistory(body, AIRoutingContextLimits{})
	require.NoError(t, err)
	require.Equal(t, 3, history.HistoryTurns)
	require.Equal(t, "t3", history.CurrentTurnID)
	require.Equal(t, len("old taskcurrent task"), history.HistoryBytes)
	require.NotEqual(t, history.Spans[0].TurnID, history.Spans[1].TurnID)
	r, err := NewAIRoutingChatRequest(body, "task_excerpt")
	require.NoError(t, err)
	require.Contains(t, r.Task, "current task")
	require.NotContains(t, r.Task, "old task")
	require.NotContains(t, r.Task, "private-noise")
	_, err = ParseAIRoutingChatHistory(body, AIRoutingContextLimits{MaxHistoryTurns: 2})
	aiHistoryTestReason(t, err, "history_turn_limit")
	_, err = ParseAIRoutingChatHistory(body, AIRoutingContextLimits{MaxHistoryBytes: 1})
	aiHistoryTestReason(t, err, "history_byte_limit")
	_, err = ParseAIRoutingChatHistory([]byte(`{"model":"x","messages":[{"role":"user","Content":"bad"}]}`), AIRoutingContextLimits{})
	aiHistoryTestReason(t, err, "history_invalid_request")
}

func TestAIRoutingChatHistorySameTailRetainsDifferentEarlierEvidence(t *testing.T) {
	var tasks []string
	for _, material := range []string{"translation requirement", "mathematical proof requirement"} {
		body := aiChatTestBody(t, []any{
			map[string]any{"role": "user", "content": material},
			map[string]any{"role": "assistant", "content": "second option follows your requirement"},
			map[string]any{"role": "user", "content": "继续第二项"},
		}, nil)
		history, err := ParseAIRoutingChatHistory(body, AIRoutingContextLimits{})
		require.NoError(t, err)
		prepared, err := BuildAIRoutingHistoryTask(context.Background(), history, AIRoutingContextLimits{}, 100, nil)
		require.NoError(t, err)
		require.Contains(t, prepared.Task, material)
		require.Contains(t, prepared.Task, "继续第二项")
		tasks = append(tasks, prepared.Task)
	}
	require.NotEqual(t, tasks[0], tasks[1], "projection must retain evidence; this does not assert model semantic accuracy")
}
