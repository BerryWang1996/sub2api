package service

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/tidwall/gjson"
)

var errAIDecisionResponse = errors.New("invalid or incomplete AI decision response")

// Struct decoding folds member names and keeps the last duplicate. Reject
// ambiguous protocol envelopes before checking completion/refusal/selection.
// Only walk protocol containers; provider metadata and arbitrary tool/input
// payloads can legitimately have case-sensitive custom keys.
func validAIDecisionJSON(raw []byte) bool {
	if !requestmodel.HasUniqueJSONMembers(raw) {
		return false
	}
	return validAIDecisionProtocolObject(gjson.ParseBytes(raw))
}

func validAIDecisionProtocolObject(value gjson.Result) bool {
	if value.IsArray() {
		valid := true
		value.ForEach(func(_, child gjson.Result) bool {
			valid = validAIDecisionProtocolObject(child)
			return valid
		})
		return valid
	}
	if !value.IsObject() {
		return true
	}
	if !aiCanonicalFields(value,
		"type", "status", "output", "content", "text", "usage", "input_tokens", "output_tokens",
		"response", "item", "answers", "route", "choice", "choices", "finish_reason", "message",
		"tool_calls", "refusal", "function_call", "prompt_tokens", "completion_tokens", "stop_reason",
		"candidates", "finishReason", "parts", "thought", "functionCall", "executableCode",
		"usageMetadata", "promptTokenCount", "candidatesTokenCount", "thoughtsTokenCount") {
		return false
	}
	for _, field := range []string{"output", "content", "usage", "response", "item", "answers", "route", "choices", "message", "candidates", "parts", "usageMetadata"} {
		if !validAIDecisionProtocolObject(value.Get(field)) {
			return false
		}
	}
	return true
}

func parseAIRoutingSelection(text string, ids map[string]string) (string, error) {
	value := strings.TrimSpace(text)
	if len(value) > 256 {
		return "", errAIDecisionResponse
	}
	if strings.HasPrefix(value, "{") {
		dec := json.NewDecoder(strings.NewReader(value))
		open, err := dec.Token()
		if err != nil || open != json.Delim('{') {
			return "", errAIDecisionResponse
		}
		key, err := dec.Token()
		if err != nil || key != "candidate_id" {
			return "", errAIDecisionResponse
		}
		if err = dec.Decode(&value); err != nil {
			return "", errAIDecisionResponse
		}
		end, err := dec.Token()
		if err != nil || end != json.Delim('}') {
			return "", errAIDecisionResponse
		}
		if _, err = dec.Token(); err != io.EOF {
			return "", errAIDecisionResponse
		}
	}
	id, ok := ids[value]
	if !ok {
		return "", errors.New("decision selected an unknown candidate")
	}
	return id, nil
}

type aiDecisionTextPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
type aiDecisionResponsesEnvelope struct {
	Status string `json:"status"`
	Output []struct {
		Type    string               `json:"type"`
		Status  string               `json:"status"`
		Content []aiDecisionTextPart `json:"content"`
	} `json:"output"`
	Usage struct {
		Input  int `json:"input_tokens"`
		Output int `json:"output_tokens"`
	} `json:"usage"`
}

func aiDecisionResponsesText(raw []byte) (string, int, int, error) {
	if !validAIDecisionJSON(raw) {
		return "", 0, 0, errAIDecisionResponse
	}
	var env aiDecisionResponsesEnvelope
	if json.Unmarshal(raw, &env) != nil {
		return "", 0, 0, errAIDecisionResponse
	}
	fail := func() (string, int, int, error) { return "", env.Usage.Input, env.Usage.Output, errAIDecisionResponse }
	if env.Status != "completed" {
		return fail()
	}
	var out strings.Builder
	for _, item := range env.Output {
		switch item.Type {
		case "reasoning":
			continue
		case "message":
			if item.Status != "" && item.Status != "completed" {
				return fail()
			}
			for _, part := range item.Content {
				if part.Type != "output_text" {
					return fail()
				}
				_, _ = out.WriteString(part.Text)
			}
		default:
			return fail()
		}
	}
	return out.String(), env.Usage.Input, env.Usage.Output, nil
}

// For Codex OAuth accept only an explicitly completed Responses stream. A delta or [DONE]
// alone cannot distinguish a usable answer from a truncated or refused generation.
func aiDecisionStreamText(raw []byte) (string, int, int, error) {
	return readAIDecisionStream(bytes.NewReader(raw))
}

