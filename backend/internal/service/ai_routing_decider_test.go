//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/typesafe"
	"github.com/stretchr/testify/require"
)

func aiTestDecisionPolicy(platform, engine, model string) *AIRoutingPolicy {
	id := int64(41)
	return &AIRoutingPolicy{GroupID: 57, Engine: engine, DecisionAccountID: &id, DecisionPlatform: platform, DecisionModel: model, Prompt: "route by complexity", TimeoutMS: 4000}
}
func aiTestDecisionCandidates() []AIRoutingCandidate {
	return []AIRoutingCandidate{{ID: "a", Model: "same-model", Description: "formatting", Enabled: true}, {ID: "b", Model: "same-model", Description: "analysis", Enabled: true}}
}
func aiTestDecider(account *Account, do func(*http.Request) (*http.Response, error)) (*AIRoutingDecider, *stubConcurrencyCacheForTest) {
	cache := &stubConcurrencyCacheForTest{acquireResult: true}
	gateway := newSystemOneTestService(&systemOneHTTPUpstream{do: do})
	gateway.accountRepo = &systemOnePolicyAccountRepo{account: account}
	gateway.concurrencyService = NewConcurrencyService(cache)
	return NewAIRoutingDecider(gateway, &OpenAIGatewayService{}), cache
}
func TestAIRoutingDeciderNativeAndGeneric(t *testing.T) {
	for _, tc := range []struct {
		name, platform, credential, model, engine, path, secret, response string
		chat                                                              bool
	}{
		{"jev", PlatformTypeSafe, AccountTypeAPIKey, "jev-latest", "jev", "/v1/systemone", "Authorization", `{"answers":{"route":{"type":"choice","choice":"c2"}},"usage":{"input_tokens":12,"output_tokens":1}}`, false},
		{"anthropic", PlatformAnthropic, AccountTypeAPIKey, "custom-decider", "generic", "/v1/messages", "x-api-key", `{"stop_reason":"end_turn","content":[{"type":"text","text":"c2"}],"usage":{"input_tokens":12,"output_tokens":1}}`, false},
		{"claude-subscription", PlatformAnthropic, AccountTypeOAuth, "claude-sonnet-5-5", "generic", "/v1/messages", "Authorization", `{"stop_reason":"end_turn","content":[{"type":"text","text":"c2"}],"usage":{"input_tokens":12,"output_tokens":1}}`, false},
		{"openai-responses", PlatformOpenAI, AccountTypeAPIKey, "custom-decider", "generic", "/v1/responses", "Authorization", `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"candidate_id\":\"c2\"}"}]}],"usage":{"input_tokens":12,"output_tokens":1}}`, false},
		{"openai-chat", PlatformOpenAI, AccountTypeAPIKey, "custom-decider", "generic", "/v1/chat/completions", "Authorization", `{"choices":[{"finish_reason":"stop","message":{"content":"c2"}}],"usage":{"prompt_tokens":12,"completion_tokens":1}}`, true},
		{"gemini", PlatformGemini, AccountTypeAPIKey, "custom-decider", "generic", "/v1beta/models/custom-decider:generateContent", "x-goog-api-key", `{"candidates":[{"finishReason":"STOP","content":{"parts":[{"thought":true,"text":"private thinking"},{"text":"c2"}]}}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":1}}`, false},
		{"codex-subscription", PlatformOpenAI, AccountTypeOAuth, "gpt-6.1-sol", "generic", "/backend-api/codex/responses", "Authorization", "data: " + `{"type":"response.output_text.delta","delta":"c2"}` + "\n\ndata: " + `{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"c2"}]}],"usage":{"input_tokens":12,"output_tokens":1}}}` + "\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			credentials := map[string]any{"api_key": "admin-key", "access_token": "admin-token", "model_mapping": map[string]any{tc.model: tc.model}}
			account := &Account{ID: 41, Platform: tc.platform, Type: tc.credential, Credentials: credentials, Status: StatusActive, Schedulable: true, Concurrency: 1}
			if tc.chat {
				account.Extra = map[string]any{"openai_responses_supported": false}
			}
			policy := aiTestDecisionPolicy(tc.platform, tc.engine, tc.model)
			decider, cache := aiTestDecider(account, func(req *http.Request) (*http.Response, error) {
				require.Equal(t, tc.path, req.URL.Path)
				require.NotEmpty(t, req.Header.Get(tc.secret))
				require.True(t, HTTPUpstreamRedirectsDisabled(req.Context()))
				require.Equal(t, "1", req.Header.Get(AIRoutingInternalHeader))
				data, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				require.NotContains(t, string(data), `"tools"`)
				require.NotContains(t, string(data), "admin-token")
				require.Contains(t, string(data), "admin")
				require.Contains(t, string(data), "analysis")
				if tc.engine == "jev" {
					_, err = typesafe.ValidateSystemOneRequest(data)
					require.NoError(t, err)
				}
				if tc.name == "codex-subscription" {
					require.Contains(t, string(data), `"store":false`)
					require.Contains(t, string(data), `"stream":true`)
					require.NotEmpty(t, req.Header.Get("Originator"))
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(tc.response))}, nil
			})
			r, err := decider.Decide(context.Background(), policy, "admin test task", aiTestDecisionCandidates(), "sub2api.invalid")
			require.NoError(t, err)
			require.Equal(t, "b", r.CandidateID)
			require.Equal(t, 12, r.InputTokens)
			require.Equal(t, 1, r.OutputTokens)
			require.Equal(t, []int64{41}, cache.releasedAccountIDs)
		})
	}
}

