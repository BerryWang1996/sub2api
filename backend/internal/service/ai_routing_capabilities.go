package service

import (
	"context"
	"math"
	"strings"
	"time"
)

// AIRoutingExecutionRequirements is an immutable, request-local restriction on
// every account serving a bounded_history candidate, including scheduler retries.
// Candidate declarations describe administrator intent, not a successful probe.
type AIRoutingExecutionRequirements struct {
	Endpoint                 string
	Operation                string
	RequiresNativeResponses  bool
	RequiresNativeChat       bool
	RequiresNativeGemini     bool
	RequiresStoredResponses  bool
	OutputLimitOmitted       bool
	Warmup                   bool
	UsesTools                bool
	InputModalities          []string
	InputBytes               int64
	MaxOutputTokens          int64
	OutputChoices            int
	ContextWindowTokens      int64
	MaxInputTokens           int64
	CandidateMaxOutputTokens int64
	SupportsTools            *bool
	SupportsImages           *bool
	DisableTools             bool
	DisableImages            bool
}

type aiRoutingExecutionRequirementsKey struct{}

// AIRoutingRequirementsForCandidate uses the complete execution request size,
// never the smaller context projection sent to the decision model.
func AIRoutingRequirementsForCandidate(r AIRoutingRequest, candidate AIRoutingCandidate) AIRoutingExecutionRequirements {
	modalities := append([]string(nil), r.InputModalities...)
	if len(modalities) == 0 {
		modalities = []string{"text"}
	}
	hasAttachmentModality := false
	for _, modality := range modalities {
		if modality != "text" {
			hasAttachmentModality = true
			break
		}
	}
	if r.HasImages && !hasAttachmentModality {
		// Older callers group documents and images under HasImages. They
		// cannot establish that an image declaration covers the attachment.
		modalities = append(modalities, "attachment")
	}
	return cloneAIRoutingExecutionRequirements(AIRoutingExecutionRequirements{
		Endpoint: r.ClientEndpoint(), Operation: r.Operation, RequiresNativeResponses: r.RequiresNativeResponses, RequiresNativeChat: r.RequiresNativeChat, RequiresNativeGemini: r.RequiresNativeGemini, RequiresStoredResponses: r.RequiresStoredResponses, OutputLimitOmitted: r.OutputLimitOmitted,
		UsesTools: r.UsesTools, InputModalities: modalities, Warmup: r.Warmup,
		InputBytes: int64(r.InputBytes), MaxOutputTokens: int64(r.MaxOutput), OutputChoices: r.OutputChoices,
		ContextWindowTokens: int64(candidate.ContextWindowTokens),
		MaxInputTokens:      int64(candidate.MaxInputTokens), CandidateMaxOutputTokens: int64(candidate.MaxOutputTokens),
		SupportsTools: candidate.SupportsTools, SupportsImages: candidate.SupportsImages,
		DisableTools: candidate.DisableTools, DisableImages: candidate.DisableImages,
	})
}

func cloneAIRoutingExecutionRequirements(r AIRoutingExecutionRequirements) AIRoutingExecutionRequirements {
	r.InputModalities = append([]string(nil), r.InputModalities...)
	if r.SupportsTools != nil {
		value := *r.SupportsTools
		r.SupportsTools = &value
	}
	if r.SupportsImages != nil {
		value := *r.SupportsImages
		r.SupportsImages = &value
	}
	return r
}

func WithAIRoutingExecutionRequirements(ctx context.Context, r AIRoutingExecutionRequirements) context.Context {
	return context.WithValue(ctx, aiRoutingExecutionRequirementsKey{}, cloneAIRoutingExecutionRequirements(r))
}

func AIRoutingExecutionRequirementsFromContext(ctx context.Context) (AIRoutingExecutionRequirements, bool) {
	if ctx == nil {
		return AIRoutingExecutionRequirements{}, false
	}
	r, ok := ctx.Value(aiRoutingExecutionRequirementsKey{}).(AIRoutingExecutionRequirements)
	return cloneAIRoutingExecutionRequirements(r), ok
}

// AIRoutingCapabilityCheck keeps declared capacity distinct from a verified
// final upstream payload. This admission check does not run a tokenizer or probe.
type AIRoutingCapabilityCheck struct {
	Allowed                  bool     `json:"allowed"`
	Reason                   string   `json:"reason,omitempty"`
	CapacityVerified         bool     `json:"capacity_verified"`
	FinalPayloadChecked      bool     `json:"final_payload_checked"`
	InputBytes               int64    `json:"input_bytes,omitempty"`
	OutputReserveTokens      int64    `json:"output_reserve_tokens,omitempty"`
	TotalOutputReserveTokens int64    `json:"total_output_reserve_tokens,omitempty"`
	OutputChoices            int      `json:"output_choices,omitempty"`
	CapacityMethod           string   `json:"capacity_method,omitempty"`
	ContextWindowTokens      int64    `json:"context_window_tokens,omitempty"`
	MetadataSource           string   `json:"metadata_source,omitempty"`
	MetadataSyncedAt         string   `json:"metadata_synced_at,omitempty"`
	Warnings                 []string `json:"warnings,omitempty"`
}

