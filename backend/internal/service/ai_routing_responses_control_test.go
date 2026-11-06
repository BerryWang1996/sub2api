//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type aiControlGroups struct {
	GroupRepository
	groups map[int64]*Group
}

func (r *aiControlGroups) GetByID(_ context.Context, id int64) (*Group, error) {
	return r.groups[id], nil
}

func TestAIRoutingResponsesControlRevalidatesOrigin(t *testing.T) {
	for _, mutation := range []string{"valid", "disabled-policy", "endpoint", "alias", "source-disabled", "candidate", "model", "target", "account-disabled", "member", "mapped", "oauth", "legacy-alias"} {
		t.Run(mutation, func(t *testing.T) {
			groupID := int64(9)
			source := &Group{ID: 1, Platform: PlatformComposite, Status: StatusActive, AIRouting: true, ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"public-alias"}}}
			groups := &aiControlGroups{groups: map[int64]*Group{1: source, 9: {ID: 9, Platform: PlatformOpenAI, Status: StatusActive}}}
			account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, AccountGroups: []AccountGroup{{GroupID: 9}}}
			accounts := &aiOutboundAccountRepo{account: account}
			gateway := &GatewayService{groupRepo: groups, accountRepo: accounts}
			policy := &AIRoutingPolicy{GroupID: 1, Enabled: true, SchemaVersion: 2, EnabledEndpoints: []string{CompositeRouteEndpointResponses}, Candidates: []AIRoutingCandidate{{ID: "c1", Platform: PlatformOpenAI, Model: "gpt-4o", Enabled: true, TargetGroupID: &groupID}}}
			origin := &AIRoutingOriginBinding{Candidate: "c1", Account: 7, Model: "gpt-4o", Platform: PlatformOpenAI, GroupTarget: 9, ResponseID: "resp_A", PublicModel: "public-alias", Native: true, Store: true}
			switch mutation {
			case "disabled-policy":
				policy.Enabled = false
			case "endpoint":
				policy.EnabledEndpoints = []string{CompositeRouteEndpointMessages}
			case "alias":
				source.ModelAllowlist.Models = []string{"other"}
			case "source-disabled":
				source.Status = "inactive"
			case "candidate":
				policy.Candidates[0].Enabled = false
			case "model":
				policy.Candidates[0].Model = "other"
			case "target":
				*policy.Candidates[0].TargetGroupID = 10
			case "account-disabled":
				account.Schedulable = false
			case "member":
				account.AccountGroups = nil
			case "mapped":
				account.Credentials = map[string]any{"model_mapping": map[string]any{"gpt-4o": "other"}}
			case "oauth":
				account.Type = AccountTypeOAuth
			case "legacy-alias":
				origin.PublicModel = ""
			}
			selected, err := gateway.ValidateAIRoutingResponsesOrigin(t.Context(), &APIKey{ID: 3, UserID: 2, Group: source}, policy, origin)
			if mutation == "valid" {
				require.NoError(t, err)
				require.Same(t, account, selected)
			} else {
				require.Error(t, err)
				require.Nil(t, selected)
			}
		})
	}
}

type aiControlHTTP struct {
	HTTPUpstream
	do func(*http.Request, string, int64, int) (*http.Response, error)
}

func (s *aiControlHTTP) Do(req *http.Request, proxy string, accountID int64, concurrency int) (*http.Response, error) {
	return s.do(req, proxy, accountID, concurrency)
}

func TestAIRoutingResponsesControlNativeHTTP(t *testing.T) {
	for _, operation := range []struct{ method, suffix string }{{"GET", ""}, {"GET", "input_items"}, {"DELETE", ""}, {"POST", "cancel"}} {
		t.Run(operation.method+"/"+operation.suffix, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls.Add(1)
				require.Equal(t, operation.method, req.Method)
				want := "/v1/responses/resp_A"
				if operation.suffix != "" {
					want += "/" + operation.suffix
				}
				require.Equal(t, want, req.URL.Path)
				require.Equal(t, "2", req.URL.Query().Get("limit"))
				require.Equal(t, "Bearer synthetic-api-key", req.Header.Get("Authorization"))
				if operation.method == "POST" {
					body, err := io.ReadAll(req.Body)
					require.NoError(t, err)
					require.Equal(t, "{}", string(body))
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotImplemented)
				_, _ = w.Write([]byte(`{"error":{"message":"synthetic provider lacks CRUD"}}`))
			}))
			defer server.Close()
			cfg := &config.Config{}
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			upstream := &aiControlHTTP{do: func(req *http.Request, proxy string, id int64, _ int) (*http.Response, error) {
				require.True(t, HTTPUpstreamRedirectsDisabled(req.Context()))
				require.Equal(t, int64(7), id)
				require.Empty(t, proxy)
				return server.Client().Do(req)
			}}
			gateway := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
			account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": server.URL, "api_key": "synthetic-api-key"}}
			resp, err := gateway.ForwardAIRoutingResponsesControl(t.Context(), account, "gpt-4o", operation.method, "resp_A", operation.suffix, "limit=2")
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, http.StatusNotImplemented, resp.StatusCode, "custom-provider errors remain visible without retry or capability claims")
			require.Equal(t, int64(1), calls.Load())
		})
	}
}

func TestAIRoutingResponsesControlRejectsUnsafeAndNonNative(t *testing.T) {
	calls := 0
	upstream := &aiControlHTTP{do: func(*http.Request, string, int64, int) (*http.Response, error) { calls++; return nil, nil }}
	gateway := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "synthetic"}}
	for _, id := range []string{"../other", "resp_A?model=other", "resp_A%2fother", "resp_A\\other", strings.Repeat("a", 257)} {
		_, err := gateway.ForwardAIRoutingResponsesControl(t.Context(), account, "gpt-4o", "GET", id, "", "")
		require.Error(t, err)
	}
	for _, typ := range []string{AccountTypeOAuth, AccountTypeSetupToken} {
		account.Type = typ
		_, err := gateway.ForwardAIRoutingResponsesControl(t.Context(), account, "gpt-4o", "GET", "resp_A", "", "")
		require.Error(t, err)
	}
	require.Zero(t, calls)
}
