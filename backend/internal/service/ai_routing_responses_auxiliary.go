package service

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func aiResponsesInputTokensRequirements(ctx context.Context) (AIRoutingExecutionRequirements, bool) {
	r, ok := AIRoutingExecutionRequirementsFromContext(ctx)
	return r, ok && r.Operation == AIRoutingResponsesInputTokens
}

func writeAIResponsesInputTokensUnsupported(c *gin.Context) {
	c.Header("X-Sub2API-Token-Count-Source", "unsupported")
	c.JSON(http.StatusUnprocessableEntity, gin.H{"object": "response.input_tokens", "count_source": "unsupported", "estimated": false,
		"error": gin.H{"type": "input_tokens_unsupported", "message": "The selected upstream cannot count this input, and local estimation cannot recover its referenced or encrypted content."}})
}

// Even a local estimate must recheck current policy/account grants. This request
// is inspection-only; no DNS, network send, access token or model call occurs.
func checkAIRoutingLocalInputTokens(ctx context.Context, c *gin.Context, account *Account, body []byte) error {
	if _, ok := aiResponsesInputTokensRequirements(ctx); !ok {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://local.invalid/responses/input_tokens", bytes.NewReader(body))
	if err == nil {
		err = CheckAIRoutingOutboundRequest(req, account.ID)
	}
	if err != nil {
		handleAIRoutingCapacityError(c, err)
	}
	return err
}

const (
	AIRoutingResponsesInputTokens = "input_tokens"
	AIRoutingResponsesCompact     = "compact"
	AIRoutingResponsesCompactV2   = "compact_v2"
)

func isAIRoutingResponsesCompact(ctx context.Context) bool {
	r, ok := AIRoutingExecutionRequirementsFromContext(ctx)
	return ok && (r.Operation == AIRoutingResponsesCompact || r.Operation == AIRoutingResponsesCompactV2)
}

// Only the established standalone paths and Codex compaction trigger are
// auxiliary operations. context_management on an ordinary create still generates.
func AIRoutingResponsesAuxiliaryKind(body []byte, path string) string {
	path = strings.TrimRight(path, "/")
	for _, base := range []string{"/v1/responses", "/responses", "/backend-api/codex/responses"} {
		switch path {
		case base + "/input_tokens":
			return AIRoutingResponsesInputTokens
		case base + "/compact":
			return AIRoutingResponsesCompact
		case base:
			if HasCompactionTriggerInInput(body) {
				if gjson.GetBytes(body, "stream").Type == gjson.True {
					return AIRoutingResponsesCompactV2
				}
				return AIRoutingResponsesCompact
			}
		}
	}
	return ""
}

// Parse references using the same strict Responses grammar, without inventing
// generated output or stored-response state for a counting/standalone operation.
// The parsing copy is never forwarded: the established handlers own wire shaping.
func NewAIRoutingResponsesAuxiliaryRequest(body []byte, mode, operation string) (AIRoutingRequest, error) {
	if operation != AIRoutingResponsesInputTokens && operation != AIRoutingResponsesCompact && operation != AIRoutingResponsesCompactV2 {
		return AIRoutingRequest{}, errors.New("unsupported AI Responses auxiliary operation")
	}
	// Parse the original first: rewriting before validation could hide duplicate
	// store/model keys or an invalid flag in otherwise valid JSON.
	r, err := NewAIRoutingResponsesRequest(body, mode)
	if err != nil {
		return r, err
	}
	if r.Background {
		return r, errors.New("background auxiliary operations are not supported")
	}
	if operation != AIRoutingResponsesCompactV2 {
		root := gjson.ParseBytes(body)
		if root.Get("store").Type == gjson.True || root.Get("stream").Type == gjson.True || root.Get("max_output_tokens").Exists() {
			return r, errors.New("counting and standalone compaction do not accept stored, streaming, or generated-output options")
		}
		copy, setErr := sjson.SetBytes(body, "store", false)
		if setErr != nil {
			return r, setErr
		}
		r, err = NewAIRoutingResponsesRequest(copy, mode)
		if err != nil {
			return r, err
		}
	}
	r.Operation = operation
	r.InputBytes = len(body)
	if operation != AIRoutingResponsesInputTokens {
		r.RequiresNativeResponses = true
	}
	r.MaxOutput, r.OutputLimitOmitted = 0, false
	return r, nil
}

// Local tokenization cannot reconstruct account-owned state or encrypted/media
// payloads. A number for only the visible fragment would misrepresent the input.
func aiResponsesLocalCountComplete(body []byte) bool {
	root := gjson.ParseBytes(body)
	for _, field := range []string{"previous_response_id", "conversation", "prompt"} {
		if v := root.Get(field); v.Exists() && v.Type != gjson.Null {
			return false
		}
	}
	complete := true
	var visit func(gjson.Result)
	visit = func(value gjson.Result) {
		if !complete {
			return
		}
		if value.IsObject() {
			value.ForEach(func(k, v gjson.Result) bool {
				switch k.Str {
				case "encrypted_content", "file_id", "file_url", "image_url", "file_data", "image_bytes", "audio", "video":
					complete = false
				case "type":
					switch v.Str {
					case "item_reference", "compaction", "input_file", "input_image", "input_audio", "input_video":
						complete = false
					}
				}
				visit(v)
				return complete
			})
		} else if value.IsArray() {
			value.ForEach(func(_, v gjson.Result) bool { visit(v); return complete })
		}
	}
	visit(root.Get("input"))
	return complete
}
