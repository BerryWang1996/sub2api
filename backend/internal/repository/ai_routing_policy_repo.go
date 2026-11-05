package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type aiRoutingPolicyRepo struct{ db *sql.DB }

// Published is the hot-path read. Draft prompts are never loaded here. The
// persisted group status also protects a request using a stale key snapshot.
func (r *aiRoutingPolicyRepo) Published(ctx context.Context, id int64) (*service.AIRoutingPolicy, error) {
	var mode, active bool
	var data []byte
	err := r.db.QueryRowContext(ctx, `SELECT g.fork_ai_routing, g.status='active', p.published FROM groups g
 LEFT JOIN fork_ai_routing_policies p ON p.group_id=g.id WHERE g.id=$1 AND g.deleted_at IS NULL`, id).Scan(&mode, &active, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrGroupNotFound
	}
	if err != nil {
		return nil, err
	}
	if !mode {
		return nil, nil
	}
	p := &service.AIRoutingPolicy{GroupID: id}
	if !active || len(data) == 0 {
		return p, nil
	}
	if err := json.Unmarshal(data, p); err != nil {
		return nil, fmt.Errorf("decode AI routing publication: %w", err)
	}
	p.GroupID = id
	return p, nil
}

func NewAIRoutingPolicyRepository(db *sql.DB) service.AIRoutingPolicyRepository {
	return &aiRoutingPolicyRepo{db}
}
func (r *aiRoutingPolicyRepo) Load(ctx context.Context, id int64) (*service.AIRoutingPolicyState, error) {
	var mode bool
	var rev int64
	var draft, published []byte
	e := r.db.QueryRowContext(ctx, `SELECT g.fork_ai_routing, COALESCE(p.revision,0), p.draft,p.published
 FROM groups g LEFT JOIN fork_ai_routing_policies p ON p.group_id=g.id WHERE g.id=$1 AND g.deleted_at IS NULL`, id).Scan(&mode, &rev, &draft, &published)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, service.ErrGroupNotFound
	}
	if e != nil {
		return nil, e
	}
	if !mode {
		return nil, nil
	}
	st := &service.AIRoutingPolicyState{Revision: rev, Draft: service.NewAIRoutingDraft(id)}
	if rev > 0 {
		if e = json.Unmarshal(draft, &st.Draft); e != nil {
			return nil, fmt.Errorf("decode AI routing draft: %w", e)
		}
		st.Draft.GroupID = id
		st.Draft.Enabled = false
	}
	if len(published) > 0 {
		if e = json.Unmarshal(published, &st.Published); e != nil {
			return nil, fmt.Errorf("decode published AI routing policy: %w", e)
		}
		if st.Published != nil {
			st.Published.GroupID = id
		}
	}
	return st, nil
}
func (r *aiRoutingPolicyRepo) Write(ctx context.Context, id, expected int64, draft service.AIRoutingPolicy, published *service.AIRoutingPolicy, status string) (*service.AIRoutingPolicyState, error) {
	if expected < 0 {
		return nil, service.ErrAIRevision
	}
	d, e := json.Marshal(draft)
	if e != nil {
		return nil, e
	}
	var p any
	if published != nil {
		b, e := json.Marshal(published)
		if e != nil {
			return nil, e
		}
		p = string(b)
	}
	var rev int64
	tx, e := r.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer func() { _ = tx.Rollback() }()
	var mode bool
	e = tx.QueryRowContext(ctx, `SELECT fork_ai_routing FROM groups WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, id).Scan(&mode)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, service.ErrGroupNotFound
	}
	if e != nil {
		return nil, e
	}
	if !mode {
		return nil, service.ErrNotAIGroup
	}
	// First-write INSERT and subsequent UPDATE deliberately use different SQL so
	// a deleted/missing row can never be recreated as a successful stale edit.
	if expected == 0 {
		e = tx.QueryRowContext(ctx, `INSERT INTO fork_ai_routing_policies(group_id,revision,draft,published)
 SELECT id,1,$2::jsonb,$3::jsonb FROM groups WHERE id=$1 AND fork_ai_routing AND deleted_at IS NULL
 ON CONFLICT (group_id) DO NOTHING RETURNING revision`, id, string(d), p).Scan(&rev)
	} else {
		e = tx.QueryRowContext(ctx, `UPDATE fork_ai_routing_policies p SET revision=p.revision+1,draft=$3::jsonb,published=$4::jsonb,updated_at=NOW()
 FROM groups g WHERE p.group_id=$1 AND p.revision=$2 AND g.id=p.group_id AND g.fork_ai_routing AND g.deleted_at IS NULL RETURNING p.revision`, id, expected, string(d), p).Scan(&rev)
	}
	if errors.Is(e, sql.ErrNoRows) {
		return nil, service.ErrAIRevision
	}
	if e != nil {
		return nil, e
	}
	if status != "" {
		if _, e := tx.ExecContext(ctx, `UPDATE groups SET status=$1,updated_at=NOW() WHERE id=$2`, status, id); e != nil {
			return nil, e
		}
		if e := enqueueSchedulerOutbox(ctx, tx, service.SchedulerOutboxEventGroupChanged, nil, &id, nil); e != nil {
			return nil, e
		}
	}
	if e := tx.Commit(); e != nil {
		return nil, e
	}
	return &service.AIRoutingPolicyState{Revision: rev, Draft: draft, Published: published}, nil
}
