package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// A continuation is authorized again from the current policy. Redis only
// records provenance from tool IDs emitted on a successful execution response.
type aiToolBinding struct {
	Candidate     string `json:"candidate"`
	Account       int64  `json:"account"`
	Model         string `json:"model"`
	Platform      string `json:"platform"`
	GroupTarget   int64  `json:"group_target,omitempty"`
	AccountTarget int64  `json:"account_target,omitempty"`
}

func newAIToolBinding(c service.AIRoutingCandidate, account int64) aiToolBinding {
	b := aiToolBinding{Candidate: c.ID, Account: account, Model: c.Model, Platform: c.Platform}
	if c.TargetGroupID != nil {
		b.GroupTarget = *c.TargetGroupID
	}
	if c.TargetAccountID != nil {
		b.AccountTarget = *c.TargetAccountID
	}
	return b
}

func aiToolKey(key *service.APIKey, alias, tool string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d/%d/%d/%q/%q", key.UserID, key.ID, key.Group.ID, alias, tool)))
	return fmt.Sprintf("airoute:tool:%x", sum[:])
}

func loadAIToolBinding(ctx context.Context, store service.AIRoutingStore, key *service.APIKey, r service.AIRoutingRequest) (aiToolBinding, error) {
	var binding aiToolBinding
	if len(r.ToolResults) == 0 {
		return binding, nil
	}
	if store == nil {
		return binding, errors.New("tool binding storage unavailable")
	}
	keys := make([]string, 0, len(r.ToolResults))
	for _, id := range r.ToolResults {
		keys = append(keys, aiToolKey(key, r.PublicModel, id))
	}
	lookup, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	rows, err := store.LoadToolBindings(lookup, keys)
	if err != nil {
		return binding, err
	}
	if len(rows) != len(keys) {
		return binding, errors.New("tool binding lookup incomplete")
	}
	for i, raw := range rows {
		var b aiToolBinding
		if json.Unmarshal([]byte(raw), &b) != nil || b.Account <= 0 || b.Candidate == "" {
			return binding, errors.New("unknown tool result")
		}
		if i > 0 && b != binding {
			return binding, errors.New("tool results have different origins")
		}
		binding = b
	}
	return binding, nil
}

func saveAIToolBinding(ctx context.Context, store service.AIRoutingStore, key *service.APIKey, alias, id string, binding aiToolBinding) error {
	if store == nil || binding.Account <= 0 {
		return errors.New("tool binding storage or execution account unavailable")
	}
	value, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return store.SaveToolBinding(writeCtx, aiToolKey(key, alias, id), string(value))
}

const (
	aiToolEventLimit = 128 << 10
	aiToolJSONLimit  = 4 << 20
	aiToolCountLimit = 64
)

type aiToolBlock struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

func validateAIToolJSON(data []byte) error {
	if !requestmodel.HasUniqueJSONMembers(data) {
		return errors.New("invalid or duplicate tool response JSON members")
	}
	var object map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&object); err != nil {
		return err
	}
	return validateAIToolProtocolObject(object)
}