// CheckAIRoutingExecutionAccount combines request needs, administrator limits
// and the selected account's saved metadata. Unknown protocol overhead is
// reported; a known exceeded bound is never excused by an estimate warning.
func CheckAIRoutingExecutionAccount(ctx context.Context, account *Account, model string) AIRoutingCapabilityCheck {
	r, strict := AIRoutingExecutionRequirementsFromContext(ctx)
	if !strict {
		return AIRoutingCapabilityCheck{Allowed: true}
	}
	result := AIRoutingCapabilityCheck{CapacityMethod: "request_bytes_plus_output", InputBytes: r.InputBytes, OutputReserveTokens: r.MaxOutputTokens, Warnings: []string{"protocol_overhead_unverified", "input_tokens_estimated"}}
	deny := func(reason string) AIRoutingCapabilityCheck {
		result.Reason = reason
		return result
	}
	choices := r.OutputChoices
	if choices == 0 {
		choices = 1
	}
	if choices < 1 || choices > 64 {
		return deny("execution_output_choices_invalid")
	}
	result.OutputChoices = choices
	if account == nil || model == "" || !account.IsModelSupported(model) || account.GetMappedModel(model) != model {
		return deny("execution_model_unavailable")
	}
	if r.RequiresNativeResponses && !AIRoutingAccountSupportsNativeResponses(account, model) {
		return deny("execution_native_responses_required")
	}
	if (r.RequiresNativeChat || choices > 1 && r.Endpoint != CompositeRouteEndpointGemini) && !AIRoutingAccountSupportsNativeChat(account, model) {
		return deny("execution_native_chat_required")
	}
	if (r.RequiresNativeGemini || choices > 1 && r.Endpoint == CompositeRouteEndpointGemini) && !AIRoutingAccountSupportsNativeGemini(account, model) {
		return deny("execution_native_gemini_required")
	}
	if r.RequiresStoredResponses && !AIRoutingAccountSupportsStoredResponses(account, model) {
		return deny("execution_stored_responses_required")
	}
	if r.Operation == AIRoutingResponsesInputTokens || r.Operation == AIRoutingGeminiCountTokens {
		// Counting an over-window input is valid and consumes no generation reserve.
		result.Allowed, result.OutputReserveTokens = true, 0
		result.CapacityMethod, result.Warnings = "input_count_no_generation", nil
		return result
	}
	compact := r.Operation == AIRoutingResponsesCompact || r.Operation == AIRoutingResponsesCompactV2
	if compact || r.Warmup {
		r.MaxOutputTokens, r.OutputLimitOmitted = 0, false
		result.OutputReserveTokens = 0
		result.CapacityMethod = "compact_input_bytes"
		if r.Warmup {
			result.CapacityMethod = "warmup_input_bytes"
		}
	}
	if r.InputBytes <= 0 || (r.MaxOutputTokens <= 0 && !r.OutputLimitOmitted && !compact && !r.Warmup) || r.ContextWindowTokens < 0 || r.MaxInputTokens < 0 || r.CandidateMaxOutputTokens < 0 {
		return deny("execution_capacity_unknown")
	}
	var metadata UpstreamModelMetadata
	metadataKnown := false
	if snapshot := account.GetUpstreamModelMetadataSnapshot(); snapshot != nil {
		result.MetadataSource, result.MetadataSyncedAt = snapshot.Source, snapshot.SyncedAt
		_, timeErr := time.Parse(time.RFC3339, snapshot.SyncedAt)
		if strings.TrimSpace(snapshot.Source) != "" && timeErr == nil {
			metadata, metadataKnown = snapshot.Models[model]
			if metadataKnown && metadata.ID != "" && metadata.ID != model {
				metadataKnown = false
			}
		}
	}
	if !metadataKnown {
		result.Warnings = append(result.Warnings, "account_model_metadata_unknown")
	}
	if r.OutputLimitOmitted && r.MaxOutputTokens <= 0 {
		if !metadataKnown || metadata.MaxOutputTokens <= 0 {
			return deny("execution_output_reserve_unknown")
		}
		r.MaxOutputTokens = metadata.MaxOutputTokens
		result.OutputReserveTokens = r.MaxOutputTokens
		result.Warnings = append(result.Warnings, "output_reserve_from_metadata_maximum")
	}
	if r.MaxOutputTokens > math.MaxInt64/int64(choices) {
		return deny("execution_output_reserve_overflow")
	}
	result.TotalOutputReserveTokens = r.MaxOutputTokens * int64(choices)
	if r.ContextWindowTokens > 0 {
		result.Warnings = append(result.Warnings, "context_window_administrator_declared")
	}
	if r.UsesTools {
		if r.DisableTools || (r.SupportsTools != nil && !*r.SupportsTools) {
			return deny("execution_tools_unsupported")
		}
		// Codex search/apply-patch capabilities are not generic function calling.
		if r.SupportsTools == nil {
			return deny("execution_tools_unknown")
		}
		result.Warnings = append(result.Warnings, "tools_administrator_declared")
	}
	if len(r.InputModalities) == 0 {
		return deny("execution_modalities_unknown")
	}
	for _, modality := range r.InputModalities {
		modality = strings.ToLower(strings.TrimSpace(modality))
		if metadataKnown && len(metadata.InputModalities) > 0 && !aiMetadataHasModality(metadata.InputModalities, modality) {
			return deny("execution_modality_unsupported")
		}
		switch modality {
		case "text":
			// Existing endpoint eligibility verifies text protocol adaptation.
		case "image":
			if r.DisableImages || (r.SupportsImages != nil && !*r.SupportsImages) {
				return deny("execution_images_unsupported")
			}
			if (r.SupportsImages == nil || !*r.SupportsImages) && (!metadataKnown || !aiMetadataHasModality(metadata.InputModalities, "image")) {
				return deny("execution_images_unknown")
			}
			if r.SupportsImages != nil && *r.SupportsImages {
				result.Warnings = append(result.Warnings, "images_administrator_declared")
			}
			result.Warnings = append(result.Warnings, "attachment_token_usage_unverified")
		case "audio", "document", "video":
			if !r.RequiresNativeChat && !r.RequiresNativeGemini {
				return deny("execution_modality_unknown")
			}
			if r.RequiresNativeChat && !AIRoutingAccountSupportsNativeChat(account, model) {
				return deny("execution_native_chat_required")
			}
			if r.RequiresNativeGemini && !AIRoutingAccountSupportsNativeGemini(account, model) {
				return deny("execution_native_gemini_required")
			}
			if r.DisableImages {
				return deny("execution_images_unsupported")
			}
			if !metadataKnown || !aiMetadataHasModality(metadata.InputModalities, modality) {
				return deny("execution_modality_unknown")
			}
			result.Warnings = append(result.Warnings, "attachment_token_usage_unverified")
		default:
			// A provider's modality advertisement alone does not establish an
			// equivalent conversion of documents/audio/video at this ingress.
			return deny("execution_modality_unknown")
		}
	}
	window := r.ContextWindowTokens
	if metadataKnown {
		window = aiMinKnownLimit(window, metadata.ContextWindow)
		// A larger advertised maximum may require an explicit upstream option.
		// It only tightens a known window; it never enables that option here.
		if window > 0 {
			window = aiMinKnownLimit(window, metadata.MaxContextWindow)
		}
	}
	result.ContextWindowTokens = window
	if window <= 0 {
		return deny("execution_context_window_unknown")
	}
	if r.MaxInputTokens > 0 && r.InputBytes > r.MaxInputTokens {
		return deny("execution_input_limit_exceeded")
	}
	outputLimit := r.CandidateMaxOutputTokens
	if metadataKnown {
		outputLimit = aiMinKnownLimit(outputLimit, metadata.MaxOutputTokens)
	}
	if outputLimit == 0 && !compact && !r.Warmup {
		result.Warnings = append(result.Warnings, "output_limit_unknown")
	}
	if outputLimit > 0 && r.MaxOutputTokens > outputLimit {
		return deny("execution_output_limit_exceeded")
	}
	// Subtract instead of adding untrusted request values to avoid overflow.
	if r.MaxOutputTokens > window || r.InputBytes > window-r.MaxOutputTokens {
		return deny("execution_context_window_exceeded")
	}
	result.Allowed = true
	return result
}

func aiMinKnownLimit(first, second int64) int64 {
	if second > 0 && (first <= 0 || second < first) {
		return second
	}
	return first
}

func aiMetadataHasModality(modalities []string, wanted string) bool {
	for _, modality := range modalities {
		if strings.EqualFold(strings.TrimSpace(modality), wanted) {
			return true
		}
	}
	return false
}
