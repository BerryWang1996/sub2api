package service

import (
	"context"
	"errors"
	"time"
)

// These hooks reuse the existing native Responses GET transport, captured-price
// calculator and idempotent usage-log/cache APIs. There is intentionally NO
// generation/POST hook in recovery, including for submission_unknown jobs.
type AIRoutingBackgroundWorkerHooks struct {
	Fetch        func(context.Context, *AIRoutingBackgroundJob) (AIRoutingBackgroundTerminal, bool, error)
	BuildCommand func(context.Context, *AIRoutingBackgroundJob) (*UsageBillingCommand, error)
	// Sync must synchronously invalidate affected billing/auth caches. For a
	// settled job it also idempotently writes usage under BillingRequestID, and
	// confirms all required post-commit quota projections before returning nil.
	Sync func(context.Context, *AIRoutingBackgroundJob) error
}

type AIRoutingBackgroundWorker struct {
	Repository AIRoutingBackgroundRepository
	Hooks      AIRoutingBackgroundWorkerHooks
	Owner      string
}

var ErrAIBackgroundRecoveryPending = errors.New("background response reconciliation remains pending")

// RunOnce performs bounded recovery. A fresh process can continue from only SQL
// state; unavailable usage, provider GET errors and cache failures keep the job
// and funds reserved. No response body or credential is persisted or logged.
func (w *AIRoutingBackgroundWorker) RunOnce(ctx context.Context, limit int) (int, error) {
	if w == nil || w.Repository == nil || w.Hooks.Fetch == nil || w.Hooks.BuildCommand == nil || w.Hooks.Sync == nil || w.Owner == "" || limit < 1 || limit > 64 {
		return 0, ErrAIBackgroundConflict
	}
	processed := 0
	var pending error
	for processed < limit {
		if err := ctx.Err(); err != nil {
			return processed, err
		}
		jobs, err := w.Repository.ClaimDue(ctx, w.Owner, time.Minute, 1)
		if err != nil {
			return processed, err
		}
		if len(jobs) == 0 {
			return processed, pending
		}
		jobCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = w.reconcile(jobCtx, jobs[0])
		cancel()
		processed++
		if err != nil {
			if errors.Is(err, ErrAIBackgroundRecoveryPending) {
				pending = err
				continue
			}
			return processed, err
		}
	}
	return processed, pending
}

func (w *AIRoutingBackgroundWorker) reconcile(ctx context.Context, j *AIRoutingBackgroundJob) error {
	// First clean up any cache_sync_pending commit. MarkCacheSynced uses the
	// claim's version, so a stale worker cannot acknowledge another transition.
	if !j.CacheSynced {
		if err := w.Hooks.Sync(ctx, j); err != nil {
			return w.deferJob(ctx, j)
		}
		if err := w.Repository.MarkCacheSynced(ctx, j.ID, j.Version); err != nil {
			return err
		}
		fresh, err := w.Repository.Get(ctx, j.ID)
		if err != nil {
			return err
		}
		j = fresh
	}
	switch j.Status {
	case AIBackgroundSettled, AIBackgroundReleased:
		return nil
	case AIBackgroundAdmitted:
		// Reserve grants the live admission handler two minutes to finish cache
		// invalidation and mark submitting. Recovery only releases provably unsent
		// admitted rows, never resubmits them.
		if err := w.Repository.ReleaseUnsubmitted(ctx, j.ID, j.Version); err != nil {
			return err
		}
	case AIBackgroundSubmitting:
		return w.Repository.MarkSubmissionUnknown(ctx, j.ID, j.Version)
	case AIBackgroundSubmissionUnknown:
		return w.deferJob(ctx, j)
	case AIBackgroundActive, AIBackgroundUsageUnknown:
		terminal, done, err := w.Hooks.Fetch(ctx, j)
		if err != nil || !done {
			return w.deferJob(ctx, j)
		}
		if err = w.Repository.SaveTerminal(ctx, j.ID, j.Version, terminal, time.Now().Add(time.Minute)); err != nil {
			return err
		}
		fresh, err := w.Repository.Get(ctx, j.ID)
		if err != nil {
			return err
		}
		j = fresh
		if !terminal.UsageKnown {
			return nil
		}
		fallthrough
	case AIBackgroundReady:
		cmd, err := w.Hooks.BuildCommand(ctx, j)
		if err != nil {
			return w.deferJob(ctx, j)
		}
		if _, err = w.Repository.Settle(ctx, j.ID, j.Version, cmd); err != nil {
			return err
		}
	default:
		return ErrAIBackgroundConflict
	}
	fresh, err := w.Repository.Get(ctx, j.ID)
	if err != nil {
		return err
	}
	j = fresh
	if err = w.Hooks.Sync(ctx, j); err != nil {
		return w.deferJob(ctx, j)
	}
	return w.Repository.MarkCacheSynced(ctx, j.ID, j.Version)
}

func (w *AIRoutingBackgroundWorker) deferJob(ctx context.Context, j *AIRoutingBackgroundJob) error {
	if err := w.Repository.Reschedule(ctx, j.ID, j.Version, time.Now().Add(time.Minute)); err != nil {
		return err
	}
	return ErrAIBackgroundRecoveryPending
}
