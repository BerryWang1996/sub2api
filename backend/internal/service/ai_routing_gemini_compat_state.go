package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Reuse the native Gemini part replay and output processing. Antigravity keeps
// its existing OAuth/proxy/final transport; only AI Gemini models use this path.
// Ordinary legacy routes and Antigravity Claude models retain their adapters.
func (s *AntigravityGatewayService) prepareAIAntigravityGeminiState(ctx context.Context, c *gin.Context, request antigravityCompatRequest) (antigravityCompatRequest, *geminiResponsesForwardOptions, error) {
	var responses *apicompat.ResponsesRequest
	var mapping apicompat.ResponsesClientToolMapping
	var chat *geminiResponsesChatOutput
	if request.protocol == antigravityCompatChatCompletions {
		var cc apicompat.ChatCompletionsRequest
		if err := json.Unmarshal(request.originalBody, &cc); err != nil {
			return request, nil, err
		}
		var err error
		responses, err = chatCompletionsToResponses(settingServiceConfig(s.settingService), &cc)
		if err != nil {
			return request, nil, err
		}
		chat = &geminiResponsesChatOutput{includeUsage: request.includeUsage}
	} else {
		parsed, err := NewAIRoutingResponsesRequest(request.originalBody, "metadata")
		if err != nil || parsed.RequiresNativeResponses || parsed.RequiresStoredResponses {
			return request, nil, errors.New("request requires a native Responses account")
		}
		if err := validateGeminiResponsesBridgeFields(request.originalBody); err != nil {
			return request, nil, err
		}
		adapted, toolMapping, err := adaptResponsesClientToolsForAnthropic(request.originalBody)
		if err != nil {
			return request, nil, err
		}
		mapping = toolMapping
		responses = &apicompat.ResponsesRequest{}
		if err := json.Unmarshal(adapted, responses); err != nil {
			return request, nil, err
		}
	}
	callbacks, _ := ctx.Value(geminiResponsesToolStateKey{}).(GeminiResponsesToolStateCallbacks)
	replay, err := prepareGeminiResponsesReplay(ctx, responses, callbacks)
	if err != nil {
		return request, nil, err
	}
	claude, err := apicompat.ResponsesToAnthropicRequest(responses)
	if err != nil {
		return request, nil, err
	}
	request.claudeBody, err = json.Marshal(claude)
	if err != nil {
		return request, nil, err
	}
	options := &geminiResponsesForwardOptions{
		prepare: func(wire []byte) ([]byte, error) {
			inner := gjson.GetBytes(wire, "request")
			if !inner.IsObject() {
				return nil, errors.New("invalid antigravity Gemini request envelope")
			}
			restored, err := replay.apply([]byte(inner.Raw), responses.ToolChoice)
			if err != nil {
				return nil, err
			}
			return sjson.SetRawBytes(wire, "request", restored)
		},
		consume: func(resp *http.Response, upstreamStream bool) (*geminiStreamResult, error) {
			return consumeGeminiResponses(ctx, c, resp, request.startTime, request.originalModel, request.clientStream, upstreamStream, mapping, callbacks, settingServiceConfig(s.settingService), nil, chat)
		},
	}
	return request, options, nil
}
