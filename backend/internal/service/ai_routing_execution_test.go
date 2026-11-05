//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestAIRoutingRequestDisclosureAndToolProvenance(t *testing.T) {
	body := []byte(`{"model":"any-client-name","system":"system-secret","max_tokens":100,"tools":[{"name":"shell","description":"tool-schema-secret"}],"messages":[{"role":"user","content":"original task"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","input":{"password":"tool-input-secret"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"tool-output-secret"},{"type":"image","source":{"data":"attachment-secret"}}]},{"type":"text","text":"review sk-abcdefghijklmopqrstuvwxyz and password=secretvalue"}]},{"role":"user","content":"continue"},{"role":"assistant","content":"prefill"}]}`)
	metadata, err := NewAIRoutingRequest(body, "metadata")
	require.NoError(t, err)
	require.True(t, metadata.UsesTools)
	require.True(t, metadata.HasImages)
	require.Equal(t, []string{"t1"}, metadata.ToolResults)
	require.Equal(t, "any-client-name", metadata.PublicModel)
	require.NotContains(t, metadata.Task, "continue")
	excerpt, err := NewAIRoutingRequest(body, "task_excerpt")
	require.NoError(t, err)
	require.Contains(t, excerpt.Task, "continue")
	for _, secret := range []string{"system-secret", "tool-schema-secret", "tool-input-secret", "tool-output-secret", "attachment-secret"} {
		require.NotContains(t, excerpt.Task, secret)
	}
	redacted := boundedTask("review sk-abcdefghijklmopqrstuvwxyz password=secretvalue")
	require.NotContains(t, redacted, "secretvalue")
	require.NotContains(t, redacted, "abcdefghijk")
	bounded := boundedTask(strings.Repeat("中文", 1500))
	require.True(t, utf8.ValidString(bounded))
	require.LessOrEqual(t, len(bounded), 2048)
	require.Equal(t, "[redacted]", boundedTask(strings.Repeat("x", 2100)+"-----BEGIN PRIVATE KEY-----"))
}

func TestAIRoutingRequestRejectsMalformedInput(t *testing.T) {
	for _, body := range []string{
		`{"model":"a","model":"b","messages":[{"role":"user","content":"x"}]}`,
		`{"model":12,"messages":[]}`,
		`{"model":"a","messages":[{"role":"system","content":"x"}]}`,
		`{"model":"a","messages":[{"role":"assistant","content":[{"type":"tool_result","tool_use_id":"x"}]}]}`,
		`{"model":"a","messages":[{"role":"user","content":[{"type":"tool_result"}]}]}`,
		`{"model":"a","messages":{}}`,
		`{"model":"a","messages":[{"role":"user","role":"assistant","content":"x"}]}`,
		`{"model":"a","messages":[{"role":"user","Role":"assistant","content":"x"}]}`,
		`{"model":"a","messages":[{"role":"user","content":[{"type":"text","type":"tool_result","text":"hi","tool_use_id":"unknown","content":"x"}]}]}`,
		`{"model":"a","messages":[{"role":"user","content":[{"type":"text","Type":"tool_result","text":"hi","tool_use_id":"unknown","content":"x"}]}]}`,
		`{"model":"a","messages":[{"role":"user","content":[{"type":"text","text":"safe","te\u0078t":"secret"}]}]}`,
		`{"model":"a","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","Type":"image","text":"x"}]}]}]}`,
		`{"model":"a","max_tokens":1,"MAX_TOKENS":1000,"messages":[{"role":"user","content":"x"}]}`,
	} {
		_, err := NewAIRoutingRequest([]byte(body), "metadata")
		require.Error(t, err, body)
	}
}

