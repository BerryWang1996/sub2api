//go:build unit

package service

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAIRoutingResponsesAndWSRetainNativeOnlyConstraints(t *testing.T) {
	for _, tc := range []struct {
		field  string
		native bool
	}{
		{`"text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object"}}}`, true},
		{`"truncation":"auto"`, true},
		{`"parallel_tool_calls":false`, true},
		{`"parallel_tool_calls":true`, false},
		{`"tools":[{"type":"custom","name":"run","format":{"type":"text"}}]`, true},
		{`"tools":[{"type":"function","name":"run","parameters":{"type":"object"},"strict":true}]`, true},
		{`"tools":[{"type":"function","name":"run","parameters":{"type":"object"},"strict":false}]`, false},
		{`"tool_choice":{"type":"allowed_tools","tools":[{"type":"function","name":"run"}]}`, true},
		{`"tool_choice":{"type":"function","name":"run"}`, false},
		{`"tool_choice":"required"`, false},
		{`"temperature":0.4,"top_p":0.8`, false},
		{`"include":[]`, false},
		{`"future_provider_feature":{"constraint":true}`, true},
		{`"reasoning":{"summary":"detailed"}`, true},
	} {
		t.Run(tc.field, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"model":"auto","store":false,"input":"task","max_output_tokens":100,%s}`, tc.field))
			req, err := NewAIRoutingResponsesRequest(body, "bounded_history")
			require.NoError(t, err)
			require.Equal(t, tc.native, req.RequiresNativeResponses)
			require.Equal(t, body, req.HistoryBody)
			frame := []byte(fmt.Sprintf(`{"type":"response.create","stream_id":"lane","generate":true,"model":"auto","store":false,"input":"task","max_output_tokens":100,%s}`, tc.field))
			ws, err := NewAIRoutingWSRequest(frame, "bounded_history")
			require.NoError(t, err)
			require.Equal(t, tc.native, ws.RequiresNativeResponses)
			require.Equal(t, frame, ws.HistoryBody, "transport controls remain on the real frame")
		})
	}
	for _, field := range []string{`"Text":{}`, `"Parallel_Tool_Calls":false`, `"tools":[{"type":"function","name":"run","Strict":true}]`} {
		_, err := NewAIRoutingResponsesRequest([]byte(fmt.Sprintf(`{"model":"auto","store":false,"input":"task",%s}`, field)), "metadata")
		require.Error(t, err)
	}
}
