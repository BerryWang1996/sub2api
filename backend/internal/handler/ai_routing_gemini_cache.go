package handler

import (
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func (h *GatewayHandler) prepareAIGeminiProviderReferences(c *gin.Context, key *service.APIKey, _ service.AIRoutingRequest, body []byte) (*service.GeminiCachedContent, error) {
	refs, err := service.ParseAIRoutingGeminiReferences(body)
	if err != nil {
		return nil, err
	}
	if len(refs.FileURIs) > 0 {
		return nil, errors.New("gemini file origin is not available for AI routing")
	}
	if refs.CachedContent == "" {
		return nil, nil
	}
	if h.geminiCachedContentService == nil || key == nil || key.Group == nil {
		return nil, errors.New("gemini cache ownership unavailable")
	}
	record, err := h.geminiCachedContentService.Resolve(c.Request.Context(), key.ID, key.GroupID, refs.CachedContent)
	if err != nil || record == nil {
		return nil, errors.New("gemini cache origin unavailable")
	}
	account, err := h.geminiCachedContentService.BoundAccount(c.Request.Context(), record)
	if err != nil || account == nil || !account.IsSchedulable() {
		return nil, errors.New("gemini cache account unavailable")
	}
	return record, nil
}

func (h *GatewayHandler) resolveAIGeminiCachedContentBinding(c *gin.Context, key *service.APIKey, model string, body []byte) (*service.GeminiCachedContent, []byte, bool) {
	record, err := h.prepareAIGeminiProviderReferences(c, key, service.AIRoutingRequest{}, body)
	if err != nil {
		googleError(c, 403, "Gemini cache or file origin unavailable")
		return nil, nil, false
	}
	if record == nil {
		return nil, body, true
	}
	if record.UpstreamModelID() != model {
		googleError(c, 409, "Gemini cache model does not match the selected route")
		return nil, nil, false
	}
	root := gjson.ParseBytes(body)
	path := "cachedContent"
	if root.Get("cached_content").Exists() {
		path = "cached_content"
	}
	for _, wrapper := range []string{"generateContentRequest", "generate_content_request"} {
		if nested := root.Get(wrapper); nested.Exists() {
			path = wrapper + ".cachedContent"
			if nested.Get("cached_content").Exists() {
				path = wrapper + ".cached_content"
			}
		}
	}
	rewritten, err := service.RewriteGeminiCachedContentReference(body, &service.GeminiCachedContentReference{Name: record.PublicName(), Paths: []string{path}}, record.UpstreamName)
	if err != nil {
		googleError(c, 400, "Invalid Gemini cache reference")
		return nil, nil, false
	}
	return record, rewritten, true
}
