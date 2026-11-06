package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func aiHTTPFreshPrincipal(key *service.APIKey, group *service.Group) *service.AIRoutingPrincipal {
	fresh := *key
	fresh.Group = group
	fresh.User = &service.User{ID: key.UserID, Status: service.StatusActive, Concurrency: 7, Role: "user"}
	return &service.AIRoutingPrincipal{APIKey: &fresh}
}

func TestAIRoutingHTTPPrincipalRevokedDuringDecision(t *testing.T) {
	for _, tc := range []struct{ name, path, body string }{
		{"messages", "/v1/messages", aiHTTPTask},
		{"responses", "/v1/responses", `{"model":"alias","store":false,"max_output_tokens":16,"input":"synthetic"}`},
		{"chat", "/v1/chat/completions", `{"model":"alias","max_tokens":16,"messages":[{"role":"user","content":"synthetic"}]}`},
		{"gemini", "/v1beta/models/alias:generateContent", `{"contents":[{"role":"user","parts":[{"text":"synthetic"}]}],"generationConfig":{"maxOutputTokens":16}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAIResponsesHTTPFixture(t)
			f.policies.policy.EnabledEndpoints = []string{"messages", "responses", "chat_completions", "gemini"}
			f.engine.POST("/v1beta/models/*modelAction", func(c *gin.Context) { f.executions++; c.Status(200) })
			revoked, reads := false, 0
			f.runtime.principal = func(_ context.Context, key *service.APIKey, _ string) (*service.AIRoutingPrincipal, error) {
				reads++
				if revoked {
					return nil, service.ErrAIRoutingPrincipalRevoked
				}
				return aiHTTPFreshPrincipal(key, f.group), nil
			}
			f.chooser.onDecide = func() { revoked = true }
			response := f.send(tc.path, tc.body, false)
			require.Equal(t, 403, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), "ai_source_revoked")
			require.Equal(t, 1, f.chooser.calls)
			require.Equal(t, 1, f.releases)
			require.Zero(t, f.executions)
			require.GreaterOrEqual(t, reads, 4)
		})
	}
}

func TestAIRoutingHTTPPrincipalFreshGroupUsedAfterDecision(t *testing.T) {
	for _, change := range []string{"privacy", "model_allowlist", "claude_code_only"} {
		t.Run(change, func(t *testing.T) {
			f := newAIResponsesHTTPFixture(t)
			freshGroup := *f.group
			f.runtime.principal = func(_ context.Context, key *service.APIKey, _ string) (*service.AIRoutingPrincipal, error) {
				return aiHTTPFreshPrincipal(key, &freshGroup), nil
			}
			seenFreshPrivacy := false
			base := f.runtime.eligible
			f.runtime.eligible = func(ctx context.Context, key *service.APIKey, p *service.AIRoutingPolicy, req service.AIRoutingRequest, endpoint string, bound int64) []service.AIRoutingCandidate {
				require.Same(t, key.Group, ctx.Value(ctxkey.Group))
				if key.Group.RequirePrivacySet {
					seenFreshPrivacy = true
					return nil // Synthetic account has not satisfied the new privacy setting.
				}
				return base(ctx, key, p, req, endpoint, bound)
			}
			f.chooser.onDecide = func() {
				switch change {
				case "privacy":
					freshGroup.RequirePrivacySet = true
				case "model_allowlist":
					freshGroup.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{"different-alias"}}
				case "claude_code_only":
					freshGroup.ClaudeCodeOnly = true
				}
			}
			response := f.send("/v1/responses", `{"model":"alias","store":false,"max_output_tokens":16,"input":"synthetic"}`, false)
			if change == "privacy" {
				require.Equal(t, 409, response.Code, response.Body.String())
				require.True(t, seenFreshPrivacy)
			} else {
				require.Equal(t, 403, response.Code, response.Body.String())
			}
			require.Zero(t, f.executions)
			require.False(t, f.group.RequirePrivacySet, "cached auth group must remain untouched")
			require.False(t, f.group.ClaudeCodeOnly)
		})
	}
}

func TestAIRoutingHTTPPrincipalInitialSnapshotAndFinalSend(t *testing.T) {
	for _, revokeAtSend := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh_snapshot", true: "revoked_before_send"}[revokeAtSend], func(t *testing.T) {
			f := newAIHTTPFixture(t)
			f.pool = f.pool[1:]
			freshGroup := *f.group
			freshGroup.RateMultiplier = 2
			key := &service.APIKey{ID: 9, UserID: 42, Key: "synthetic", GroupID: &f.group.ID, Group: f.group}
			var principal *service.AIRoutingPrincipal
			revoked := false
			f.runtime.principal = func(_ context.Context, original *service.APIKey, _ string) (*service.AIRoutingPrincipal, error) {
				require.Equal(t, "synthetic", original.Key)
				if revoked {
					return nil, service.ErrAIRoutingPrincipalRevoked
				}
				principal = aiHTTPFreshPrincipal(original, &freshGroup)
				return principal, nil
			}
			h := &GatewayHandler{aiRouting: f.runtime, gatewayService: &service.GatewayService{}}
			engine := gin.New()
			engine.Use(func(c *gin.Context) {
				c.Set(string(middleware.ContextKeyAPIKey), key)
				c.Set(string(middleware.ContextKeySubscription), &service.UserSubscription{UserID: 999})
			}, h.AIRoute)
			sends := 0
			engine.POST("/v1/messages", func(c *gin.Context) {
				current, _ := middleware.GetAPIKeyFromContext(c)
				require.NotSame(t, key, current)
				require.Same(t, &freshGroup, current.Group)
				require.Same(t, current.Group, c.Request.Context().Value(ctxkey.Group))
				subject, _ := middleware.GetAuthSubjectFromContext(c)
				require.Equal(t, 7, subject.Concurrency)
				sub, _ := middleware.GetSubscriptionFromContext(c)
				require.Nil(t, sub, "stale subscription must be cleared")
				revoked = revokeAtSend
				outbound := httptest.NewRequest(http.MethodPost, "http://synthetic.invalid/messages", strings.NewReader(`{}`)).WithContext(c.Request.Context())
				err := service.CheckAIRoutingOutboundRequest(outbound, 75)
				if revokeAtSend {
					var denied *service.AIRoutingCapacityError
					require.ErrorAs(t, err, &denied)
					require.Equal(t, "ai_source_revoked", denied.Check.Reason)
					c.Status(403)
					return
				}
				require.NoError(t, err)
				sends++
				c.Status(200)
			})
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(aiHTTPTask)))
			if revokeAtSend {
				require.Equal(t, 403, w.Code, w.Body.String())
				require.Zero(t, sends)
			} else {
				require.Equal(t, 200, w.Code, w.Body.String())
				require.Equal(t, 1, sends)
			}
			require.Zero(t, f.chooser.calls, "single candidate must still be revalidated")
			require.NotNil(t, principal)
			require.Same(t, f.group, key.Group)
		})
	}
}

func TestAIRoutingHTTPPrincipalInitiallyRevokedIncludesAuxiliary(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens", "/v1/responses", "/v1/responses/input_tokens", "/v1/responses/compact", "/v1/chat/completions"} {
		t.Run(path, func(t *testing.T) {
			f := newAIResponsesHTTPFixture(t)
			f.runtime.principal = func(context.Context, *service.APIKey, string) (*service.AIRoutingPrincipal, error) {
				return nil, service.ErrAIRoutingPrincipalRevoked
			}
			w := f.send(path, `{}`, false)
			require.Equal(t, 403, w.Code, w.Body.String())
			require.Zero(t, f.chooser.calls)
			require.Zero(t, f.executions)
			require.Zero(t, f.policies.reads)
		})
	}
}

func TestAIRoutingHTTPPrincipalStaticRoutesUntouched(t *testing.T) {
	f := newAIHTTPFixture(t)
	f.group.AIRouting = false
	reads := 0
	f.runtime.principal = func(context.Context, *service.APIKey, string) (*service.AIRoutingPrincipal, error) {
		reads++
		return nil, service.ErrAIRoutingPrincipalRevoked
	}
	w := f.send("/v1/messages", aiHTTPTask, false)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Zero(t, reads)
	require.Zero(t, f.chooser.calls)
	require.Equal(t, 1, f.executions)
}

type aiFreshControlValidator struct {
	aiControlValidator
	seenPrivacy bool
}

func (v *aiFreshControlValidator) ValidateAIRoutingResponsesOrigin(ctx context.Context, key *service.APIKey, p *service.AIRoutingPolicy, origin *service.AIRoutingOriginBinding) (*service.Account, error) {
	if key.Group.RequirePrivacySet {
		v.seenPrivacy = true
		return nil, errors.New("synthetic account no longer satisfies fresh group")
	}
	return v.aiControlValidator.ValidateAIRoutingResponsesOrigin(ctx, key, p, origin)
}

func TestAIRoutingHTTPControlPrincipalRevokedBeforeSend(t *testing.T) {
	for _, change := range []string{"key", "privacy", "allowlist"} {
		for _, operation := range []struct{ method, path string }{{http.MethodGet, "/v1/responses/resp_A"}, {http.MethodGet, "/v1/responses/resp_A/input_items"}, {http.MethodDelete, "/v1/responses/resp_A"}, {http.MethodPost, "/v1/responses/resp_A/cancel"}} {
			t.Run(change+operation.method+operation.path, func(t *testing.T) {
				key, policy, origin, _, runtime := aiControlFixture(t)
				if operation.method == http.MethodPost {
					origin.Background = true
					store := &aiControlStore{}
					require.NoError(t, saveAIResponsesBinding(t.Context(), store, key, service.AIRoutingOriginRef{Kind: "response", ID: origin.ResponseID}, *origin))
					runtime.store = store
				}
				freshGroup := *key.Group
				revoked := false
				runtime.principal = func(_ context.Context, original *service.APIKey, _ string) (*service.AIRoutingPrincipal, error) {
					if revoked {
						return nil, service.ErrAIRoutingPrincipalRevoked
					}
					return aiHTTPFreshPrincipal(original, &freshGroup), nil
				}
				validator := &aiFreshControlValidator{}
				forwarder := &aiControlForwarder{body: `{"id":"resp_A"}`, beforeSend: func() {
					switch change {
					case "key":
						revoked = true
					case "privacy":
						freshGroup.RequirePrivacySet = true
					case "allowlist":
						freshGroup.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{"other"}}
					}
				}}
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest(operation.method, operation.path, nil)
				require.True(t, routeAIResponsesControlWith(c, runtime, key, policy, validator, forwarder, &aiControlBackgroundSettlement{}))
				require.NotEqual(t, 200, w.Code)
				require.Zero(t, forwarder.calls)
				if change == "privacy" {
					require.True(t, validator.seenPrivacy)
				} else {
					require.Equal(t, 403, w.Code, w.Body.String())
				}
			})
		}
	}
}
