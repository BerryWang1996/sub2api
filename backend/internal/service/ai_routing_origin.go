package service

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"
)

// AIRoutingOriginStore is optional: existing tool-only stores keep their contract.
// Each origin is immutable; conflicting writes must fail closed across instances.
type AIRoutingOriginStore interface {
	LoadOriginBindings(context.Context, []string) ([]string, error)
	SaveOriginBinding(ctx context.Context, key, value string, ttl time.Duration) error
}

// Revocation shares the origin's storage key so readers on every instance see
// the tombstone before accepting either the response or any child identifier.
type AIRoutingOriginRevoker interface {
	RevokeOriginBinding(ctx context.Context, key string, ttl time.Duration) error
}

const (
	AIRoutingResponseOriginTTL = 30 * 24 * time.Hour
	AIRoutingToolOriginTTL     = 24 * time.Hour
)

// AIRoutingOriginBinding describes an execution destination, not response status
// or executable history. Native identifies the actual Responses wire protocol;
// Store records whether this particular response requested storage. Current
// provider storage capability must still be checked before stateful continuations.
type AIRoutingOriginBinding struct {
	Protocol      string `json:"protocol,omitempty"`
	ConnectionID  string `json:"connection_id,omitempty"`
	Candidate     string `json:"candidate"`
	Account       int64  `json:"account"`
	Model         string `json:"model"`
	Platform      string `json:"platform"`
	GroupTarget   int64  `json:"group_target,omitempty"`
	AccountTarget int64  `json:"account_target,omitempty"`
	ResponseID    string `json:"response_id"`
	ParentKind    string `json:"parent_kind,omitempty"`
	PublicModel   string `json:"public_model,omitempty"`
	Store         bool   `json:"store"`
	Background    bool   `json:"background"`
	Native        bool   `json:"native"`
}

func (b AIRoutingOriginBinding) Valid() bool {
	wsTransport := b.Native && b.ConnectionID != "" || !b.Native && b.ConnectionID == "" && !b.Store && !b.Background
	return b.Candidate != "" && b.Account > 0 && strings.TrimSpace(b.Model) != "" && b.Platform != "" &&
		(b.Protocol == "" || b.Protocol == CompositeRouteEndpointResponses || b.Protocol == AIRoutingEndpointResponsesWS && wsTransport) &&
		(b.ParentKind == "" || b.ParentKind == "response" || b.ParentKind == "compaction" && !b.Store && !b.Background) &&
		(b.GroupTarget > 0) != (b.AccountTarget > 0) &&
		b.GroupTarget >= 0 && b.AccountTarget >= 0 &&
		(b.AccountTarget == 0 || b.AccountTarget == b.Account)
}

func (b AIRoutingOriginBinding) ClientProtocol() string {
	if b.Protocol == "" {
		return CompositeRouteEndpointResponses
	}
	return b.Protocol
}

func (b AIRoutingOriginBinding) OriginParentKind() string {
	if b.ParentKind == "" {
		return "response"
	}
	return b.ParentKind
}

func (b AIRoutingOriginBinding) TTL(kind string) time.Duration {
	if b.ClientProtocol() == AIRoutingEndpointResponsesWS && !b.Store {
		return AIRoutingToolOriginTTL
	}
	if kind == "response" || b.Native && b.Store {
		return AIRoutingResponseOriginTTL
	}
	return AIRoutingToolOriginTTL
}

// Aliases are intentionally absent: another currently authorized alias may
// resume the same strict-Key origin. ID kinds cannot alias one another.
func AIRoutingOriginKey(groupID, userID, apiKeyID int64, kind, id string) string {
	return AIRoutingProtocolOriginKey(CompositeRouteEndpointResponses, groupID, userID, apiKeyID, kind, id)
}

func AIRoutingProtocolOriginKey(protocol string, groupID, userID, apiKeyID int64, kind, id string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d/%d/%d/%s/%q/%q", groupID, userID, apiKeyID, protocol, kind, id)))
	return fmt.Sprintf("airoute:origin:%x", sum[:])
}

func ValidAIRoutingOriginRef(ref AIRoutingOriginRef) bool {
	if ref.ID == "" || len(ref.ID) > 256 || strings.TrimSpace(ref.ID) != ref.ID {
		return false
	}
	switch ref.Kind {
	case "response", "compaction", "tool_call", "item", "conversation":
		return true
	case "opaque":
		if len(ref.ID) != 64 {
			return false
		}
		for _, c := range ref.ID {
			if c < '0' || c > '9' {
				if c < 'a' || c > 'f' {
					return false
				}
			}
		}
		return true
	default:
		return false
	}
}
