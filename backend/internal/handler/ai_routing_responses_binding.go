package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

const aiResponsesOriginCountLimit = 256

func aiResponsesOriginKey(key *service.APIKey, ref service.AIRoutingOriginRef) (string, error) {
	return aiResponsesProtocolOriginKey(key, ref, service.CompositeRouteEndpointResponses)
}

func aiResponsesProtocolOriginKey(key *service.APIKey, ref service.AIRoutingOriginRef, protocol string) (string, error) {
	if key == nil || key.Group == nil || key.Group.ID <= 0 || key.UserID <= 0 || key.ID <= 0 || !service.ValidAIRoutingOriginRef(ref) {
		return "", errors.New("invalid AI response origin reference")
	}
	if protocol != service.CompositeRouteEndpointResponses && protocol != service.AIRoutingEndpointResponsesWS {
		return "", errors.New("invalid AI response origin protocol")
	}
	return service.AIRoutingProtocolOriginKey(protocol, key.Group.ID, key.UserID, key.ID, ref.Kind, ref.ID), nil
}

func aiResponsesToolBinding(origin service.AIRoutingOriginBinding) aiToolBinding {
	return aiToolBinding{Candidate: origin.Candidate, Account: origin.Account, Model: origin.Model, Platform: origin.Platform, GroupTarget: origin.GroupTarget, AccountTarget: origin.AccountTarget}
}

func loadAIResponsesBinding(ctx context.Context, store service.AIRoutingOriginStore, key *service.APIKey, req service.AIRoutingRequest) (aiToolBinding, *service.AIRoutingOriginBinding, error) {
	var target aiToolBinding
	protocol := service.CompositeRouteEndpointResponses
	if req.Endpoint == service.AIRoutingEndpointResponsesWS {
		protocol = service.AIRoutingEndpointResponsesWS
	}
	if len(req.OriginRefs) == 0 && !req.RequiresOrigin {
		return target, nil, nil
	}
	if store == nil || len(req.OriginRefs) == 0 || len(req.OriginRefs) > aiResponsesOriginCountLimit {
		return target, nil, errors.New("AI response origin storage or references unavailable")
	}
	keys := make([]string, len(req.OriginRefs))
	for i, ref := range req.OriginRefs {
		var err error
		keys[i], err = aiResponsesProtocolOriginKey(key, ref, protocol)
		if err != nil {
			return target, nil, err
		}
	}
	lookup, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	rows, err := store.LoadOriginBindings(lookup, keys)
	if err != nil || len(rows) != len(keys) {
		return target, nil, errors.New("AI response origin lookup failed")
	}
	var selected *service.AIRoutingOriginBinding
	children := make([]service.AIRoutingOriginBinding, 0, len(rows))
	for i, raw := range rows {
		var origin service.AIRoutingOriginBinding
		if !requestmodel.HasUniqueJSONMembers([]byte(raw)) || json.Unmarshal([]byte(raw), &origin) != nil || !origin.Valid() || origin.ClientProtocol() != protocol {
			return target, nil, errors.New("unknown AI response origin")
		}
		current := aiResponsesToolBinding(origin)
		if i > 0 && target != current {
			return target, nil, errors.New("AI response references have different execution origins")
		}
		target = current
		ref := req.OriginRefs[i]
		leaseContinuation := protocol == service.AIRoutingEndpointResponsesWS && ref.Kind == "response" && origin.Native && origin.ConnectionID != ""
		if ref.RequiresStored && (!origin.Native || !origin.Store) && !leaseContinuation {
			return target, nil, errors.New("AI response item does not have provider-stored state")
		}
		if !service.ValidAIRoutingOriginRef(service.AIRoutingOriginRef{Kind: "response", ID: origin.ResponseID}) {
			return target, nil, errors.New("AI response parent origin unavailable")
		}
		if ref.Kind == "response" && (origin.OriginParentKind() != "response" || origin.ResponseID != ref.ID || !origin.Native || !origin.Store && !leaseContinuation) {
			return target, nil, errors.New("AI response does not have provider-stored continuation state")
		}
		if selected == nil || ref.Kind == "response" && ref.ID == req.PreviousResponseID {
			copy := origin
			selected = &copy
		}
		if ref.Kind != "response" {
			children = append(children, origin)
		}
	}
	if err := validateAIResponsesParents(lookup, store, key, children); err != nil {
		return target, nil, err
	}
	return target, selected, nil
}

