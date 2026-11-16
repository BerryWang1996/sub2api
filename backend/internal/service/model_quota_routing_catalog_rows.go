package service

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/geminicli"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
)

// GatewayCatalogModels returns model metadata shared by ordinary discovery and
// the quota representation.
func GatewayCatalogModels(platform string, modelIDs []string) any {
	switch platform {
	case PlatformOpenAI:
		return gatewayOpenAIModels(modelIDs)
	case PlatformGrok:
		return gatewayGrokModels(modelIDs)
	default:
		models := make([]claude.Model, 0, len(modelIDs))
		for _, modelID := range modelIDs {
			models = append(models, claude.Model{
				ID: modelID, Type: "model", DisplayName: modelID, CreatedAt: "2024-01-01T00:00:00Z",
			})
		}
		return models
	}
}

type grokReasoningEffortOption struct {
	Value   string `json:"value"`
	Label   string `json:"label"`
	Default bool   `json:"default,omitempty"`
}

type grokModelListItem struct {
	xai.Model
	SupportsReasoningEffort bool                        `json:"supportsReasoningEffort,omitempty"`
	ReasoningEffort         string                      `json:"reasoningEffort,omitempty"`
	ReasoningEfforts        []grokReasoningEffortOption `json:"reasoningEfforts,omitempty"`
}

func gatewayGrokModels(modelIDs []string) []grokModelListItem {
	defaults := xai.DefaultModels()
	defaultsByID := make(map[string]xai.Model, len(defaults))
	for _, model := range defaults {
		defaultsByID[model.ID] = model
	}

	models := make([]grokModelListItem, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		model, ok := defaultsByID[modelID]
		if !ok {
			model = xai.Model{
				ID:          modelID,
				Object:      "model",
				OwnedBy:     "xai",
				DisplayName: modelID,
			}
		}
		item := grokModelListItem{Model: model}
		if grokModelSupportsConfigurableReasoning(modelID) {
			item.SupportsReasoningEffort = true
			item.ReasoningEffort = "high"
			efforts := []grokReasoningEffortOption{
				{Value: "low", Label: "Low"},
				{Value: "medium", Label: "Medium"},
				{Value: "high", Label: "High", Default: true},
			}
			if GrokSupportsXHighReasoningEffort(modelID) {
				efforts = append(efforts, grokReasoningEffortOption{Value: "xhigh", Label: "xHigh"})
			}
			item.ReasoningEfforts = efforts
		}
		models = append(models, item)
	}

	return models
}

func grokModelSupportsConfigurableReasoning(modelID string) bool {
	switch strings.ToLower(strings.TrimSpace(modelID)) {
	case "grok-4.7", "grok-4.7-latest", "grok-4.6", "grok-4.6-latest", "grok-4.5", "grok-4.5-latest", "grok", "grok-latest", "grok-build", "grok-build-latest", "grok-build-0.1":
		return true
	default:
		return false
	}
}

func gatewayOpenAIModels(modelIDs []string) []openai.Model {
	defaultsByID := make(map[string]openai.Model, len(openai.DefaultModels))
	for _, model := range openai.DefaultModels {
		defaultsByID[model.ID] = model
	}

	models := make([]openai.Model, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		if model, ok := defaultsByID[modelID]; ok {
			models = append(models, model)
			continue
		}
		models = append(models, openai.Model{
			ID:          modelID,
			Object:      "model",
			Created:     1704067200,
			OwnedBy:     "openai",
			Type:        "model",
			DisplayName: modelID,
		})
	}
	return models
}

func GatewayDefaultModelIDs(platform string) []string {
	switch platform {
	case PlatformOpenAI:
		return openai.DefaultModelIDs()
	case PlatformGemini:
		ids := make([]string, 0, len(geminicli.DefaultModels))
		for _, model := range geminicli.DefaultModels {
			ids = append(ids, model.ID)
		}
		return ids
	case PlatformAntigravity:
		models := antigravity.DefaultModels()
		ids := make([]string, 0, len(models))
		for _, model := range models {
			ids = append(ids, model.ID)
		}
		return ids
	case PlatformAnthropic:
		return claude.DefaultModelIDs()
	case PlatformGrok:
		return xai.DefaultModelIDs()
	case PlatformOpenCodeGo:
		return DefaultOpenCodeGoModelIDs()
	case PlatformTypeSafe:
		return []string{"jev-latest"}
	case PlatformComposite:
		ids := make([]string, 0)
		seen := make(map[string]struct{})
		for _, concretePlatform := range domain.CompositePrecedencePlatformIDs() {
			// TypeSafe is deliberately skipped: jev-latest only works through
			// /v1/systemone, so the static fallback never advertises it to LLM
			// clients. compositeAvailableModels lists it when the group can serve it.
			if concretePlatform == PlatformTypeSafe {
				continue
			}
			for _, id := range GatewayDefaultModelIDs(concretePlatform) {
				if _, ok := seen[id]; ok {
					continue
				}
				seen[id] = struct{}{}
				ids = append(ids, id)
			}
		}
		return ids
	default:
		ids := make([]string, 0, len(claude.DefaultModels))
		for _, model := range claude.DefaultModels {
			ids = append(ids, model.ID)
		}
		return ids
	}
}
