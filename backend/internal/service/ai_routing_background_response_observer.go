package service

import (
	"bytes"
	"context"
	"strings"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/tidwall/gjson"
)

type aiBackgroundResponseObserverKey struct{}
type aiBackgroundResponseObserver struct {
	mu         sync.Mutex
	observe    func(context.Context, int64, string) error
	terminal   func(context.Context, int64, string, AIRoutingBackgroundTerminal) error
	account    int64
	responseID string
	err        error
}

func aiBackgroundResponseAccountID(account *Account) int64 {
	if account == nil {
		return 0
	}
	return account.ID
}

// This callback records execution identity even when the client has disconnected
// and native HTTP parsing continues to drain. It is not a generation capability
// and is deliberately never copied into asynchronous billing contexts.
func WithAIRoutingBackgroundResponseObserver(ctx context.Context, fn func(context.Context, int64, string) error) context.Context {
	return context.WithValue(ctx, aiBackgroundResponseObserverKey{}, &aiBackgroundResponseObserver{observe: fn})
}

// Terminal evidence is recorded independently from ID exposure. Controls already
// have an authorized ID and may install this callback without an ID observer.
func WithAIRoutingBackgroundTerminalObserver(ctx context.Context, fn func(context.Context, int64, string, AIRoutingBackgroundTerminal) error) context.Context {
	next := &aiBackgroundResponseObserver{terminal: fn}
	if previous, _ := ctx.Value(aiBackgroundResponseObserverKey{}).(*aiBackgroundResponseObserver); previous != nil {
		previous.mu.Lock()
		next.observe, next.account, next.responseID, next.err = previous.observe, previous.account, previous.responseID, previous.err
		previous.mu.Unlock()
	}
	return context.WithValue(ctx, aiBackgroundResponseObserverKey{}, next)
}

func ObserveAIRoutingBackgroundResponsePayload(ctx context.Context, accountID int64, payload []byte, event string) error {
	return observeAIBackgroundResponsePayload(ctx, accountID, payload, event)
}

func observeAIBackgroundResponsePayload(ctx context.Context, accountID int64, payload []byte, event string) error {
	observer, _ := ctx.Value(aiBackgroundResponseObserverKey{}).(*aiBackgroundResponseObserver)
	if observer == nil {
		return nil
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.err != nil {
		return observer.err
	}
	deny := func() error {
		observer.err = &AIRoutingCapacityError{Check: AIRoutingCapabilityCheck{Reason: "execution_background_submission_unavailable"}}
		return observer.err
	}
	if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
		return nil
	}
	limit := 4 << 20
	if event != "" {
		limit = 128 << 10
	}
	if len(payload) > limit || !requestmodel.HasUniqueJSONMembers(payload) {
		return deny()
	}
	object := gjson.ParseBytes(payload)
	if !object.IsObject() || !aiCanonicalFields(object, "id", "response", "response_id", "type") {
		return deny()
	}
	if typ := object.Get("type"); typ.Exists() && (typ.Type != gjson.String || event != "" && typ.Str != event) {
		return deny()
	}
	if strings.HasPrefix(object.Get("type").Str, "response.") && len(payload) > 128<<10 {
		return deny()
	}
	var id string
	ids := []gjson.Result{object.Get("id"), object.Get("response_id")}
	if response := object.Get("response"); response.Exists() && response.Type != gjson.Null {
		if !response.IsObject() || !aiCanonicalFields(response, "id") {
			return deny()
		}
		ids = append(ids, response.Get("id"))
	}
	for _, value := range ids {
		if !value.Exists() || value.Type == gjson.Null {
			continue
		}
		if value.Type != gjson.String || !ValidAIRoutingOriginRef(AIRoutingOriginRef{Kind: "response", ID: value.Str}) || id != "" && id != value.Str {
			return deny()
		}
		id = value.Str
	}
	if id == "" {
		return nil
	}
	if accountID <= 0 || observer.observe == nil && observer.terminal == nil || observer.responseID != "" && (observer.responseID != id || observer.account != accountID) {
		return deny()
	}
	if observer.responseID == "" {
		if observer.observe != nil {
			if err := observer.observe(ctx, accountID, id); err != nil {
				return deny()
			}
		}
		observer.account, observer.responseID = accountID, id
	}
	if observer.terminal != nil {
		response := object
		if nested := object.Get("response"); nested.IsObject() {
			response = nested
		}
		if !aiCanonicalFields(response, "id", "object", "status", "model", "service_tier", "usage", "output", "tool_usage") {
			return deny()
		}
		effectiveEvent := event
		if effectiveEvent == "" {
			effectiveEvent = object.Get("type").Str
		}
		terminalEvent := effectiveEvent == "response.completed" || effectiveEvent == "response.failed" || effectiveEvent == "response.cancelled" || effectiveEvent == "response.incomplete" || effectiveEvent == "response.done"
		if terminalEvent && !response.Get("status").Exists() {
			return deny()
		}
		if response.Get("status").Exists() {
			terminal, done, err := parseAIBackgroundTerminal([]byte(response.Raw), id)
			if err != nil {
				return deny()
			}
			if terminalEvent && (!done || effectiveEvent != "response.done" && effectiveEvent != "response."+terminal.Status) || done && effectiveEvent != "" && !terminalEvent {
				return deny()
			}
			if done {
				if err = observer.terminal(ctx, accountID, id, terminal); err != nil {
					return deny()
				}
			}
		}
	}
	return nil
}

func observeAIBackgroundResponseBody(ctx context.Context, accountID int64, body []byte) error {
	if _, ok := ctx.Value(aiBackgroundResponseObserverKey{}).(*aiBackgroundResponseObserver); !ok {
		return nil
	}
	if !bodyHasSSEFraming(body) {
		return observeAIBackgroundResponsePayload(ctx, accountID, body, "")
	}
	if len(body) > 4<<20 {
		return &AIRoutingCapacityError{Check: AIRoutingCapabilityCheck{Reason: "execution_background_submission_unavailable"}}
	}
	var observedErr error
	var parser openAICompatSSEFrameParser
	emit := func(frame openAICompatSSEFrame, ok bool) {
		if ok && strings.TrimSpace(frame.Data) != "" && observedErr == nil {
			observedErr = observeAIBackgroundResponsePayload(ctx, accountID, []byte(frame.Data), frame.EventType)
		}
	}
	for _, line := range strings.Split(string(body), "\n") {
		emit(parser.AddLine(strings.TrimRight(line, "\r")))
	}
	emit(parser.Finish())
	return observedErr
}

// Queued/in-progress Responses legitimately have usage:null. Creation bypasses
// ordinary billing; validated terminal evidence reaches settlement only through
// the durable ledger. Unknown usage stays unknown in the unmodified wire object.
func aiBackgroundResponseMayLackUsage(ctx context.Context, body []byte) bool {
	if !AIRoutingBackgroundOwnsBilling(ctx) || !requestmodel.HasUniqueJSONMembers(body) {
		return false
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() || !aiCanonicalFields(root, "id", "status", "model", "object", "usage") {
		return false
	}
	id := root.Get("id")
	if id.Type != gjson.String || !ValidAIRoutingOriginRef(AIRoutingOriginRef{Kind: "response", ID: id.Str}) || root.Get("status").Type != gjson.String {
		return false
	}
	if object := root.Get("object"); object.Exists() && (object.Type != gjson.String || object.Str != "response") {
		return false
	}
	if model := root.Get("model"); model.Exists() && model.Type != gjson.String {
		return false
	}
	_, _, err := parseAIBackgroundTerminal(body, id.Str)
	return err == nil
}
