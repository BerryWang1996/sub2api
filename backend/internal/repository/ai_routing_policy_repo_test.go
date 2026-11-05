package repository

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAIRoutingPolicyPublishedUsesLiveGroupStatusAndNeverLoadsDraft(t *testing.T) {
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	repo := NewAIRoutingPolicyRepository(db)
	for _, tt := range []struct {
		mode, active               bool
		publication                []byte
		isStatic, enabled, wantErr bool
	}{
		{false, true, nil, true, false, false},
		{true, true, nil, false, false, false},
		{true, false, []byte(`{"enabled":true}`), false, false, false},
		{true, true, []byte(`{"enabled":true}`), false, true, false},
		{true, true, []byte(`{`), false, false, true},
	} {
		m.ExpectQuery("SELECT g.fork_ai_routing, g.status='active', p.published").WithArgs(int64(7)).WillReturnRows(sqlmock.NewRows([]string{"mode", "active", "published"}).AddRow(tt.mode, tt.active, tt.publication))
		p, err := repo.Published(context.Background(), 7)
		if tt.wantErr {
			require.Error(t, err)
			continue
		}
		require.NoError(t, err)
		require.Equal(t, tt.isStatic, p == nil)
		if p != nil {
			require.Equal(t, tt.enabled, p.Enabled)
			require.EqualValues(t, 7, p.GroupID)
		}
	}
	require.NoError(t, m.ExpectationsWereMet())
}

func TestAIRoutingPolicyLoadDistinguishesStaticUnpublishedAndCorrupt(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer func() { _ = db.Close() }()
	repo := NewAIRoutingPolicyRepository(db)
	ctx := context.Background()
	cols := []string{"fork_ai_routing", "revision", "draft", "published"}
	for _, tt := range []struct {
		mode             bool
		rev              int64
		draft, published any
		wantNil, wantErr bool
	}{
		{false, 0, nil, nil, true, false}, {true, 0, nil, nil, false, false}, {true, 4, []byte("{"), nil, true, true}, {true, 4, []byte("{}"), []byte("{"), true, true},
	} {
		m.ExpectQuery("SELECT g.fork_ai_routing").WithArgs(int64(42)).WillReturnRows(sqlmock.NewRows(cols).AddRow(tt.mode, tt.rev, tt.draft, tt.published))
		st, e := repo.Load(ctx, 42)
		require.Equal(t, tt.wantErr, e != nil)
		require.Equal(t, tt.wantNil, st == nil)
		if st != nil {
			require.EqualValues(t, 42, st.Draft.GroupID)
			require.Nil(t, st.Published)
		}
	}
	require.NoError(t, m.ExpectationsWereMet())
}

func TestAIRoutingPolicyWriteRollsBackLostRevisionOrStatusFailure(t *testing.T) {
	for _, stale := range []bool{true, false} {
		db, m, e := sqlmock.New()
		require.NoError(t, e)
		repo := NewAIRoutingPolicyRepository(db)
		p := service.NewAIRoutingDraft(8)
		m.ExpectBegin()
		m.ExpectQuery("SELECT fork_ai_routing FROM groups").WithArgs(int64(8)).WillReturnRows(sqlmock.NewRows([]string{"mode"}).AddRow(true))
		w := m.ExpectQuery("UPDATE fork_ai_routing_policies").WithArgs(int64(8), int64(3), sqlmock.AnyArg(), sqlmock.AnyArg())
		if stale {
			w.WillReturnError(sql.ErrNoRows)
		} else {
			w.WillReturnRows(sqlmock.NewRows([]string{"revision"}).AddRow(int64(4)))
			m.ExpectExec("UPDATE groups SET status=").WithArgs("active", int64(8)).WillReturnError(errors.New("failed to update group"))
		}
		m.ExpectRollback()
		_, e = repo.Write(context.Background(), 8, 3, p, &p, "active")
		require.Error(t, e)
		if stale {
			require.ErrorIs(t, e, service.ErrAIRevision)
		}
		require.NoError(t, m.ExpectationsWereMet())
		_ = db.Close()
	}
}