func validateAIResponsesParents(ctx context.Context, store service.AIRoutingOriginStore, key *service.APIKey, children []service.AIRoutingOriginBinding) error {
	if len(children) == 0 {
		return nil
	}
	keys := make([]string, len(children))
	for i, child := range children {
		var err error
		keys[i], err = aiResponsesProtocolOriginKey(key, service.AIRoutingOriginRef{Kind: child.OriginParentKind(), ID: child.ResponseID}, child.ClientProtocol())
		if err != nil {
			return err
		}
	}
	rows, err := store.LoadOriginBindings(ctx, keys)
	if err != nil || len(rows) != len(keys) {
		return errors.New("AI response parent origin lookup failed")
	}
	for i, raw := range rows {
		var parent service.AIRoutingOriginBinding
		if !requestmodel.HasUniqueJSONMembers([]byte(raw)) || json.Unmarshal([]byte(raw), &parent) != nil || !parent.Valid() || parent.ClientProtocol() != children[i].ClientProtocol() || parent.ConnectionID != children[i].ConnectionID || parent.OriginParentKind() != children[i].OriginParentKind() || parent.ResponseID != children[i].ResponseID || aiResponsesToolBinding(parent) != aiResponsesToolBinding(children[i]) || parent.Native != children[i].Native || parent.Store != children[i].Store {
			return errors.New("AI response parent origin unavailable or revoked")
		}
	}
	return nil
}

func saveAIResponsesBinding(ctx context.Context, store service.AIRoutingOriginStore, key *service.APIKey, ref service.AIRoutingOriginRef, origin service.AIRoutingOriginBinding) error {
	if store == nil || !origin.Valid() || origin.ResponseID == "" || len(origin.ResponseID) > 256 {
		return errors.New("AI response origin storage or execution destination unavailable")
	}
	storageKey, err := aiResponsesProtocolOriginKey(key, ref, origin.ClientProtocol())
	if err != nil {
		return err
	}
	rootRef := ref.Kind == "response" || ref.Kind == "compaction"
	if rootRef && (ref.Kind != origin.OriginParentKind() || ref.ID != origin.ResponseID) {
		return errors.New("AI response origin ID mismatch")
	}
	encoded, err := json.Marshal(origin)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if !rootRef {
		if err := validateAIResponsesParents(writeCtx, store, key, []service.AIRoutingOriginBinding{origin}); err != nil {
			return err
		}
	}
	return store.SaveOriginBinding(writeCtx, storageKey, string(encoded), origin.TTL(ref.Kind))
}

// Reuse the Messages writer's exact frame buffering, byte bounds, write failure
// handling and failover barriers. Only the protocol inspector is different.
func newAIResponsesWriter(base gin.ResponseWriter, bind func(kind, id, responseID string) error, onFailure func(error)) *aiToolWriter {
	inspector := &aiResponsesOriginInspector{bind: bind, emitted: make(map[string]bool)}
	return &aiToolWriter{ResponseWriter: base, inspect: inspector.inspect, onFailure: onFailure}
}

func newAIResponsesCompactWriter(base gin.ResponseWriter, bind func(kind, id, responseID string) error, onFailure func(error)) *aiToolWriter {
	inspector := &aiResponsesOriginInspector{bind: bind, emitted: make(map[string]bool), compact: true}
	return &aiToolWriter{ResponseWriter: base, inspect: inspector.inspect, onFailure: onFailure}
}

func newAIResponsesControlWriter(base gin.ResponseWriter, responseID string, bind func(kind, id, responseID string) error, onFailure func(error)) *aiToolWriter {
	inspector := &aiResponsesOriginInspector{bind: bind, responseID: responseID, emitted: make(map[string]bool), inputItems: true}
	return &aiToolWriter{ResponseWriter: base, inspect: inspector.inspect, onFailure: onFailure}
}

type aiResponsesOriginInspector struct {
	bind       func(kind, id, responseID string) error
	responseID string
	emitted    map[string]bool
	toolCount  int
	inputItems bool
	compact    bool
}

func (w *aiResponsesOriginInspector) bindID(kind, id string) error {
	if id == "" {
		return nil
	}
	if !service.ValidAIRoutingOriginRef(service.AIRoutingOriginRef{Kind: kind, ID: id}) {
		return errors.New("invalid AI response identifier")
	}
	if kind == "response" || kind == "compaction" {
		if w.responseID != "" && w.responseID != id {
			return errors.New("AI response identifier changed within one execution")
		}
		w.responseID = id
	}
	if w.responseID == "" {
		return errors.New("AI response item has no response origin")
	}
	key := kind + "\x00" + id
	if w.emitted[key] {
		return nil
	}
	if len(w.emitted) >= aiResponsesOriginCountLimit || kind == "tool_call" && w.toolCount >= aiToolCountLimit {
		return errors.New("too many AI response origin bindings")
	}
	if w.bind == nil {
		return errors.New("AI response binding callback unavailable")
	}
	if err := w.bind(kind, id, w.responseID); err != nil {
		return err
	}
	w.emitted[key] = true
	if kind == "tool_call" {
		w.toolCount++
	}
	return nil
}

func aiResponsesProtocolString(object map[string]any, field string) (string, error) {
	value, exists := object[field]
	if !exists || value == nil {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", errors.New("invalid AI response protocol field")
	}
	return text, nil
}