func TestAIRoutingDeciderRejectsUntrustedAndTruncatedAnswers(t *testing.T) {
	ids := map[string]string{"c1": "safe"}
	for _, bad := range []string{"c12", "c1 or c2", "choose c1", `{"candidate_id":"c2","candidate_id":"c1"}`, `{"candidate_id":"c1","name":"steer"}`, `{"candidate_id":"c1"}junk`} {
		_, err := parseAIRoutingSelection(bad, ids)
		require.Error(t, err)
	}
	for _, tc := range []struct {
		proto, raw string
		stream     bool
	}{
		{APIProtocolChatCompletions, `{"choices":[{"finish_reason":"length","message":{"content":"c1"}}]}`, false},
		{APIProtocolChatCompletions, `{"choices":[{"finish_reason":"stop","message":{"content":"c1","tool_calls":[{}]}}]}`, false},
		{APIProtocolResponses, `{"status":"completed","output":[{"type":"function_call"},{"type":"message","content":[{"type":"output_text","text":"c1"}]}]}`, false},
		{APIProtocolResponses, "data: " + `{"type":"response.output_text.delta","delta":"c1"}` + "\n\ndata: [DONE]\n", true},
		{APIProtocolAnthropic, `{"stop_reason":"tool_use","content":[{"type":"text","text":"c1"}]}`, false},
		{"systemone", `{"answers":{"route":{"type":"noul","noul":0.5}}}`, false},
		{"systemone", `{"answers":{"route":{"type":"choice","choice":"{\"candidate_id\":\"c1\"}"}}}`, false},
	} {
		_, _, _, err := decodeAIRoutingResponse(tc.proto, []byte(tc.raw), tc.stream)
		require.Error(t, err)
	}
}

func TestAIRoutingDeciderCustomEndpointCancellationAndRelease(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/test/v1/responses", r.URL.Path)
		var payload map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		require.Equal(t, "internal/model", payload["model"])
		w.WriteHeader(429)
	}))
	defer server.Close()
	account := &Account{ID: 41, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"base_url": server.URL + "/test/v1", "api_key": "placeholder", "model_mapping": map[string]any{"alias": "internal/model"}}}
	p := aiTestDecisionPolicy(PlatformOpenAI, "generic", "alias")
	d, cache := aiTestDecider(account, server.Client().Do)
	r, err := d.Decide(context.Background(), p, "metadata", aiTestDecisionCandidates(), "app.example")
	require.ErrorContains(t, err, "429")
	require.Equal(t, "internal/model", r.Model)
	require.Len(t, cache.releasedAccountIDs, 1)
	_, err = d.Decide(context.Background(), p, "metadata", aiTestDecisionCandidates(), server.URL)
	require.ErrorContains(t, err, "gateway")
	require.Len(t, cache.releasedAccountIDs, 2)
	d.gateway.httpUpstream = &systemOneHTTPUpstream{do: func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}}
	p.TimeoutMS = 5
	_, err = d.Decide(context.Background(), p, "metadata", aiTestDecisionCandidates(), "app.example")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Len(t, cache.releasedAccountIDs, 3)
	cache.acquireResult = false
	_, err = d.Decide(context.Background(), p, "metadata", aiTestDecisionCandidates(), "app.example")
	require.Error(t, err)
	require.False(t, errors.Is(err, context.DeadlineExceeded))
	require.Len(t, cache.releasedAccountIDs, 3)
}

