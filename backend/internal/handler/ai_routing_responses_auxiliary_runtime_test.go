package handler

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAIRoutingResponsesAuxiliaryRuntimeNeverChooses(t *testing.T) {
	for _, path := range []string{"/v1/responses/input_tokens", "/v1/responses/compact"} {
		t.Run(path, func(t *testing.T) {
			f := newAIResponsesHTTPFixture(t)
			body := `{"model":"alias","input":"long original context"}`
			ambiguous := f.send(path, body, false)
			require.Equal(t, http.StatusBadRequest, ambiguous.Code, ambiguous.Body.String())
			require.Contains(t, ambiguous.Body.String(), "ai_auxiliary_target_ambiguous")
			require.Zero(t, f.chooser.calls)
			require.Zero(t, f.executions)
			f.pool = f.pool[1:]
			f.responses = func(c *gin.Context, _, _ bool) {
				if path == "/v1/responses/input_tokens" {
					c.Data(200, "application/json", []byte(`{"object":"response.input_tokens","input_tokens":123,"estimated":true,"count_source":"local_estimate"}`))
				} else {
					c.Data(200, "application/json", []byte(`{"object":"response.compaction","id":"cmp_1","output":[{"type":"compaction","id":"cmp_item_1","encrypted_content":"opaque_compact_state"}],"usage":{"input_tokens":123,"output_tokens":10}}`))
				}
			}
			result := f.send(path, body, false)
			require.Equal(t, http.StatusOK, result.Code, result.Body.String())
			require.Zero(t, f.chooser.calls)
			require.Zero(t, f.admissions, "auxiliary requests never reserve a decision slot")
			require.Equal(t, 1, f.executions)
			if path == "/v1/responses/compact" {
				f.responses = func(c *gin.Context, _, _ bool) {
					c.Data(200, "application/json", []byte(`{"object":"response","id":"resp_after_compact","output":[]}`))
				}
				resume := f.send("/v1/responses", `{"model":"alias","store":false,"max_output_tokens":100,"input":[{"type":"compaction","id":"cmp_item_1","encrypted_content":"opaque_compact_state"},{"role":"user","content":"continue"}]}`, false)
				require.Equal(t, http.StatusOK, resume.Code, resume.Body.String())
				require.EqualValues(t, 75, f.scope.BoundAccountID)
				invalid := f.send("/v1/responses", `{"model":"alias","previous_response_id":"cmp_1","max_output_tokens":100,"input":"continue"}`, false)
				require.Equal(t, http.StatusConflict, invalid.Code)
			}
		})
	}
}
