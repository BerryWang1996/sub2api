package service

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
)

const forwardedModelExposureKey = "forwarded_model_exposure"

// AllowForwardedModelHeaders reads settings before forwarding starts. The routing
// grant is checked later, after the final attempt is known.
func AllowForwardedModelHeaders(c *gin.Context, settings *SettingService) {
	if c == nil || c.Request == nil || settings == nil || c.Request.Method != http.MethodPost {
		return
	}
	switch c.FullPath() {
	case "/v1/messages", "/v1/chat/completions", "/chat/completions",
		"/v1/responses", "/responses", "/backend-api/codex/responses":
		c.Set(forwardedModelExposureKey, settings.IsUpstreamModelExposureEnabled(c.Request.Context()))
	}
}

func DisableForwardedModelHeaders(c *gin.Context) {
	c.Set(forwardedModelExposureKey, false)
}

// ClearUncommittedForwardedModelHeaders prevents an AI binding failure from
// returning metadata staged for a frame the binding writer rejected.
func ClearUncommittedForwardedModelHeaders(w gin.ResponseWriter) {
	if w != nil && !w.Written() {
		w.Header().Del(responseheaders.ForwardedModelHeader)
		w.Header().Del(responseheaders.ForwardedRouteHeader)
	}
}

// writeForwardedModelHeaders is called at an existing successful output gate
// with the model used to build that attempt's upstream request.
func writeForwardedModelHeaders(c *gin.Context, model string) {
	if c == nil || c.Request == nil || !c.GetBool(forwardedModelExposureKey) || c.Writer.Written() || !safeForwardedModelHeader(model) {
		return
	}
	if c.Writer.Status() < 200 || c.Writer.Status() >= 300 {
		return
	}
	ctx := c.Request.Context()
	scope, matched := CompositeRouteScopeFromContext(ctx)
	if !matched || !isConcreteRequestPlatform(scope.TargetPlatform) {
		return
	}
	mode := "composite"
	if source, _ := CompositeRouteSourceFromContext(ctx); source == CompositeRouteSourceAI {
		mode = "ai"
	}
	c.Header(responseheaders.ForwardedModelHeader, model)
	c.Header(responseheaders.ForwardedRouteHeader, mode+"/"+scope.TargetPlatform)
}

func safeForwardedModelHeader(value string) bool {
	if len(value) == 0 || len(value) > 256 || value[0] == ' ' || value[len(value)-1] == ' ' {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return false
		}
	}
	return true
}