func TestAIRoutingTaskExcerptCombinesCurrentUserTurn(t *testing.T) {
	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"old task"},{"role":"assistant","content":"assistant secret"},{"role":"user","content":[{"type":"text","text":"Review transaction isolation"},{"type":"text","text":"password=supersecret"},{"type":"tool_result","tool_use_id":"tool-1","content":"tool secret"}]},{"role":"user","content":"Use Chinese"},{"role":"assistant","content":"prefill secret"}]}`)
	r, err := NewAIRoutingRequest(body, "task_excerpt")
	require.NoError(t, err)
	var task map[string]any
	require.NoError(t, json.Unmarshal([]byte(r.Task), &task))
	require.Equal(t, "Review transaction isolation\n[redacted]\nUse Chinese", task["user_task"])
	for _, secret := range []string{"old task", "assistant secret", "supersecret", "tool secret", "prefill secret"} {
		require.NotContains(t, r.Task, secret)
	}
	r, err = NewAIRoutingRequest(body, "metadata")
	require.NoError(t, err)
	require.NotContains(t, r.Task, "user_task")
	var joined strings.Builder
	appendAITaskText(&joined, strings.Repeat("中文", 400))
	appendAITaskText(&joined, "ignored")
	require.LessOrEqual(t, joined.Len(), 2048)
	require.True(t, utf8.ValidString(joined.String()))
}

type aiChoiceStub struct {
	calls int
	id    string
	err   error
}

func (s *aiChoiceStub) Decide(context.Context, *AIRoutingPolicy, string, []AIRoutingCandidate, string) (AIRoutingDecisionResult, error) {
	s.calls++
	return AIRoutingDecisionResult{CandidateID: s.id}, s.err
}
func TestAIRoutingFiniteChoiceAndFallback(t *testing.T) {
	p := &AIRoutingPolicy{FallbackID: "b"}
	candidates := []AIRoutingCandidate{{ID: "a"}, {ID: "b"}}
	chooser := &aiChoiceStub{id: "a"}
	c, _, source, err := ChooseAIRoutingCandidate(context.Background(), p, AIRoutingRequest{}, nil, chooser, "")
	require.ErrorIs(t, err, ErrNoAIRoutingCandidate)
	require.Empty(t, c.ID)
	require.Equal(t, 0, chooser.calls)
	c, _, source, err = ChooseAIRoutingCandidate(context.Background(), p, AIRoutingRequest{}, candidates[:1], chooser, "")
	require.NoError(t, err)
	require.Equal(t, "single", source)
	require.Equal(t, "a", c.ID)
	require.Equal(t, 0, chooser.calls)
	c, _, source, err = ChooseAIRoutingCandidate(context.Background(), p, AIRoutingRequest{}, candidates, chooser, "")
	require.NoError(t, err)
	require.Equal(t, "model", source)
	require.Equal(t, "a", c.ID)
	chooser.id = "outside"
	c, _, source, err = ChooseAIRoutingCandidate(context.Background(), p, AIRoutingRequest{}, candidates, chooser, "")
	require.NoError(t, err)
	require.Equal(t, "fallback", source)
	require.Equal(t, "b", c.ID)
	c, _, source, err = ChooseAIRoutingCandidate(context.Background(), p, AIRoutingRequest{}, candidates, nil, "")
	require.NoError(t, err)
	require.Equal(t, "fallback", source)
	require.Equal(t, "b", c.ID)
	p.FallbackID = "revoked"
	_, _, _, err = ChooseAIRoutingCandidate(context.Background(), p, AIRoutingRequest{}, candidates, chooser, "")
	require.Error(t, err)
	p.FallbackID = "a"
	chooser.err = errors.New("upstream failed")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, err = ChooseAIRoutingCandidate(ctx, p, AIRoutingRequest{}, candidates, chooser, "")
	require.ErrorIs(t, err, context.Canceled)
}

func TestAIRoutingEligibilityRechecksModelGrantAndCurrentAccounts(t *testing.T) {
	repo, groups := scopeIsolationFixtures()
	source := groups.groups[10]
	source.AllowMessagesDispatch = true
	target := int64(20)
	c := AIRoutingCandidate{ID: "a", Platform: PlatformOpenAI, Model: "gpt-5.1", Enabled: true, TargetGroupID: &target}
	p := &AIRoutingPolicy{GroupID: source.ID, Candidates: []AIRoutingCandidate{c}}
	key := &APIKey{GroupID: &source.ID, Group: source}
	gateway := &GatewayService{accountRepo: repo, groupRepo: groups, billingService: &BillingService{fallbackPrices: map[string]*ModelPricing{"gpt-5.1": {}}}}
	eligible := func(r AIRoutingRequest, bound int64) []AIRoutingCandidate {
		return gateway.EligibleAIRoutingCandidates(context.Background(), key, p, r, CompositeRouteEndpointMessages, bound)
	}
	require.Len(t, eligible(AIRoutingRequest{}, 0), 1)
	require.Empty(t, eligible(AIRoutingRequest{}, 1), "bound source account cannot replace destination account")
	source.AllowMessagesDispatch = false
	require.Empty(t, eligible(AIRoutingRequest{}, 0))
	source.AllowMessagesDispatch = true
	original, err := repo.GetByID(context.Background(), 2)
	require.NoError(t, err)
	changed := *original
	changed.Credentials = map[string]any{"model_mapping": map[string]any{"gpt-5.1": "gpt-5.2"}}
	repo.batchAccounts = []*Account{&changed}
	require.Empty(t, eligible(AIRoutingRequest{}, 0), "account remapping must not change priced execution model")
	changed.Credentials = nil
	changed.AccountGroups = nil
	require.Empty(t, eligible(AIRoutingRequest{}, 0), "stale list membership must be rechecked")
	repo.batchAccounts = nil
	p.Candidates[0].DisableTools = true
	require.Empty(t, eligible(AIRoutingRequest{UsesTools: true}, 0))
	p.Candidates[0].DisableImages = true
	require.Empty(t, eligible(AIRoutingRequest{HasImages: true}, 0))
	p.Candidates[0].Model = "unpriced-model"
	require.Empty(t, eligible(AIRoutingRequest{}, 0))
	p.Candidates[0].Model = "gpt-image-2"
	gateway.billingService.fallbackPrices["gpt-image-2"] = &ModelPricing{}
	require.Empty(t, eligible(AIRoutingRequest{}, 0), "image models cannot enter a Messages execution pool")
}

type aiBalanceCache struct {
	BillingCache
	balance float64
}

func (c *aiBalanceCache) GetUserBalance(context.Context, int64) (float64, error) {
	return c.balance, nil
}

func TestAIRoutingDecisionAdmissionCountsRPMOnceAndRechecksBalance(t *testing.T) {
	cache := &aiBalanceCache{balance: 5}
	rpm := &userRPMCacheStub{}
	svc := &BillingCacheService{cache: cache, userRPMCache: rpm, cfg: &config.Config{}}
	user := &User{ID: 1, RPMLimit: 10}
	group := &Group{ID: 10}
	key := &APIKey{ID: 5, Group: group, User: user}
	ctx, err := svc.AdmitAIRoutingDecision(context.Background(), user, key, group, nil)
	require.NoError(t, err)
	require.EqualValues(t, 1, rpm.userCalls)
	require.NoError(t, svc.CheckBillingEligibility(ctx, user, key, group, nil, ""))
	require.EqualValues(t, 1, rpm.userCalls)
	cache.balance = -1
	require.ErrorIs(t, svc.CheckBillingEligibility(ctx, user, key, group, nil, ""), ErrInsufficientBalance)
	cache.balance = 5
	other := *key
	other.ID++
	require.NoError(t, svc.CheckBillingEligibility(ctx, user, &other, group, nil, ""))
	require.EqualValues(t, 2, rpm.userCalls, "admission cannot be transferred to a different key")
}
