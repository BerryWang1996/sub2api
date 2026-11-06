package handler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

const aiGeminiStateLimit = 256 << 10

type aiGeminiResponseState struct {
	Target aiToolBinding                    `json:"target"`
	State  service.GeminiResponsesToolState `json:"state"`
}

// Native signatures are short-lived execution state, never decision evidence or
// diagnostics. The client-visible call ID is already protected by the strict
// Responses origin; the additional namespace binds its opaque part to that exact
// candidate/model/account and prevents cross-Key or cross-provider restoration.
func withAIGeminiResponseState(ctx context.Context, store service.AIRoutingOriginStore, key *service.APIKey, candidate service.AIRoutingCandidate, selectedAccount func() int64, protocols ...string) context.Context {
	if candidate.Platform != service.PlatformGemini && (candidate.Platform != service.PlatformAntigravity || !strings.HasPrefix(candidate.Model, "gemini-")) {
		return ctx
	}
	return service.WithGeminiResponsesToolState(ctx, newAIGeminiResponseStateCallbacks(ctx, store, key, candidate, selectedAccount, protocols...))
}

func newAIGeminiResponseStateCallbacks(ctx context.Context, store service.AIRoutingOriginStore, key *service.APIKey, candidate service.AIRoutingCandidate, selectedAccount func() int64, protocols ...string) service.GeminiResponsesToolStateCallbacks {
	protocol := service.CompositeRouteEndpointResponses
	if len(protocols) != 0 {
		protocol = protocols[0]
	}
	refused := func() error {
		return &service.AIRoutingCapacityError{Check: service.AIRoutingCapabilityCheck{Reason: "execution_origin_unavailable"}}
	}
	saved := map[string]bool{}
	stateKey := func(id string) (string, aiToolBinding, error) {
		target := newAIToolBinding(candidate, selectedAccount())
		if store == nil || key == nil || key.Group == nil || !validAIChatBinding(target) || !strings.HasPrefix(id, service.GeminiResponsesToolCallPrefix) {
			return "", target, refused()
		}
		var base string
		var err error
		if protocol == service.CompositeRouteEndpointChatCompletions {
			base, err = aiChatToolKey(key, id)
		} else {
			base, err = aiResponsesProtocolOriginKey(key, service.AIRoutingOriginRef{Kind: "tool_call", ID: id}, protocol)
		}
		if err != nil {
			return "", target, refused()
		}
		hash := sha256.Sum256([]byte(fmt.Sprintf("%s/%q/%d/%q/%d/%d", base, target.Candidate, target.Account, target.Model, target.GroupTarget, target.AccountTarget)))
		return fmt.Sprintf("airoute:gemini-response-state:%x", hash[:]), target, nil
	}
	return service.GeminiResponsesToolStateCallbacks{
		Save: func(callCtx context.Context, id string, state service.GeminiResponsesToolState) error {
			storageKey, target, err := stateKey(id)
			if err != nil || ctx.Err() != nil || callCtx.Err() != nil || !saved[id] && len(saved) >= aiToolCountLimit {
				return refused()
			}
			value, err := json.Marshal(aiGeminiResponseState{Target: target, State: state})
			if err != nil || len(value) > aiGeminiStateLimit {
				return refused()
			}
			writeCtx, cancel := context.WithTimeout(callCtx, time.Second)
			defer cancel()
			if store.SaveOriginBinding(writeCtx, storageKey, string(value), service.AIRoutingToolOriginTTL) != nil {
				return refused()
			}
			saved[id] = true
			return nil
		},
		Load: func(callCtx context.Context, id string) (service.GeminiResponsesToolState, error) {
			var empty service.GeminiResponsesToolState
			storageKey, target, err := stateKey(id)
			if err != nil || ctx.Err() != nil || callCtx.Err() != nil {
				return empty, refused()
			}
			lookup, cancel := context.WithTimeout(callCtx, time.Second)
			defer cancel()
			var bound aiToolBinding
			if protocol == service.CompositeRouteEndpointChatCompletions {
				toolStore, ok := store.(service.AIRoutingStore)
				if !ok {
					return empty, refused()
				}
				bound, err = loadAIChatBinding(lookup, toolStore, key, service.AIRoutingRequest{Endpoint: protocol, RequiresOrigin: true, ToolResults: []string{id}})
			} else {
				bound, _, err = loadAIResponsesBinding(lookup, store, key, service.AIRoutingRequest{Endpoint: protocol, RequiresOrigin: true, OriginRefs: []service.AIRoutingOriginRef{{Kind: "tool_call", ID: id}}})
			}
			if err != nil || bound != target {
				return empty, refused()
			}
			rows, err := store.LoadOriginBindings(lookup, []string{storageKey})
			if err != nil || len(rows) != 1 || len(rows[0]) > aiGeminiStateLimit || !requestmodel.HasUniqueJSONMembers([]byte(rows[0])) {
				return empty, refused()
			}
			var value aiGeminiResponseState
			if json.Unmarshal([]byte(rows[0]), &value) != nil || value.Target != target {
				return empty, refused()
			}
			return value.State, nil
		},
	}
}
