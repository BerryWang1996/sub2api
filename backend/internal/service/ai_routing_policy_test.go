package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type aiPolicyStoreStub struct {
	st     *AIRoutingPolicyState
	err    error
	action string
}

func (s *aiPolicyStoreStub) Published(ctx context.Context, id int64) (*AIRoutingPolicy, error) {
	st, err := s.Load(ctx, id)
	if err != nil || st == nil {
		return nil, err
	}
	if st.Published == nil {
		return &AIRoutingPolicy{GroupID: id}, nil
	}
	return st.Published, nil
}

func (s *aiPolicyStoreStub) Load(context.Context, int64) (*AIRoutingPolicyState, error) {
	return s.st, s.err
}
func (s *aiPolicyStoreStub) Write(_ context.Context, _ int64, rev int64, d AIRoutingPolicy, p *AIRoutingPolicy, status string) (*AIRoutingPolicyState, error) {
	if rev != s.st.Revision {
		return nil, ErrAIRevision
	}
	s.action = status
	s.st = &AIRoutingPolicyState{Draft: d, Published: p, Revision: rev + 1}
	return s.st, nil
}
func TestAIRoutingPolicyDraftCannotActivateOrReplaceLiveVersion(t *testing.T) {
	ctx := context.Background()
	source := int64(5)
	live := NewAIRoutingDraft(source)
	live.Prompt = "published"
	live.Version = 3
	live.Enabled = true
	store := &aiPolicyStoreStub{st: &AIRoutingPolicyState{Draft: live, Published: &live, Revision: 3}}
	s := &AIRoutingPolicyService{repo: store}
	forged := live
	forged.Prompt = "edited"
	forged.GroupID = 9
	forged.Version = 200
	forged.Enabled = true
	_, err := s.SaveDraft(ctx, source, 2, forged)
	require.ErrorIs(t, err, ErrAIRevision)
	st, err := s.SaveDraft(ctx, source, 3, forged)
	require.NoError(t, err)
	require.Equal(t, "published", st.Published.Prompt)
	require.True(t, st.Published.Enabled)
	require.EqualValues(t, 3, st.Published.Version)
	require.EqualValues(t, 4, st.Revision)
	require.Equal(t, source, st.Draft.GroupID)
	require.False(t, st.Draft.Enabled)
	require.Empty(t, store.action)
	st, err = s.Disable(ctx, source, 4)
	require.NoError(t, err)
	require.Equal(t, "inactive", store.action)
	require.False(t, st.Published.Enabled)
	require.Equal(t, "edited", st.Draft.Prompt)
}
func TestAIRoutingPolicyModeCannotFallThroughOnMissingPublication(t *testing.T) {
	store := &aiPolicyStoreStub{}
	s := &AIRoutingPolicyService{repo: store}
	p, e := s.GetPublished(context.Background(), 1)
	require.NoError(t, e)
	require.Nil(t, p)
	store.st = &AIRoutingPolicyState{Draft: NewAIRoutingDraft(1)}
	p, e = s.GetPublished(context.Background(), 1)
	require.NoError(t, e)
	require.NotNil(t, p)
	require.False(t, p.Enabled)
	store.err = errors.New("database unavailable")
	_, e = s.GetPublished(context.Background(), 1)
	require.Error(t, e)
}
func TestAIRoutingPolicyRejectsConflictingDestinationsAndDoesNotDiscloseMetadata(t *testing.T) {
	p := NewAIRoutingDraft(7)
	dst := int64(3)
	c := AIRoutingCandidate{ID: "a", Platform: "openai", Model: "custom", TargetGroupID: &dst}
	p.Candidates = []AIRoutingCandidate{c}
	require.NoError(t, validateAIRoutingDraft(p))
	p.Candidates[0].TargetAccountID = &dst
	require.Error(t, validateAIRoutingDraft(p))
	p.Candidates = []AIRoutingCandidate{c, c}
	p.Candidates[1].ID = "b"
	require.Error(t, validateAIRoutingDraft(p))
	require.NotContains(t, AIRoutingSampleTask("metadata", "private task"), "private")
	require.NotContains(t, AIRoutingSampleTask("task_excerpt", "please use sk-abcdefghijklmnopqrst0123456 to help"), "abcdefghijkl")
}

type aiPolicyCacheInvalidator struct {
	APIKeyAuthCacheInvalidator
	groups []int64
}

func (c *aiPolicyCacheInvalidator) InvalidateAuthCacheByGroupID(_ context.Context, id int64) {
	c.groups = append(c.groups, id)
}

func TestAIRoutingPolicyInvalidatesAuthOnlyAfterPublicationWrite(t *testing.T) {
	draft := NewAIRoutingDraft(7)
	store := &aiPolicyStoreStub{st: &AIRoutingPolicyState{Draft: draft}}
	cache := &aiPolicyCacheInvalidator{}
	svc := NewAIRoutingPolicyService(store, nil, nil, nil, cache)
	_, err := svc.SaveDraft(context.Background(), 7, 0, draft)
	require.NoError(t, err)
	require.Empty(t, cache.groups)
	_, err = svc.Disable(context.Background(), 7, 1)
	require.NoError(t, err)
	require.Equal(t, []int64{7}, cache.groups)
	_, err = svc.Disable(context.Background(), 7, 1)
	require.ErrorIs(t, err, ErrAIRevision)
	require.Equal(t, []int64{7}, cache.groups)
}

func TestAIRoutingPolicyDuplicateKeepsModeAndStartsInactive(t *testing.T) {
	source := &Group{ID: 7, Name: "AI source", AIRouting: true, Platform: PlatformComposite, Status: StatusActive}
	copy := cloneGroupForDuplicate(source, "operation")
	require.True(t, copy.AIRouting)
	require.Equal(t, PlatformComposite, copy.Platform)
	require.Equal(t, "inactive", copy.Status)
	require.Zero(t, copy.ID)
}