type aiDecisionGroupStore struct {
	GroupRepository
	groups map[int64]*Group
}

func (s *aiDecisionGroupStore) GetByIDLite(_ context.Context, id int64) (*Group, error) {
	return s.groups[id], nil
}

type aiDecisionGroupAccounts struct {
	AccountRepository
	list    []Account
	current map[int64]*Account
}

func (s *aiDecisionGroupAccounts) ListSchedulableByGroupIDAndPlatform(context.Context, int64, string) ([]Account, error) {
	return append([]Account(nil), s.list...), nil
}
func (s *aiDecisionGroupAccounts) GetByID(_ context.Context, id int64) (*Account, error) {
	return s.current[id], nil
}

func TestAIRoutingDeciderRechecksMembershipAndSourcePrivacy(t *testing.T) {
	gid := int64(22)
	acct := Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"api_key": "fake"}, AccountGroups: []AccountGroup{{GroupID: gid}}}
	other := acct
	other.ID = 2
	other.Extra = map[string]any{"privacy_mode": PrivacyModeTrainingOff}
	detached := other
	detached.AccountGroups = nil
	repo := &aiDecisionGroupAccounts{list: []Account{acct, other}, current: map[int64]*Account{1: &acct, 2: &detached}}
	p := aiTestDecisionPolicy(PlatformOpenAI, "generic", "custom-model")
	p.ContextMode = "task_excerpt"
	p.DecisionAccountID = nil
	p.DecisionGroupID = &gid
	d, cache := aiTestDecider(&acct, func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"c1"}]}]}`))}, nil
	})
	d.gateway.accountRepo = repo
	d.gateway.groupRepo = &aiDecisionGroupStore{groups: map[int64]*Group{
		p.GroupID: {ID: p.GroupID, Platform: PlatformComposite, AIRouting: true, Status: StatusActive, RequirePrivacySet: true},
		gid:       {ID: gid, Platform: PlatformOpenAI, Status: StatusActive},
	}}
	_, err := d.Decide(context.Background(), p, "redacted excerpt", aiTestDecisionCandidates(), "app.example")
	require.ErrorContains(t, err, "no eligible")
	require.Empty(t, cache.releasedAccountIDs)
	repo.current[2] = &other
	r, err := d.Decide(context.Background(), p, "redacted excerpt", aiTestDecisionCandidates(), "app.example")
	require.NoError(t, err)
	require.Equal(t, int64(2), r.AccountID)
	require.Equal(t, []int64{2}, cache.releasedAccountIDs)
	// Only the explicit administrator trial may use an unpublished source group.
	d.gateway.groupRepo.(*aiDecisionGroupStore).groups[p.GroupID].Status = StatusDisabled
	_, err = d.Decide(context.Background(), p, "redacted excerpt", aiTestDecisionCandidates(), "app.example")
	require.ErrorContains(t, err, "source group unavailable")
	_, err = d.Decide(WithAIRoutingAdminSample(context.Background()), p, "redacted excerpt", aiTestDecisionCandidates(), "app.example")
	require.NoError(t, err)
	require.Equal(t, []int64{2, 2}, cache.releasedAccountIDs)
	// The marker must not weaken the source group's data disclosure boundary.
	other.Extra = nil
	_, err = d.Decide(WithAIRoutingAdminSample(context.Background()), p, "redacted excerpt", aiTestDecisionCandidates(), "app.example")
	require.ErrorContains(t, err, "no eligible")
	require.Equal(t, []int64{2, 2}, cache.releasedAccountIDs)
}

// A mock transport body that stays connected after its initial bytes, like a
// keepalive SSE connection. It honors the request context just as net/http does.
type aiDecisionOpenStreamBody struct {
	reader  io.Reader
	ctx     context.Context
	waiting chan struct{}
	blocked atomic.Bool
	closed  atomic.Bool
}

func (b *aiDecisionOpenStreamBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	if n > 0 || err != io.EOF {
		return n, err
	}
	if b.blocked.CompareAndSwap(false, true) && b.waiting != nil {
		close(b.waiting)
	}
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}
func (b *aiDecisionOpenStreamBody) Close() error {
	b.closed.Store(true)
	return nil
}

func aiDecisionCodexAccount() *Account {
	return &Account{ID: 41, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"access_token": "local-placeholder", "chatgpt_account_id": "local"}}
}

func TestAIRoutingDeciderCompletedStreamReturnsBeforeEOF(t *testing.T) {
	account := aiDecisionCodexAccount()
	policy := aiTestDecisionPolicy(PlatformOpenAI, "generic", "gpt-6.1-sol")
	policy.TimeoutMS = 1000
	var body *aiDecisionOpenStreamBody
	d, slots := aiTestDecider(account, func(req *http.Request) (*http.Response, error) {
		body = &aiDecisionOpenStreamBody{ctx: req.Context(), reader: strings.NewReader("data: " + `{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"c2"}]}],"usage":{"input_tokens":12,"output_tokens":3}}}` + "\n\n")}
		return &http.Response{StatusCode: 200, Body: body}, nil
	})
	result, err := d.Decide(context.Background(), policy, "metadata", aiTestDecisionCandidates(), "app.example")
	require.NoError(t, err)
	require.Equal(t, "b", result.CandidateID)
	require.Equal(t, 12, result.InputTokens)
	require.Equal(t, 3, result.OutputTokens)
	require.False(t, body.blocked.Load(), "terminal response must not read until connection EOF")
	require.True(t, body.closed.Load())
	require.Equal(t, []int64{account.ID}, slots.releasedAccountIDs)
}

func TestAIRoutingDeciderCancelledStreamRetainsMeteredUsageAndReleases(t *testing.T) {
	account := aiDecisionCodexAccount()
	policy := aiTestDecisionPolicy(PlatformOpenAI, "generic", "gpt-6.1-sol")
	waiting := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-waiting:
			cancel()
		case <-ctx.Done():
		}
	}()
	var body *aiDecisionOpenStreamBody
	d, slots := aiTestDecider(account, func(req *http.Request) (*http.Response, error) {
		body = &aiDecisionOpenStreamBody{ctx: req.Context(), waiting: waiting, reader: strings.NewReader("data: " + `{"type":"response.in_progress","response":{"usage":{"input_tokens":12,"output_tokens":2}}}` + "\n\ndata: " + `{"type":"response.output_text.delta","delta":"c2"}` + "\n\n")}
		return &http.Response{StatusCode: 200, Body: body}, nil
	})
	result, err := d.Decide(ctx, policy, "metadata", aiTestDecisionCandidates(), "app.example")
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, result.CandidateID, "partial text cannot authorize execution")
	require.Equal(t, 12, result.InputTokens)
	require.Equal(t, 2, result.OutputTokens)
	require.True(t, body.closed.Load())
	require.Equal(t, []int64{account.ID}, slots.releasedAccountIDs)
}

type aiDecisionCountingReader struct {
	io.Reader
	read int
}

func (r *aiDecisionCountingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.read += n
	return n, err
}

func TestAIRoutingDeciderStreamBoundedAndIncomplete(t *testing.T) {
	prefix := "data: " + `{"type":"response.in_progress","response":{"usage":{"input_tokens":12,"output_tokens":2}}}` + "\n\n"
	for _, ending := range []string{
		"data: [DONE]\n\n",
		"data: " + `{"type":"response.output_text.delta","delta":"c2"}` + "\n\n",
		"data: " + `{"type":"response.incomplete","response":{"status":"incomplete","usage":{"input_tokens":12,"output_tokens":3}}}` + "\n\n",
		"data: " + `{"type":"response.output_item.added","item":{"type":"function_call"}}` + "\n\n",
		"data: malformed\n\n",
	} {
		text, input, output, err := readAIDecisionStream(strings.NewReader(prefix + ending))
		require.Error(t, err, ending)
		require.Empty(t, text)
		require.Equal(t, 12, input)
		require.GreaterOrEqual(t, output, 2)
	}
	// The limit includes comments and blank records, not just JSON payloads.
	for _, data := range []string{strings.Repeat(":x\n", aiDecisionMaxResponse), strings.Repeat("x", aiDecisionMaxResponse+100)} {
		source := &aiDecisionCountingReader{Reader: strings.NewReader(data)}
		_, _, _, err := readAIDecisionStream(source)
		require.Error(t, err)
		require.LessOrEqual(t, source.read, aiDecisionMaxResponse+1)
	}
}
