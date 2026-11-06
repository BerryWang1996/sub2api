package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func aiGeminiOriginKey(key *service.APIKey, ref service.AIRoutingOriginRef) (string, error) {
	if key == nil || key.Group == nil || key.Group.ID <= 0 || key.ID <= 0 || key.UserID <= 0 || !service.ValidAIRoutingOriginRef(ref) {
		return "", errors.New("invalid Gemini origin")
	}
	if ref.Kind != "response" && ref.Kind != "tool_call" && ref.Kind != "opaque" {
		return "", errors.New("unsupported Gemini origin kind")
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d/%d/%d/gemini/%q/%q", key.Group.ID, key.UserID, key.ID, ref.Kind, ref.ID)))
	return fmt.Sprintf("airoute:gemini-origin:%x", sum[:]), nil
}

func loadAIGeminiBinding(ctx context.Context, store service.AIRoutingOriginStore, key *service.APIKey, request service.AIRoutingRequest) (aiToolBinding, error) {
	var target aiToolBinding
	if !request.RequiresOrigin && len(request.OriginRefs) == 0 {
		return target, nil
	}
	if store == nil || len(request.OriginRefs) == 0 || len(request.OriginRefs) > aiToolCountLimit {
		return target, errors.New("gemini origins unavailable")
	}
	keys := make([]string, len(request.OriginRefs))
	for i, ref := range request.OriginRefs {
		var err error
		keys[i], err = aiGeminiOriginKey(key, ref)
		if err != nil {
			return target, err
		}
	}
	lookup, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	rows, err := store.LoadOriginBindings(lookup, keys)
	if err != nil || len(rows) != len(keys) {
		return target, errors.New("gemini origin lookup failed")
	}
	parents := make([]service.AIRoutingOriginBinding, len(rows))
	for i, raw := range rows {
		var origin service.AIRoutingOriginBinding
		if !requestmodel.HasUniqueJSONMembers([]byte(raw)) || json.Unmarshal([]byte(raw), &origin) != nil || !origin.Valid() || origin.ResponseID == "" || origin.Store || origin.Background {
			return target, errors.New("unknown Gemini origin")
		}
		current := aiResponsesToolBinding(origin)
		if i > 0 && target != current {
			return target, errors.New("conflicting Gemini origins")
		}
		if request.OriginRefs[i].Kind == "opaque" && !origin.Native {
			return target, errors.New("gemini signature has no native origin")
		}
		target = current
		parents[i] = origin
		keys[i], err = aiGeminiOriginKey(key, service.AIRoutingOriginRef{Kind: "response", ID: origin.ResponseID})
		if err != nil {
			return target, err
		}
	}
	rows, err = store.LoadOriginBindings(lookup, keys)
	if err != nil || len(rows) != len(keys) {
		return target, errors.New("gemini parent lookup failed")
	}
	for i, raw := range rows {
		var parent service.AIRoutingOriginBinding
		if !requestmodel.HasUniqueJSONMembers([]byte(raw)) || json.Unmarshal([]byte(raw), &parent) != nil || parent != parents[i] {
			return target, errors.New("gemini parent unavailable or revoked")
		}
	}
	return target, nil
}

type aiGeminiNativeStoredState struct {
	Origin service.AIRoutingOriginBinding    `json:"origin"`
	Tool   *service.GeminiResponsesToolState `json:"tool,omitempty"`
	Part   json.RawMessage                   `json:"part,omitempty"`
}

func saveAIGeminiOrigin(ctx context.Context, store service.AIRoutingOriginStore, key *service.APIKey, ref service.AIRoutingOriginRef, origin service.AIRoutingOriginBinding, state *aiGeminiNativeStoredState) error {
	if store == nil || !origin.Valid() || ctx.Err() != nil {
		return errors.New("gemini origin storage unavailable")
	}
	storageKey, err := aiGeminiOriginKey(key, ref)
	if err != nil {
		return err
	}
	value, err := json.Marshal(origin)
	if err != nil {
		return err
	}
	write, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if state != nil {
		raw, err := json.Marshal(state)
		if err != nil || len(raw) > aiGeminiStateLimit {
			return errors.New("gemini state exceeds storage bound")
		}
		if err := store.SaveOriginBinding(write, storageKey+":state", string(raw), service.AIRoutingToolOriginTTL); err != nil {
			return err
		}
	}
	return store.SaveOriginBinding(write, storageKey, string(value), service.AIRoutingToolOriginTTL)
}

func (h *GatewayHandler) forwardAIGeminiWithOrigin(c *gin.Context, runtime *aiRoutingRuntime, store service.AIRoutingOriginStore, key *service.APIKey, req service.AIRoutingRequest, chosen service.AIRoutingCandidate) {
	base := c.Writer
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	parentID := "gemini_" + uuid.NewString()
	var selected int64
	var parent service.AIRoutingOriginBinding
	ensureParent := func() error {
		account := c.GetInt64(opsAccountIDKey)
		if ctx.Err() != nil || account <= 0 || selected != 0 && selected != account {
			return errors.New("gemini execution account changed")
		}
		if selected != 0 {
			return nil
		}
		target := newAIToolBinding(chosen, account)
		parent = service.AIRoutingOriginBinding{Candidate: target.Candidate, Account: target.Account, Model: target.Model, Platform: target.Platform, GroupTarget: target.GroupTarget, AccountTarget: target.AccountTarget, ResponseID: parentID, PublicModel: req.PublicModel, Native: chosen.Platform == service.PlatformGemini || chosen.Platform == service.PlatformAntigravity && strings.HasPrefix(strings.ToLower(chosen.Model), "gemini-")}
		if err := saveAIGeminiOrigin(ctx, store, key, service.AIRoutingOriginRef{Kind: "response", ID: parentID}, parent, nil); err != nil {
			return err
		}
		selected = account
		return nil
	}
	loadState := func(callCtx context.Context, ref service.AIRoutingOriginRef) (aiGeminiNativeStoredState, error) {
		var state aiGeminiNativeStoredState
		refused := func() (aiGeminiNativeStoredState, error) {
			return state, &service.AIRoutingCapacityError{Check: service.AIRoutingCapabilityCheck{Reason: "execution_origin_unavailable"}}
		}
		if ctx.Err() != nil || callCtx.Err() != nil {
			return refused()
		}
		binding, err := loadAIGeminiBinding(callCtx, store, key, service.AIRoutingRequest{RequiresOrigin: true, OriginRefs: []service.AIRoutingOriginRef{ref}})
		if err != nil || binding != newAIToolBinding(chosen, c.GetInt64(opsAccountIDKey)) {
			return refused()
		}
		storageKey, err := aiGeminiOriginKey(key, ref)
		if err != nil {
			return refused()
		}
		lookup, done := context.WithTimeout(callCtx, time.Second)
		defer done()
		rows, err := store.LoadOriginBindings(lookup, []string{storageKey + ":state", storageKey})
		var current service.AIRoutingOriginBinding
		if err != nil || len(rows) != 2 || len(rows[0]) > aiGeminiStateLimit || !requestmodel.HasUniqueJSONMembers([]byte(rows[0])) || json.Unmarshal([]byte(rows[0]), &state) != nil || !requestmodel.HasUniqueJSONMembers([]byte(rows[1])) || json.Unmarshal([]byte(rows[1]), &current) != nil || state.Origin != current || aiResponsesToolBinding(current) != binding {
			return refused()
		}
		return state, nil
	}
	ctx = service.WithAIRoutingGeminiNativeState(ctx, service.AIRoutingGeminiNativeStateCallbacks{
		LoadTool: func(callCtx context.Context, id string) (service.GeminiResponsesToolState, error) {
			state, err := loadState(callCtx, service.AIRoutingOriginRef{Kind: "tool_call", ID: id})
			if err != nil {
				return service.GeminiResponsesToolState{}, err
			}
			if state.Tool == nil {
				return service.GeminiResponsesToolState{}, errors.New("gemini tool state unavailable")
			}
			return *state.Tool, nil
		},
		LoadOpaque: func(callCtx context.Context, id string) (json.RawMessage, error) {
			state, err := loadState(callCtx, service.AIRoutingOriginRef{Kind: "opaque", ID: id})
			return state.Part, err
		},
	})
	c.Request = c.Request.WithContext(ctx)
	count := 0
	seenCandidates := make(map[int]bool)
	finishedCandidates := make(map[int]bool)
	origins := make(map[string]bool)
	checkRef := func(ref service.AIRoutingOriginRef) error {
		name := ref.Kind + ":" + ref.ID
		if !origins[name] && len(origins) >= aiToolCountLimit {
			return errors.New("too many Gemini origins")
		}
		origins[name] = true
		return nil
	}
	writer := &aiToolWriter{ResponseWriter: base}
	writer.inspect = func(raw []byte, event string) error {
		if writer.Status() >= 300 {
			return nil
		}
		if !gjson.ValidBytes(raw) || !requestmodel.HasUniqueJSONMembers(raw) {
			return errors.New("invalid Gemini response JSON")
		}
		if !aiGeminiOutputFields(gjson.ParseBytes(raw), "response", "candidates", "error") {
			return errors.New("ambiguous Gemini response fields")
		}
		if gjson.GetBytes(raw, "error").Exists() {
			return nil
		}
		payload := raw
		prefix := ""
		if wrapped := gjson.GetBytes(raw, "response"); wrapped.IsObject() {
			if !aiGeminiOutputFields(wrapped, "candidates", "error") {
				return errors.New("ambiguous wrapped Gemini response")
			}
			prefix = "response."
		}
		candidates := gjson.GetBytes(raw, prefix+"candidates")
		if !candidates.Exists() {
			return nil
		}
		if !candidates.IsArray() {
			return errors.New("invalid Gemini candidates")
		}
		if err := ensureParent(); err != nil {
			return err
		}
		for candidateIndex, candidate := range candidates.Array() {
			if !candidate.IsObject() || !aiGeminiOutputFields(candidate, "content", "index", "finishReason", "finish_reason") {
				return errors.New("invalid Gemini candidate")
			}
			wireIndex := candidateIndex
			if index := candidate.Get("index"); index.Exists() {
				if index.Type != gjson.Number || index.Float() != float64(index.Int()) || index.Int() < 0 || index.Int() > 63 {
					return errors.New("invalid Gemini candidate index")
				}
				wireIndex = int(index.Int())
			}
			seenCandidates[wireIndex] = true
			finish := candidate.Get("finishReason")
			if alternate := candidate.Get("finish_reason"); alternate.Exists() {
				if finish.Exists() {
					return errors.New("ambiguous Gemini finish reason")
				}
				finish = alternate
			}
			if finish.Exists() {
				if finish.Type != gjson.String || finish.Str == "" {
					return errors.New("invalid Gemini finish reason")
				}
				finishedCandidates[wireIndex] = true
			}
			if !candidate.Get("content").Exists() {
				continue
			}
			if !candidate.Get("content").IsObject() || !aiGeminiOutputFields(candidate.Get("content"), "parts") || !candidate.Get("content.parts").IsArray() {
				return errors.New("invalid Gemini candidate content")
			}
			for partIndex, part := range candidate.Get("content.parts").Array() {
				if !part.IsObject() || !aiGeminiOutputFields(part, "functionCall", "function_call", "thoughtSignature", "thought_signature") || part.Get("functionCall").Exists() && part.Get("function_call").Exists() {
					return errors.New("ambiguous Gemini output part")
				}
				path := fmt.Sprintf("%scandidates.%d.content.parts.%d", prefix, candidateIndex, partIndex)
				original := json.RawMessage(part.Raw)
				name, args, _, tool := service.GeminiNativeFunctionCall(original)
				if !tool && (part.Get("functionCall").Exists() || part.Get("function_call").Exists()) {
					return errors.New("invalid Gemini function call")
				}
				if tool {
					if count >= aiToolCountLimit {
						return errors.New("too many Gemini calls")
					}
					id := service.GeminiResponsesToolCallPrefix + strings.ReplaceAll(uuid.NewString(), "-", "")
					if err := checkRef(service.AIRoutingOriginRef{Kind: "tool_call", ID: id}); err != nil {
						return err
					}
					state := service.GeminiResponsesToolState{Name: name, Arguments: args, Part: original, Batch: fmt.Sprintf("%s:%d", parentID, wireIndex), Index: count}
					if err := saveAIGeminiOrigin(ctx, store, key, service.AIRoutingOriginRef{Kind: "tool_call", ID: id}, parent, &aiGeminiNativeStoredState{Origin: parent, Tool: &state}); err != nil {
						return err
					}
					field := "functionCall"
					if part.Get("function_call").Exists() {
						field = "function_call"
					}
					var err error
					payload, err = sjson.SetBytes(payload, path+"."+field+".id", id)
					if err != nil {
						return err
					}
					count++
				}
				signature := part.Get("thoughtSignature")
				if part.Get("thought_signature").Exists() {
					if signature.Exists() {
						return errors.New("ambiguous Gemini signature")
					}
					signature = part.Get("thought_signature")
				}
				if signature.Exists() {
					if !parent.Native || signature.Type != gjson.String || signature.Str == "" {
						return errors.New("invalid native Gemini signature")
					}
					if err := checkRef(service.AIRoutingOriginRef{Kind: "opaque", ID: service.AIRoutingOpaqueOriginID(signature.Str)}); err != nil {
						return err
					}
					if err := saveAIGeminiOrigin(ctx, store, key, service.AIRoutingOriginRef{Kind: "opaque", ID: service.AIRoutingOpaqueOriginID(signature.Str)}, parent, &aiGeminiNativeStoredState{Origin: parent, Part: original}); err != nil {
						return err
					}
				}
			}
		}
		if ctx.Err() != nil {
			return errors.New("gemini response cancelled")
		}
		if writer.isStream() {
			var encoded bytes.Buffer
			inserted := false
			for _, line := range bytes.Split(writer.pending, []byte{'\n'}) {
				if bytes.HasPrefix(line, []byte("data:")) {
					if !inserted {
						_, _ = encoded.WriteString("data: ")
						_, _ = encoded.Write(payload)
						_ = encoded.WriteByte('\n')
						inserted = true
					}
					continue
				}
				_, _ = encoded.Write(line)
				_ = encoded.WriteByte('\n')
			}
			writer.pending = bytes.TrimSuffix(encoded.Bytes(), []byte{'\n'})
			if len(writer.pending) > aiToolEventLimit {
				return errors.New("gemini event exceeds limit")
			}
		} else {
			writer.pending = payload
			writer.Header().Del("Content-Length")
		}
		return nil
	}
	writer.onFailure = func(error) {
		cancelled := ctx.Err() != nil
		c.Abort()
		cancel()
		c.Writer = base
		if !cancelled && !base.Written() {
			c.Header("Content-Length", "")
			googleError(c, http.StatusServiceUnavailable, "Gemini continuation could not be recorded")
		} else if !cancelled && writer.isStream() {
			_, _ = base.Write([]byte("data: {\"error\":{\"code\":503,\"status\":\"UNAVAILABLE\",\"message\":\"Gemini continuation could not be recorded\"}}\n\n"))
			base.Flush()
		}
		service.MarkResponseCommitted(c)
		c.Writer = writer
	}
	c.Writer = writer
	defer func() { c.Writer = base }()
	c.Next()
	if writer.finish() == nil && writer.isStream() && writer.Status() < 300 && ctx.Err() == nil {
		for candidate := range seenCandidates {
			if !finishedCandidates[candidate] {
				_ = writer.fail(errors.New("gemini stream ended before candidate completion"))
				break
			}
		}
	}
}

// Protocol fields are case sensitive. Opaque arguments remain uninterpreted.
func aiGeminiOutputFields(object gjson.Result, fields ...string) bool {
	valid := true
	object.ForEach(func(key, value gjson.Result) bool {
		for _, field := range fields {
			if strings.EqualFold(key.Str, field) && key.Str != field {
				valid = false
				return false
			}
		}
		return true
	})
	return valid
}
