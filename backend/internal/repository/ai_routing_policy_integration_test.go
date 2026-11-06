//go:build integration

package repository

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAIRoutingPolicyPostgresLifecycle(t *testing.T) {
	ctx := context.Background()
	groups := newGroupRepositoryWithSQL(testEntClient(t), integrationDB)
	policies := NewAIRoutingPolicyRepository(integrationDB)
	source := &service.Group{Name: fmt.Sprintf("ai-policy-source-%d", time.Now().UnixNano()), Platform: service.PlatformComposite, AIRouting: true, Status: "inactive", RateMultiplier: 1, SubscriptionType: service.SubscriptionTypeStandard}
	require.NoError(t, groups.Create(ctx, source))
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := integrationDB.ExecContext(cleanup, "DELETE FROM scheduler_outbox WHERE group_id=$1", source.ID)
		require.NoError(t, err, "cleanup AI policy source scheduling events without a foreign key")
		_, err = integrationDB.ExecContext(cleanup, "DELETE FROM groups WHERE id=$1", source.ID)
		require.NoError(t, err, "cleanup AI policy source and cascading policy")
	})
	unpub, err := policies.Published(ctx, source.ID)
	require.NoError(t, err)
	require.NotNil(t, unpub)
	require.False(t, unpub.Enabled)

	// Two independent editors create their first draft from revision zero.
	start := make(chan struct{})
	written := make(chan error, 2)
	for _, prompt := range []string{"first editor", "second editor"} {
		go func(prompt string) {
			<-start
			draft := service.NewAIRoutingDraft(source.ID)
			draft.Prompt = prompt
			draft.Version = 1
			_, err := policies.Write(ctx, source.ID, 0, draft, nil, "")
			written <- err
		}(prompt)
	}
	close(start)
	err1, err2 := <-written, <-written
	require.True(t, (err1 == nil && errors.Is(err2, service.ErrAIRevision)) || (err2 == nil && errors.Is(err1, service.ErrAIRevision)), "competing edits: %v / %v", err1, err2)
	saved, err := policies.Load(ctx, source.ID)
	require.NoError(t, err)
	require.EqualValues(t, 1, saved.Revision)
	require.Nil(t, saved.Published)
	staleGroup, err := groups.GetByIDLite(ctx, source.ID)
	require.NoError(t, err)
	require.True(t, staleGroup.AIRouting)
	require.False(t, staleGroup.IsActive())

	publication := saved.Draft
	publication.Version = 2
	publication.Enabled = true
	_, err = policies.Write(ctx, source.ID, 1, saved.Draft, &publication, "active")
	require.NoError(t, err)
	staleGroup.Description = "metadata edit begun before publication"
	require.NoError(t, groups.Update(ctx, staleGroup))
	freshGroup, err := groups.GetByIDLite(ctx, source.ID)
	require.NoError(t, err)
	require.True(t, freshGroup.IsActive())
	actual, err := policies.Published(ctx, source.ID)
	require.NoError(t, err)
	require.True(t, actual.Enabled)
	require.Equal(t, publication.Prompt, actual.Prompt)

	// A duplicate carries only the draft, independent version, and an inactive group.
	duplicate := *freshGroup
	duplicate.ID = 0
	duplicate.Name = source.Name + "-copy"
	duplicate.Status = "inactive"
	require.NoError(t, groups.CreateFromSource(ctx, &duplicate, source.ID))
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := integrationDB.ExecContext(cleanup, "DELETE FROM scheduler_outbox WHERE group_id=$1", duplicate.ID)
		require.NoError(t, err, "cleanup copied AI group scheduling events without a foreign key")
		_, err = integrationDB.ExecContext(cleanup, "DELETE FROM groups WHERE id=$1", duplicate.ID)
		require.NoError(t, err, "cleanup copied AI group and cascading policy")
	})
	copyState, err := policies.Load(ctx, duplicate.ID)
	require.NoError(t, err)
	require.Nil(t, copyState.Published)
	require.EqualValues(t, 1, copyState.Revision)
	require.Equal(t, publication.Prompt, copyState.Draft.Prompt)
	require.Equal(t, duplicate.ID, copyState.Draft.GroupID)
	require.False(t, copyState.Draft.Enabled)
	p := publication
	p.Enabled = false
	p.Version = 3
	_, err = policies.Write(ctx, source.ID, 2, saved.Draft, &p, "inactive")
	require.NoError(t, err)
	actual, err = policies.Published(ctx, source.ID)
	require.NoError(t, err)
	require.False(t, actual.Enabled)
	freshGroup, err = groups.GetByIDLite(ctx, source.ID)
	require.NoError(t, err)
	require.False(t, freshGroup.IsActive())
	_, err = policies.Write(ctx, source.ID, 2, saved.Draft, &publication, "active")
	require.ErrorIs(t, err, service.ErrAIRevision)
}