// Only protocol envelopes are case-sensitive here. Arbitrary tool inputs may
// legitimately contain both "type" and "Type", so they are not traversed.
func validateAIToolProtocolObject(object map[string]any) error {
	for name := range object {
		for _, canonical := range []string{"type", "id", "content_block", "message", "content"} {
			if name != canonical && strings.EqualFold(name, canonical) {
				return errors.New("ambiguous tool response protocol member")
			}
		}
	}
	for _, name := range []string{"content_block", "message", "content"} {
		switch value := object[name].(type) {
		case map[string]any:
			if err := validateAIToolProtocolObject(value); err != nil {
				return err
			}
		case []any:
			for _, block := range value {
				if child, ok := block.(map[string]any); ok {
					if err := validateAIToolProtocolObject(child); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// Hold one SSE event, or one bounded non-stream response, until every tool ID
// it exposes has been persisted. Other complete SSE events flow immediately.
type aiToolWriter struct {
	gin.ResponseWriter
	pending     []byte
	lineStart   int
	emitted     map[string]bool
	bind        func(string) error
	inspect     func([]byte, string) error
	inspectDone func() error
	onFailure   func(error)
	err         error
}

func (w *aiToolWriter) bindTool(block aiToolBlock) error {
	if block.Type != "tool_use" {
		return nil
	}
	if block.ID == "" || len(block.ID) > 256 {
		return errors.New("invalid tool binding ID")
	}
	if w.emitted[block.ID] {
		return nil
	}
	if len(w.emitted) >= aiToolCountLimit {
		return errors.New("too many tool bindings in one response")
	}
	if err := w.bind(block.ID); err != nil {
		return err
	}
	if w.emitted == nil {
		w.emitted = make(map[string]bool)
	}
	w.emitted[block.ID] = true
	return nil
}

func (w *aiToolWriter) bindEvent() error {
	var data []byte
	var event string
	for rest := w.pending; len(rest) > 0; {
		line, next, _ := bytes.Cut(rest, []byte{'\n'})
		rest = next
		line = bytes.TrimSuffix(line, []byte{'\r'})
		field, value, _ := bytes.Cut(line, []byte{':'})
		value = bytes.TrimPrefix(value, []byte{' '})
		switch string(field) {
		case "event":
			event = string(value)
		case "data":
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, value...)
		}
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) && w.inspectDone != nil {
		return w.inspectDone()
	}
	if len(data) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return nil
	}
	if w.inspect != nil {
		return w.inspect(data, event)
	}
	if err := validateAIToolJSON(data); err != nil {
		return err
	}
	var frame struct {
		Type         string       `json:"type"`
		ContentBlock *aiToolBlock `json:"content_block"`
		Message      struct {
			Content []aiToolBlock `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		return fmt.Errorf("invalid tool event JSON: %w", err)
	}
	if frame.Type == "content_block_start" || event == "content_block_start" {
		if frame.ContentBlock == nil {
			return errors.New("missing content block in tool event")
		}
		if err := w.bindTool(*frame.ContentBlock); err != nil {
			return err
		}
	}
	for _, block := range frame.Message.Content {
		if err := w.bindTool(block); err != nil {
			return err
		}
	}
	return nil
}

func (w *aiToolWriter) fail(err error) error {
	if w.err == nil {
		w.err = err
		w.pending = nil
		if w.onFailure != nil {
			w.onFailure(err)
		}
	}
	return w.err
}

func (w *aiToolWriter) forwardPending() error {
	n, err := w.ResponseWriter.Write(w.pending)
	if err == nil && n != len(w.pending) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return w.fail(err)
	}
	w.pending = w.pending[:0]
	w.lineStart = 0
	return nil
}

func (w *aiToolWriter) Write(data []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.inspect == nil && (w.bind == nil || w.Status() >= 300) {
		w.pending = nil
		return w.ResponseWriter.Write(data)
	}
	if !w.isStream() {
		if len(data) > aiToolJSONLimit-len(w.pending) {
			return 0, w.fail(errors.New("tool response exceeds JSON limit"))
		}
		w.pending = append(w.pending, data...)
		return len(data), nil
	}
	consumed := 0
	for consumed < len(data) {
		part := data[consumed:]
		end := bytes.IndexByte(part, '\n')
		if end >= 0 {
			part = part[:end+1]
		}
		if len(part) > aiToolEventLimit-len(w.pending) {
			return consumed, w.fail(errors.New("tool response exceeds SSE event limit"))
		}
		w.pending = append(w.pending, part...)
		if end >= 0 {
			line := bytes.TrimSuffix(w.pending[w.lineStart:len(w.pending)-1], []byte{'\r'})
			if len(line) == 0 {
				if err := w.bindEvent(); err != nil {
					return consumed, w.fail(err)
				}
				if err := w.forwardPending(); err != nil {
					return consumed, err
				}
			}
			w.lineStart = len(w.pending)
		}
		consumed += len(part)
	}
	return consumed, nil
}

func (w *aiToolWriter) WriteString(data string) (int, error) {
	return w.Write([]byte(data))
}

func (w *aiToolWriter) isStream() bool {
	return strings.Contains(w.Header().Get("Content-Type"), "text/event-stream")
}

// Accepted buffered bytes also stop the existing Size/Written failover guards.
func (w *aiToolWriter) Size() int {
	size := w.ResponseWriter.Size()
	if size < 0 && (len(w.pending) > 0 || w.err != nil) {
		size = 0
	}
	return size + len(w.pending)
}

func (w *aiToolWriter) Written() bool {
	return w.ResponseWriter.Written() || len(w.pending) > 0 || w.err != nil
}

func (w *aiToolWriter) WriteHeaderNow() {
	if w.inspect == nil && (w.bind == nil || w.Status() >= 300) {
		w.ResponseWriter.WriteHeaderNow()
	}
}

func (w *aiToolWriter) Flush() {
	if w.err == nil && (w.ResponseWriter.Written() || w.inspect == nil && (w.bind == nil || w.Status() >= 300)) {
		w.ResponseWriter.Flush()
	}
}

func (w *aiToolWriter) finish() error {
	if w.err != nil {
		return w.err
	}
	if len(w.pending) == 0 {
		return nil
	}
	if w.isStream() {
		return w.fail(errors.New("incomplete tool SSE event"))
	}
	if w.inspect != nil {
		if err := w.inspect(w.pending, ""); err != nil {
			return w.fail(err)
		}
		return w.forwardPending()
	}
	if err := validateAIToolJSON(w.pending); err != nil {
		return w.fail(err)
	}
	var message struct {
		Content []aiToolBlock `json:"content"`
	}
	if err := json.Unmarshal(w.pending, &message); err != nil {
		return w.fail(fmt.Errorf("invalid tool response JSON: %w", err))
	}
	for _, block := range message.Content {
		if err := w.bindTool(block); err != nil {
			return w.fail(err)
		}
	}
	return w.forwardPending()
}
