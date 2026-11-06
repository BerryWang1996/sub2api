//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type aiHistoryChooserFunc func(context.Context, *AIRoutingPolicy, string, []AIRoutingCandidate, string) (AIRoutingDecisionResult, error)

func (f aiHistoryChooserFunc) Decide(ctx context.Context, p *AIRoutingPolicy, task string, candidates []AIRoutingCandidate, host string) (AIRoutingDecisionResult, error) {
	return f(ctx, p, task, candidates, host)
}

func aiHistoryRuntimeRequest(t *testing.T, messages []map[string]string) AIRoutingRequest {
	t.Helper()
	body, err := json.Marshal(map[string]any{"model": "public-random-alias", "max_tokens": 512, "system": "system-not-disclosed", "messages": messages})
	require.NoError(t, err)
	request, err := NewAIRoutingRequest(body, "bounded_history")
	require.NoError(t, err)
	return request
}

func TestAIRoutingHistoryRuntimeUsesConversationAndStableCandidates(t *testing.T) {
	for _, engine := range []string{"generic", "jev"} {
		t.Run(engine, func(t *testing.T) {
			platform, model := PlatformOpenAI, "custom-decider"
			if engine == "jev" {
				platform, model = PlatformTypeSafe, "jev-1.14.0"
			}
			account := &Account{ID: 41, Platform: platform, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"api_key": "fake", "model_mapping": map[string]any{model: model}}}
			policy := aiTestDecisionPolicy(platform, engine, model)
			policy.ContextMode = "bounded_history"
			calls := 0
			decider, _ := aiTestDecider(account, func(req *http.Request) (*http.Response, error) {
				calls++
				raw, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				wire := string(raw)
				for _, text := range []string{"translate the release notes", "second option is literal translation", "continue with the second option", "stable_id"} {
					require.Contains(t, wire, text)
				}
				for _, secret := range []string{"system-not-disclosed", "secretvalue"} {
					require.NotContains(t, wire, secret)
				}
				response := `{"status":"completed","model":"custom-decider-v1","output":[{"type":"message","content":[{"type":"output_text","text":"c2"}]}],"usage":{"input_tokens":12,"output_tokens":1}}`
				if engine == "jev" {
					response = `{"model":"jev-1.14.0","answers":{"route":{"type":"choice","choice":"c2"}},"usage":{"input_tokens":12,"output_tokens":1}}`
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(response))}, nil
			})
			decider.gateway.groupRepo = &aiDecisionGroupStore{groups: map[int64]*Group{policy.GroupID: {ID: policy.GroupID, AIRouting: true, Platform: PlatformComposite, Status: StatusActive}}}
			request := aiHistoryRuntimeRequest(t, []map[string]string{{"role": "user", "content": "translate the release notes password=secretvalue"}, {"role": "assistant", "content": "second option is literal translation"}, {"role": "user", "content": "continue with the second option"}})
			candidates := aiTestDecisionCandidates()
			candidates[0], candidates[1] = candidates[1], candidates[0]
			chosen, result, source, err := ChooseAIRoutingCandidate(context.Background(), policy, request, candidates, decider, "app.invalid")
			require.NoError(t, err)
			require.Equal(t, "a", chosen.ID)
			require.Equal(t, "model", source)
			require.Equal(t, 1, calls)
			require.Equal(t, 1, result.DecisionCalls)
			require.Len(t, result.Calls, 1)
			require.True(t, result.Calls[0].UsageKnown)
			require.Equal(t, 12, result.InputTokens)
			require.Equal(t, "complete", result.Context.Coverage)
			trace, err := json.Marshal(AIRoutingTrace{Context: result.Context, Stages: result.Calls})
			require.NoError(t, err)
			require.NotContains(t, string(trace), "translate the release notes")
		})
	}
}