func validateAIResponsesProtocolObject(object map[string]any) error {
	for key := range object {
		for _, canonical := range []string{"type", "object", "id", "response", "response_id", "item", "item_id", "call_id", "output", "encrypted_content", "data", "first_id", "last_id", "conversation"} {
			if key != canonical && strings.EqualFold(key, canonical) {
				return errors.New("ambiguous AI response protocol field")
			}
		}
	}
	return nil
}

func (w *aiResponsesOriginInspector) bindField(object map[string]any, field, kind string) error {
	id, err := aiResponsesProtocolString(object, field)
	if err != nil {
		return err
	}
	return w.bindID(kind, id)
}

func (w *aiResponsesOriginInspector) inspectItem(item map[string]any) error {
	if err := validateAIResponsesProtocolObject(item); err != nil {
		return err
	}
	typ, err := aiResponsesProtocolString(item, "type")
	if err != nil {
		return err
	}
	if typ == "function_call" || typ == "custom_tool_call" {
		callID, err := aiResponsesProtocolString(item, "call_id")
		if err != nil || callID == "" {
			return errors.New("AI response tool call has no call identifier")
		}
	}
	if err := w.bindField(item, "id", "item"); err != nil {
		return err
	}
	if err := w.bindField(item, "call_id", "tool_call"); err != nil {
		return err
	}
	encrypted, err := aiResponsesProtocolString(item, "encrypted_content")
	if err != nil {
		return err
	}
	if encrypted != "" {
		return w.bindID("opaque", service.AIRoutingOpaqueOriginID(encrypted))
	}
	return nil
}

func (w *aiResponsesOriginInspector) inspectResponse(response map[string]any) error {
	if err := validateAIResponsesProtocolObject(response); err != nil {
		return err
	}
	if conversation := response["conversation"]; conversation != nil {
		return errors.New("AI response conversation resources are not supported")
	}
	kind := "response"
	if w.compact {
		kind = "compaction"
	}
	if err := w.bindField(response, "id", kind); err != nil {
		return err
	}
	if output, exists := response["output"]; exists && output != nil {
		items, ok := output.([]any)
		if !ok {
			return errors.New("invalid AI response output")
		}
		for _, value := range items {
			item, ok := value.(map[string]any)
			if !ok {
				return errors.New("invalid AI response output item")
			}
			if err := w.inspectItem(item); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *aiResponsesOriginInspector) inspect(data []byte, event string) error {
	if !requestmodel.HasUniqueJSONMembers(data) {
		return errors.New("invalid or duplicate AI response JSON fields")
	}
	var object map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&object); err != nil || object == nil {
		return errors.New("invalid AI response JSON object")
	}
	if err := validateAIResponsesProtocolObject(object); err != nil {
		return err
	}
	if w.compact {
		// Standalone compact is a stateless artifact, never a stored Response.
		// Error objects remain errors; successful artifacts need their own root.
		if _, failed := object["error"]; !failed {
			if object["object"] != "response.compaction" || object["id"] == nil || object["id"] == "" {
				return errors.New("invalid standalone AI compaction artifact")
			}
		}
	}
	typ, err := aiResponsesProtocolString(object, "type")
	if err != nil {
		return err
	}
	if event != "" && typ != "" && event != typ {
		return errors.New("conflicting AI response event types")
	}
	if err := w.bindField(object, "response_id", "response"); err != nil {
		return err
	}
	if response, exists := object["response"]; exists && response != nil {
		responseObject, ok := response.(map[string]any)
		if !ok {
			return errors.New("invalid AI response envelope")
		}
		if err := w.inspectResponse(responseObject); err != nil {
			return err
		}
	}
	// A unary response has a top-level id/output. Process it before any item
	// references so their binding always carries the client-visible response ID.
	if err := w.inspectResponse(object); err != nil {
		return err
	}
	if item, exists := object["item"]; exists && item != nil {
		itemObject, ok := item.(map[string]any)
		if !ok {
			return errors.New("invalid AI response item envelope")
		}
		if err := w.inspectItem(itemObject); err != nil {
			return err
		}
	}
	if err := w.bindField(object, "item_id", "item"); err != nil {
		return err
	}
	if w.inputItems {
		if data, exists := object["data"]; exists && data != nil {
			items, ok := data.([]any)
			if !ok {
				return errors.New("invalid AI response input items")
			}
			for _, value := range items {
				item, ok := value.(map[string]any)
				if !ok {
					return errors.New("invalid AI response input item")
				}
				if err := w.inspectItem(item); err != nil {
					return err
				}
			}
		}
		for _, field := range []string{"first_id", "last_id"} {
			if err := w.bindField(object, field, "item"); err != nil {
				return err
			}
		}
	}
	return w.bindField(object, "call_id", "tool_call")
}
