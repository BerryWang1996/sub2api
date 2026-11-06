package handler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Chat IDs cannot authorize a Responses or Messages continuation. A currently
// allowed alias can change without changing the strict source Key provenance.
func aiChatToolKey(key *service.APIKey, id string) (string, error) {
	if key == nil || key.Group == nil || key.Group.ID <= 0 || key.UserID <= 0 || key.ID <= 0 || !service.ValidAIRoutingOriginRef(service.AIRoutingOriginRef{Kind: "tool_call", ID: id}) {
		return "", errors.New("invalid Chat tool origin")
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d/%d/%d/chat_completions/%q", key.Group.ID, key.UserID, key.ID, id)))
	return fmt.Sprintf("airoute:chat-tool:%x", sum[:]), nil
}

func loadAIChatBinding(ctx context.Context, store service.AIRoutingStore, key *service.APIKey, request service.AIRoutingRequest) (aiToolBinding, error) {
	var binding aiToolBinding
	if len(request.ToolResults) == 0 && !request.RequiresOrigin {
		return binding, nil
	}
	if store == nil || len(request.ToolResults) == 0 || len(request.ToolResults) > aiToolCountLimit {
		return binding, errors.New("chat tool origins unavailable")
	}
	keys := make([]string, len(request.ToolResults))
	for i, id := range request.ToolResults {
		var err error
		keys[i], err = aiChatToolKey(key, id)
		if err != nil {
			return binding, err
		}
	}
	lookup, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	rows, err := store.LoadToolBindings(lookup, keys)
	if err != nil || len(rows) != len(keys) {
		return binding, errors.New("chat tool origin lookup failed")
	}
	for i, raw := range rows {
		var current aiToolBinding
		if !requestmodel.HasUniqueJSONMembers([]byte(raw)) || json.Unmarshal([]byte(raw), &current) != nil || !validAIChatBinding(current) || i > 0 && current != binding {
			return binding, errors.New("unknown or conflicting Chat tool origins")
		}
		binding = current
	}
	return binding, nil
}

func validAIChatBinding(binding aiToolBinding) bool {
	return (service.AIRoutingOriginBinding{Candidate: binding.Candidate, Account: binding.Account, Model: binding.Model, Platform: binding.Platform, GroupTarget: binding.GroupTarget, AccountTarget: binding.AccountTarget}).Valid()
}

func saveAIChatBinding(ctx context.Context, store service.AIRoutingStore, key *service.APIKey, id string, binding aiToolBinding) error {
	if store == nil || !validAIChatBinding(binding) {
		return errors.New("chat tool execution account unavailable")
	}
	storageKey, err := aiChatToolKey(key, id)
	if err != nil {
		return err
	}
	value, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return store.SaveToolBinding(writeCtx, storageKey, string(value))
}

type aiChatToolPosition struct{ choice, tool int }
type aiChatToolID struct{ id, name string }

// A stream does not mark its last ID fragment. Hold frames from the first tool
// delta until the involved choices finish, then bind the assembled IDs before
// releasing the original bytes. Plain text before tools still streams normally.
type aiChatWriter struct {
	*aiToolWriter
	gate                *aiChatFrameGate
	tools               map[aiChatToolPosition]aiChatToolID
	finished            map[int]bool
	choices             map[int]bool
	seenIDs             map[string]aiChatToolPosition
	bind                func(string) error
	done, upstreamError bool
}

func newAIChatWriter(base gin.ResponseWriter, bind func(string) error, failed func(error)) *aiChatWriter {
	gate := &aiChatFrameGate{ResponseWriter: base}
	w := &aiChatWriter{gate: gate, tools: map[aiChatToolPosition]aiChatToolID{}, finished: map[int]bool{}, choices: map[int]bool{}, seenIDs: map[string]aiChatToolPosition{}, bind: bind}
	w.aiToolWriter = &aiToolWriter{ResponseWriter: gate, inspect: w.inspectChat, inspectDone: w.inspectChatDone, onFailure: func(err error) {
		gate.pending = nil
		if failed != nil {
			failed(err)
		}
	}}
	return w
}

func aiChatIndex(value gjson.Result) (int, bool) {
	index, err := strconv.Atoi(value.Raw)
	return index, value.Type == gjson.Number && err == nil && index >= 0 && index < aiToolCountLimit
}

func aiChatEnvelope(value gjson.Result, fields ...string) bool {
	if !value.IsObject() {
		return false
	}
	ok := true
	value.ForEach(func(key, _ gjson.Result) bool {
		for _, field := range fields {
			if key.Str != field && strings.EqualFold(key.Str, field) {
				ok = false
				return false
			}
		}
		return true
	})
	return ok
}

func (w *aiChatWriter) inspectChat(data []byte, _ string) error {
	invalid := errors.New("invalid Chat tool response")
	if w.done || w.upstreamError {
		return errors.New("chat data after terminal event")
	}
	if !utf8.Valid(data) || !requestmodel.HasUniqueJSONMembers(data) || !gjson.ValidBytes(data) {
		return invalid
	}
	root := gjson.ParseBytes(data)
	if !aiChatEnvelope(root, "choices", "error", "id", "object") {
		return invalid
	}
	if root.Get("error").Exists() {
		if root.Get("choices").Exists() || !aiChatEnvelope(root.Get("error"), "type", "code", "message", "param") {
			return invalid
		}
		// Preserve the provider's final error, while dropping every held tool
		// frame. An error is not permission to release an incomplete tool ID.
		w.gate.pending, w.gate.hold, w.upstreamError = nil, false, true
		return nil
	}
	choices := root.Get("choices")
	if !choices.IsArray() {
		return invalid
	}
	seenChoices := map[int]bool{}
	for _, choice := range choices.Array() {
		if !aiChatEnvelope(choice, "index", "delta", "message", "finish_reason") {
			return invalid
		}
		index, ok := aiChatIndex(choice.Get("index"))
		if !ok || seenChoices[index] {
			return invalid
		}
		if w.finished[index] {
			return errors.New("chat data after choice completion")
		}
		seenChoices[index] = true
		w.choices[index] = true
		message := choice.Get("message")
		stream := w.isStream()
		if stream {
			if message.Exists() {
				return invalid
			}
			message = choice.Get("delta")
		} else if choice.Get("delta").Exists() {
			return invalid
		}
		if !aiChatEnvelope(message, "tool_calls", "function_call", "content", "role") || message.Get("function_call").Exists() && message.Get("function_call").Type != gjson.Null {
			return invalid
		}
		calls := message.Get("tool_calls")
		if calls.Exists() && calls.Type != gjson.Null {
			if !calls.IsArray() || w.finished[index] {
				return invalid
			}
			seenTools := map[int]bool{}
			for toolIndex, call := range calls.Array() {
				function := call.Get("function")
				if !aiChatEnvelope(call, "index", "id", "type", "function") || (!stream || function.Exists()) && !aiChatEnvelope(function, "name", "arguments") {
					return invalid
				}
				if stream {
					toolIndex, ok = aiChatIndex(call.Get("index"))
					if !ok {
						return invalid
					}
				}
				if seenTools[toolIndex] {
					return errors.New("duplicate Chat tool position in one delta")
				}
				seenTools[toolIndex] = true
				if kind := call.Get("type"); kind.Exists() && (kind.Type != gjson.String || kind.Str != "function") {
					return invalid
				}
				pos := aiChatToolPosition{index, toolIndex}
				current, exists := w.tools[pos]
				if !exists && len(w.tools) >= aiToolCountLimit {
					return errors.New("too many Chat tool calls")
				}
				for _, field := range []struct {
					value gjson.Result
					dst   *string
				}{{call.Get("id"), &current.id}, {call.Get("function.name"), &current.name}} {
					if field.value.Exists() {
						if field.value.Type != gjson.String || len(*field.dst)+len(field.value.Str) > 256 {
							return invalid
						}
						*field.dst += field.value.Str
					}
				}
				if args := call.Get("function.arguments"); args.Exists() && args.Type != gjson.String {
					return invalid
				}
				w.tools[pos] = current
			}
		}
		finish := choice.Get("finish_reason")
		if !stream || finish.Exists() && finish.Type != gjson.Null {
			if stream && finish.Type != gjson.String {
				return invalid
			}
			for pos, tool := range w.tools {
				if pos.choice != index || w.finished[index] {
					continue
				}
				if tool.name == "" || !service.ValidAIRoutingOriginRef(service.AIRoutingOriginRef{Kind: "tool_call", ID: tool.id}) {
					return invalid
				}
				if previous, exists := w.seenIDs[tool.id]; exists && previous != pos {
					return errors.New("chat tool ID reused by different choices or tools")
				}
				if w.bind == nil {
					return errors.New("chat tool origin writer unavailable")
				}
				if err := w.bind(tool.id); err != nil {
					return err
				}
				w.seenIDs[tool.id] = pos
			}
			w.finished[index] = true
		}
	}
	w.gate.hold = false
	for pos := range w.tools {
		if !w.finished[pos.choice] {
			w.gate.hold = true
			break
		}
	}
	return nil
}

func (w *aiChatWriter) inspectChatDone() error {
	if w.done {
		return errors.New("duplicate Chat terminal event")
	}
	if !w.upstreamError {
		for index := range w.choices {
			if !w.finished[index] {
				return errors.New("chat terminal event before choice completion")
			}
		}
		if w.gate.hold {
			return errors.New("chat terminal event before tool origin binding")
		}
	}
	w.done = true
	return nil
}

func (w *aiChatWriter) finish() error {
	if err := w.aiToolWriter.finish(); err != nil {
		return err
	}
	if w.gate.hold || len(w.gate.pending) > 0 {
		return w.fail(errors.New("incomplete Chat tool stream"))
	}
	if w.isStream() && !w.done && !w.upstreamError {
		return w.fail(errors.New("chat stream ended before terminal event"))
	}
	return nil
}

type aiChatFrameGate struct {
	gin.ResponseWriter
	hold    bool
	pending []byte
}

func (w *aiChatFrameGate) Write(data []byte) (int, error) {
	if w.hold {
		if len(data) > aiToolJSONLimit-len(w.pending) {
			return 0, errors.New("chat tool stream exceeds binding buffer limit")
		}
		w.pending = append(w.pending, data...)
		return len(data), nil
	}
	if len(w.pending) > 0 {
		n, err := w.ResponseWriter.Write(w.pending)
		if err == nil && n != len(w.pending) {
			err = io.ErrShortWrite
		}
		w.pending = nil
		if err != nil {
			return 0, err
		}
	}
	return w.ResponseWriter.Write(data)
}

func (w *aiChatFrameGate) Size() int {
	size := w.ResponseWriter.Size()
	if size < 0 && len(w.pending) > 0 {
		size = 0
	}
	return size + len(w.pending)
}

func (w *aiChatFrameGate) Written() bool { return w.ResponseWriter.Written() || len(w.pending) > 0 }
func (w *aiChatFrameGate) Flush() {
	if !w.hold && len(w.pending) == 0 && w.ResponseWriter.Written() {
		w.ResponseWriter.Flush()
	}
}