func TestAIRoutingHistoryRuntimeScansBeforeFinalAndAggregates(t *testing.T) {
	p := NewAIRoutingDraft(57)
	p.Prompt = "translation goes to a; concurrency goes to b"
	var messages []map[string]string
	for i := 0; i < 30; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		text := strings.Repeat("unrelated archive text ", 25)
		if i == 14 {
			text = "needle: diagnose duplicate payment and lock inversion"
		}
		messages = append(messages, map[string]string{"role": role, "content": text})
	}
	messages = append(messages, map[string]string{"role": "user", "content": "continue the payment investigation"})
	request := aiHistoryRuntimeRequest(t, messages)
	scans, finals := 0, 0
	chooser := aiHistoryChooserFunc(func(ctx context.Context, policy *AIRoutingPolicy, task string, candidates []AIRoutingCandidate, host string) (AIRoutingDecisionResult, error) {
		result := AIRoutingDecisionResult{Model: "fake", Protocol: "fake", AccountID: 1, InputTokens: 10, OutputTokens: 1, Attempted: true, DecisionCalls: 1, UsageKnown: true}
		if gjson.Get(task, "kind").String() == "history_relevance" {
			scans++
			result.CandidateID = "irrelevant"
			for _, span := range gjson.Get(task, "spans").Array() {
				if strings.Contains(span.Get("text").String(), "needle:") {
					result.CandidateID = "relevant"
				}
			}
		} else {
			finals++
			require.Positive(t, scans)
			require.Contains(t, task, "needle:")
			require.Contains(t, task, "continue the payment investigation")
			result.CandidateID = "b"
		}
		return result, nil
	})
	chosen, result, _, err := ChooseAIRoutingCandidate(context.Background(), &p, request, aiTestDecisionCandidates(), chooser, "")
	require.NoError(t, err)
	require.Equal(t, "b", chosen.ID)
	require.Positive(t, scans)
	require.Equal(t, 1, finals)
	require.Equal(t, scans+1, result.DecisionCalls)
	require.Equal(t, 10*(scans+1), result.InputTokens)
	require.Equal(t, result.Context.Segments, result.Context.ScannedSegments)
	require.Equal(t, "complete", result.Context.Coverage)
}

func TestAIRoutingHistoryRuntimeAbstentionCancellationAndRevocation(t *testing.T) {
	p := NewAIRoutingDraft(57)
	request := aiHistoryRuntimeRequest(t, []map[string]string{{"role": "user", "content": "continue"}})
	for _, control := range []string{AIRoutingControlInsufficient, AIRoutingControlAmbiguous} {
		chooser := &aiChoiceStub{id: control}
		chosen, result, source, err := ChooseAIRoutingCandidate(context.Background(), &p, request, aiTestDecisionCandidates(), chooser, "")
		require.Error(t, err)
		require.Empty(t, chosen.ID)
		require.Equal(t, "failed", source)
		require.Equal(t, strings.TrimPrefix(control, "__"), result.FailureReason)
		p.FallbackID = "a"
		chosen, _, source, err = ChooseAIRoutingCandidate(context.Background(), &p, request, aiTestDecisionCandidates(), chooser, "")
		require.NoError(t, err)
		require.Equal(t, "a", chosen.ID)
		require.Equal(t, "fallback", source)
		p.FallbackID = ""
	}
	p.FallbackID = "a"
	chooser := &aiChoiceStub{err: ErrAIRoutingDecisionRevoked}
	chosen, _, source, err := ChooseAIRoutingCandidate(context.Background(), &p, request, aiTestDecisionCandidates(), chooser, "")
	require.ErrorIs(t, err, ErrAIRoutingDecisionRevoked)
	require.Empty(t, chosen.ID)
	require.Equal(t, "rejected", source)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, err = ChooseAIRoutingCandidate(ctx, &p, request, aiTestDecisionCandidates()[:1], chooser, "")
	require.ErrorIs(t, err, context.Canceled)
	before := chooser.calls
	chosen, _, source, err = ChooseAIRoutingCandidate(context.Background(), &p, AIRoutingRequest{}, aiTestDecisionCandidates()[:1], chooser, "")
	require.NoError(t, err)
	require.Equal(t, "single", source)
	require.Equal(t, "a", chosen.ID)
	require.Equal(t, before, chooser.calls)
}

