package service

import (
	"errors"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const AIRoutingEndpointResponsesWS = "responses_ws"

// WebSocket uses Responses' native visible-history grammar, with its own origin
// namespace. A previous store:false response requires the exact original lease;
// this does not authorize HTTP CRUD or arbitrary provider-stored item lookup.
func NewAIRoutingWSRequest(body []byte, mode string) (AIRoutingRequest, error) {
	if !utf8.Valid(body) || !gjson.ValidBytes(body) || !requestmodel.HasUniqueJSONMembers(body) {
		return AIRoutingRequest{}, errors.New("invalid AI websocket create request")
	}
	root := gjson.ParseBytes(body)
	if root.Get("type").Str != "response.create" || root.Get("background").Exists() || !aiCanonicalFields(root, "type", "stream_id", "generate", "background") {
		return AIRoutingRequest{}, errors.New("invalid AI websocket create request")
	}
	if lane := root.Get("stream_id"); lane.Exists() && (lane.Type != gjson.String || !aiRoutingWSValidLane(lane.Str)) {
		return AIRoutingRequest{}, errors.New("invalid AI websocket stream_id")
	}
	if generate := root.Get("generate"); generate.Exists() && generate.Type != gjson.True && generate.Type != gjson.False {
		return AIRoutingRequest{}, errors.New("invalid websocket generate flag")
	}
	parsedBody, err := aiWSHistoryBody(body)
	if err != nil {
		return AIRoutingRequest{}, err
	}
	r, err := NewAIRoutingResponsesRequest(parsedBody, mode)
	if err != nil {
		return r, err
	}
	r.Endpoint = AIRoutingEndpointResponsesWS
	r.Warmup = root.Get("generate").Type == gjson.False
	if r.Warmup {
		r.RequiresNativeResponses = true
	}
	r.InputBytes = len(body)
	if mode == "bounded_history" {
		r.HistoryBody = append([]byte(nil), body...)
	}
	r.RequiresStoredResponses = r.StoreResponse
	for _, ref := range r.OriginRefs {
		if ref.RequiresStored && ref.Kind != "response" {
			r.RequiresStoredResponses = true
		}
	}
	return r, nil
}

func aiWSHistoryBody(body []byte) ([]byte, error) {
	var err error
	// Envelope controls belong to the WS transport. Keep them on the actual
	// frame, but exclude them from the shared Responses semantic projection.
	for _, field := range []string{"type", "stream_id"} {
		body, err = sjson.DeleteBytes(body, field)
		if err != nil {
			return nil, err
		}
	}
	if gjson.GetBytes(body, "generate").Type == gjson.False && !gjson.GetBytes(body, "input").Exists() {
		body, err = sjson.SetRawBytes(body, "input", []byte("[]"))
		if err != nil {
			return nil, err
		}
	}
	return sjson.DeleteBytes(body, "generate")
}

// Empty warmup inputs are legal; this parser-only placeholder never replaces
// the original frame or introduces visible history into the decision evidence.
func ParseAIRoutingWSHistory(body []byte, limits AIRoutingContextLimits) (*AIRoutingHistory, error) {
	if _, err := NewAIRoutingWSRequest(body, "metadata"); err != nil {
		return nil, err
	}
	parsed, err := aiWSHistoryBody(body)
	if err != nil {
		return nil, err
	}
	return ParseAIRoutingResponsesHistory(parsed, limits)
}