// A completed Responses event is terminal; waiting for the connection to close
// needlessly consumes the decision deadline and can discard a valid answer.
// Count every byte, including comments/blank lines, while retaining metered usage
// on a failed or interrupted stream. The request context bounds blocked reads.
func readAIDecisionStream(source io.Reader) (string, int, int, error) {
	reader := bufio.NewReader(io.LimitReader(source, aiDecisionMaxResponse+1))
	total, inputTokens, outputTokens := 0, 0, 0
	fail := func(err error) (string, int, int, error) { return "", inputTokens, outputTokens, err }
	for {
		raw, readErr := reader.ReadBytes('\n')
		total += len(raw)
		if total > aiDecisionMaxResponse {
			return fail(errAIDecisionResponse)
		}
		line := strings.TrimSpace(string(raw))
		if !strings.HasPrefix(line, "data:") {
			if readErr != nil {
				return fail(errAIDecisionResponse)
			}
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			if readErr != nil {
				return fail(errAIDecisionResponse)
			}
			continue
		}
		var event struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
			Item     struct {
				Type string `json:"type"`
			} `json:"item"`
		}
		if !validAIDecisionJSON([]byte(data)) || json.Unmarshal([]byte(data), &event) != nil {
			return fail(errAIDecisionResponse)
		}
		if len(event.Response) > 0 {
			var reported struct {
				Usage struct {
					Input  *int `json:"input_tokens"`
					Output *int `json:"output_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(event.Response, &reported) != nil {
				return fail(errAIDecisionResponse)
			}
			if reported.Usage.Input != nil {
				inputTokens = *reported.Usage.Input
			}
			if reported.Usage.Output != nil {
				outputTokens = *reported.Usage.Output
			}
		}
		switch event.Type {
		case "response.completed":
			text, _, _, err := aiDecisionResponsesText(event.Response)
			return text, inputTokens, outputTokens, err
		case "error", "response.failed", "response.incomplete", "response.refusal.delta", "response.refusal.done":
			return fail(errAIDecisionResponse)
		case "response.output_item.added", "response.output_item.done":
			if event.Item.Type != "message" && event.Item.Type != "reasoning" {
				return fail(errAIDecisionResponse)
			}
		}
		if readErr != nil {
			return fail(errAIDecisionResponse)
		}
	}
}

func decodeAIRoutingResponse(protocol string, raw []byte, stream bool) (string, int, int, error) {
	// Responses JSON and SSE are validated by their dedicated readers.
	if protocol != APIProtocolResponses && !validAIDecisionJSON(raw) {
		return "", 0, 0, errAIDecisionResponse
	}
	switch protocol {
	case "systemone":
		var env struct {
			Answers map[string]struct {
				Type   string `json:"type"`
				Choice string `json:"choice"`
			} `json:"answers"`
			Usage struct {
				Input  int `json:"input_tokens"`
				Output int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(raw, &env) != nil {
			return "", 0, 0, errAIDecisionResponse
		}
		answer := env.Answers["route"]
		if answer.Type != "choice" || !aiShortChoiceID(answer.Choice) {
			return "", env.Usage.Input, env.Usage.Output, errAIDecisionResponse
		}
		return answer.Choice, env.Usage.Input, env.Usage.Output, nil
	case APIProtocolChatCompletions:
		var env struct {
			Choices []struct {
				Finish  string `json:"finish_reason"`
				Message struct {
					Content  string            `json:"content"`
					Calls    []json.RawMessage `json:"tool_calls"`
					Refusal  string            `json:"refusal"`
					Function json.RawMessage   `json:"function_call"`
				} `json:"message"`
			} `json:"choices"`
			Usage struct {
				Input  int `json:"prompt_tokens"`
				Output int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(raw, &env) != nil {
			return "", 0, 0, errAIDecisionResponse
		}
		if len(env.Choices) != 1 {
			return "", env.Usage.Input, env.Usage.Output, errAIDecisionResponse
		}
		c := env.Choices[0]
		if c.Finish != "stop" || len(c.Message.Calls) > 0 || c.Message.Refusal != "" || len(c.Message.Function) > 0 && string(c.Message.Function) != "null" {
			return "", env.Usage.Input, env.Usage.Output, errAIDecisionResponse
		}
		return c.Message.Content, env.Usage.Input, env.Usage.Output, nil
	case APIProtocolAnthropic:
		var env struct {
			Stop    string               `json:"stop_reason"`
			Content []aiDecisionTextPart `json:"content"`
			Usage   struct {
				Input  int `json:"input_tokens"`
				Output int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(raw, &env) != nil {
			return "", 0, 0, errAIDecisionResponse
		}
		if env.Stop != "end_turn" {
			return "", env.Usage.Input, env.Usage.Output, errAIDecisionResponse
		}
		var out strings.Builder
		for _, p := range env.Content {
			switch p.Type {
			case "text":
				_, _ = out.WriteString(p.Text)
			case "thinking", "redacted_thinking":
			default:
				return "", env.Usage.Input, env.Usage.Output, errAIDecisionResponse
			}
		}
		return out.String(), env.Usage.Input, env.Usage.Output, nil
	case APIProtocolResponses:
		if stream {
			return aiDecisionStreamText(raw)
		}
		return aiDecisionResponsesText(raw)
	case "gemini":
		var env struct {
			Candidates []struct {
				Finish  string `json:"finishReason"`
				Content struct {
					Parts []struct {
						Text     *string         `json:"text"`
						Thought  bool            `json:"thought"`
						Function json.RawMessage `json:"functionCall"`
						Code     json.RawMessage `json:"executableCode"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
			Usage struct {
				Input    int `json:"promptTokenCount"`
				Output   int `json:"candidatesTokenCount"`
				Thoughts int `json:"thoughtsTokenCount"`
			} `json:"usageMetadata"`
		}
		if json.Unmarshal(raw, &env) != nil {
			return "", 0, 0, errAIDecisionResponse
		}
		outputTokens := env.Usage.Output + env.Usage.Thoughts
		if len(env.Candidates) != 1 || env.Candidates[0].Finish != "STOP" {
			return "", env.Usage.Input, outputTokens, errAIDecisionResponse
		}
		var out strings.Builder
		for _, p := range env.Candidates[0].Content.Parts {
			if p.Text == nil || len(p.Function) > 0 || len(p.Code) > 0 {
				return "", env.Usage.Input, outputTokens, errAIDecisionResponse
			}
			if !p.Thought {
				_, _ = out.WriteString(*p.Text)
			}
		}
		return out.String(), env.Usage.Input, outputTokens, nil
	}
	return "", 0, 0, errAIDecisionResponse
}

func aiShortChoiceID(s string) bool {
	if len(s) < 2 || len(s) > 4 || s[0] != 'c' {
		return false
	}
	for _, c := range s[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