func TestAIRoutingHistoryTransportBudgetReservesFinal(t *testing.T) {
	p := NewAIRoutingDraft(1)
	p.ContextBudget.MaxDecisionCalls = 2
	limits, err := NormalizeAIRoutingContextLimits(p.ContextBudget)
	require.NoError(t, err)
	ctx := context.WithValue(context.Background(), aiDecisionBudgetKey{}, &aiDecisionBudget{limits: limits})
	account := &Account{}
	require.NoError(t, reserveAIRoutingDecisionAttempt(ctx, &p, account, "custom", []byte("{}")))
	require.Error(t, reserveAIRoutingDecisionAttempt(ctx, &p, account, "custom", []byte("{}")))
	require.NoError(t, reserveAIRoutingDecisionAttempt(context.WithValue(ctx, aiDecisionStageKey{}, "final"), &p, account, "custom", []byte("{}")))
	require.Error(t, reserveAIRoutingDecisionAttempt(context.WithValue(ctx, aiDecisionStageKey{}, "final"), &p, account, "custom", []byte("{}")))
	revoked := WithAIRoutingDecisionGuard(context.Background(), func(context.Context) error { return errors.New("revoked") })
	require.ErrorIs(t, reserveAIRoutingDecisionAttempt(revoked, &p, account, "custom", []byte("{}")), ErrAIRoutingDecisionRevoked)
}

func TestAIRoutingHistoryOutputBudgetReservesFinalAndBoundsReads(t *testing.T) {
	t.Run("reserve final response", func(t *testing.T) {
		p := NewAIRoutingDraft(1)
		p.ContextBudget.MaxOutputBytes = 1024
		p.ContextBudget.MaxTotalOutputBytes = 2048
		limits, err := NormalizeAIRoutingContextLimits(p.ContextBudget)
		require.NoError(t, err)
		ctx := context.WithValue(context.Background(), aiDecisionBudgetKey{}, &aiDecisionBudget{limits: limits})
		require.NoError(t, reserveAIRoutingDecisionAttempt(ctx, &p, &Account{}, "custom", []byte("{}")))
		require.NoError(t, recordAIRoutingDecisionOutput(ctx, &p, 1024))
		require.ErrorContains(t, reserveAIRoutingDecisionAttempt(ctx, &p, &Account{}, "custom", []byte("{}")), "history_output_budget")
		final := context.WithValue(ctx, aiDecisionStageKey{}, "final")
		require.NoError(t, reserveAIRoutingDecisionAttempt(final, &p, &Account{}, "custom", []byte("{}")))
		require.ErrorContains(t, recordAIRoutingDecisionOutput(final, &p, 1025), "history_output_budget")
	})
	t.Run("oversized actual response", func(t *testing.T) {
		account := &Account{ID: 41, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"api_key": "fake"}}
		policy := aiTestDecisionPolicy(PlatformOpenAI, "generic", "custom-decider")
		policy.ContextMode = "bounded_history"
		policy.ContextBudget.MaxOutputBytes = 1024
		response := strings.NewReader(strings.Repeat(" ", 65536))
		calls := 0
		decider, _ := aiTestDecider(account, func(_ *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 200, Body: io.NopCloser(response)}, nil
		})
		decider.gateway.groupRepo = &aiDecisionGroupStore{groups: map[int64]*Group{policy.GroupID: {ID: policy.GroupID, AIRouting: true, Platform: PlatformComposite, Status: StatusActive}}}
		request := aiHistoryRuntimeRequest(t, []map[string]string{{"role": "user", "content": "translate this"}})
		chosen, result, _, err := ChooseAIRoutingCandidate(context.Background(), policy, request, aiTestDecisionCandidates(), decider, "app.invalid")
		require.Error(t, err)
		require.Empty(t, chosen.ID)
		require.Equal(t, 1, calls)
		require.Equal(t, "history_output_budget", result.FailureReason)
		require.Equal(t, 1025, result.OutputBytes)
		require.Equal(t, 65536-1025, response.Len())
		require.False(t, result.Calls[0].UsageKnown)
	})
}

func TestAIRoutingHistoryLateFinalChoiceCannotExecute(t *testing.T) {
	p := NewAIRoutingDraft(57)
	p.TimeoutMS = 5
	request := aiHistoryRuntimeRequest(t, []map[string]string{{"role": "user", "content": "translate this"}})
	chooser := aiHistoryChooserFunc(func(ctx context.Context, _ *AIRoutingPolicy, _ string, _ []AIRoutingCandidate, _ string) (AIRoutingDecisionResult, error) {
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Fatal("overall deadline not propagated")
		}
		return AIRoutingDecisionResult{CandidateID: "a"}, nil
	})
	chosen, result, _, err := ChooseAIRoutingCandidate(context.Background(), &p, request, aiTestDecisionCandidates(), chooser, "")
	require.Error(t, err)
	require.Empty(t, chosen.ID)
	require.Equal(t, "timeout", result.FailureReason)
	require.Equal(t, result.FailureReason, result.Context.FailureReason)
}
