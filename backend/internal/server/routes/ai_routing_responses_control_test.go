package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAIResponsesResourceRoutesAreExplicitAndLegacyIsUnavailable(t *testing.T) {
	router := newGatewayRoutesTestRouter()
	for _, prefix := range []string{"/v1/responses", "/responses", "/backend-api/codex/responses"} {
		for _, route := range []struct{ method, suffix string }{{"GET", "/resp_A"}, {"GET", "/resp_A/input_items"}, {"DELETE", "/resp_A"}} {
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(route.method, prefix+route.suffix, nil))
			require.Equal(t, http.StatusNotFound, response.Code)
			require.Contains(t, response.Body.String(), "Responses resource operation is unavailable", "registered routes still pass legacy groups to the explicit unsupported handler")
		}
	}
}
