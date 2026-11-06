package service

import errs "github.com/Wei-Shaw/sub2api/internal/pkg/errors"

var ErrAIRoutingBackgroundNotReleased = errs.BadRequest("ai_background_not_released", "Background AI routing has not been released; use foreground Responses")

// AIRoutingBackgroundReleased is a build-time release decision. Background
// submission and recovery remain development-only in this release; there is no
// environment, database or administrator switch that can enable them.
// Existing ledger reservations still participate in ordinary billing checks.
func AIRoutingBackgroundReleased() bool { return false }
