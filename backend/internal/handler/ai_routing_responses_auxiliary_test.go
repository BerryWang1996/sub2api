package handler

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAIResponsesCompactArtifactOriginAndReplay(t *testing.T) {
	key := &service.APIKey{ID: 3, UserID: 2, Group: &service.Group{ID: 1}}
	store := &aiResponsesMemoryStore{}
	origin := service.AIRoutingOriginBinding{Candidate: "c1", Account: 7, Model: "gpt-5.1", Platform: "openai", AccountTarget: 7, PublicModel: "auto", Native: true, ParentKind: "compaction"}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Header("Content-Type", "application/json")
	writer := newAIResponsesCompactWriter(c.Writer, func(kind, id, parent string) error {
		require.NotContains(t, rec.Body.String(), id)
		binding := origin
		binding.ResponseID = parent
		return saveAIResponsesControlBinding(t.Context(), store, key, service.AIRoutingOriginRef{Kind: kind, ID: id}, binding)
	}, nil)
	body := `{"id":"cmp_artifact","object":"response.compaction","output":[{"id":"cmp_item","type":"compaction","encrypted_content":"fake opaque"}]}`
	for _, part := range strings.Split(body, ",") {
		if len(writer.pending) > 0 {
			_, err := writer.WriteString(",")
			require.NoError(t, err)
		}
		_, err := writer.WriteString(part)
		require.NoError(t, err)
	}
	require.NoError(t, writer.finish())
	require.JSONEq(t, body, rec.Body.String())
	for _, ref := range []service.AIRoutingOriginRef{{Kind: "item", ID: "cmp_item"}, {Kind: "opaque", ID: service.AIRoutingOpaqueOriginID("fake opaque")}} {
		binding, parent, err := loadAIResponsesBinding(t.Context(), store, key, service.AIRoutingRequest{OriginRefs: []service.AIRoutingOriginRef{ref}, RequiresOrigin: true})
		require.NoError(t, err)
		require.EqualValues(t, 7, binding.Account)
		require.Equal(t, "compaction", parent.OriginParentKind())
		storageKey, err := aiResponsesOriginKey(key, ref)
		require.NoError(t, err)
		require.Equal(t, service.AIRoutingToolOriginTTL, store.ttls[storageKey])
		ref.RequiresStored = true
		_, _, err = loadAIResponsesBinding(t.Context(), store, key, service.AIRoutingRequest{OriginRefs: []service.AIRoutingOriginRef{ref}, RequiresOrigin: true})
		require.Error(t, err)
	}
	_, _, err := loadAIResponsesBinding(t.Context(), store, key, service.AIRoutingRequest{PreviousResponseID: "cmp_artifact", OriginRefs: []service.AIRoutingOriginRef{{Kind: "response", ID: "cmp_artifact", RequiresStored: true}}, RequiresOrigin: true})
	require.Error(t, err, "standalone compaction ID is not a stored Response")
	parentKey, _ := aiResponsesOriginKey(key, service.AIRoutingOriginRef{Kind: "compaction", ID: "cmp_artifact"})
	delete(store.rows, parentKey)
	_, _, err = loadAIResponsesBinding(t.Context(), store, key, service.AIRoutingRequest{OriginRefs: []service.AIRoutingOriginRef{{Kind: "item", ID: "cmp_item"}}, RequiresOrigin: true})
	require.Error(t, err, "missing parent never proves provenance")
}

func TestAIResponsesCompactWriterRejectsUnboundOrInvalidArtifacts(t *testing.T) {
	for _, body := range []string{
		`{"id":"cmp_a","object":"response.compaction","output":[{"type":"compaction","encrypted_content":"opaque"}]}`,
		`{"id":"cmp_a","object":"response","output":[]}`,
		`{"object":"response.compaction","output":[]}`,
		`{"id":"cmp_a","object":"response.compaction","Object":"response","output":[]}`,
	} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Header("Content-Type", "application/json")
		writer := newAIResponsesCompactWriter(c.Writer, func(string, string, string) error { return errors.New("store unavailable") }, nil)
		_, err := writer.WriteString(body)
		require.NoError(t, err)
		require.Error(t, writer.finish())
		require.Empty(t, rec.Body.String())
	}
}

func TestAIResponsesCompactRetainedItemsKeepPriorParent(t *testing.T) {
	key := &service.APIKey{ID: 3, UserID: 2, Group: &service.Group{ID: 1}}
	store := &aiResponsesMemoryStore{}
	origin := service.AIRoutingOriginBinding{Candidate: "c1", Account: 7, Model: "gpt-5.1", Platform: "openai", AccountTarget: 7, ResponseID: "resp_prior", Native: true, Store: true}
	ref := service.AIRoutingOriginRef{Kind: "item", ID: "item_retained"}
	require.NoError(t, saveAIResponsesBinding(t.Context(), store, key, service.AIRoutingOriginRef{Kind: "response", ID: "resp_prior"}, origin))
	require.NoError(t, saveAIResponsesBinding(t.Context(), store, key, ref, origin))
	compacted := origin
	compacted.ResponseID = "cmp_artifact"
	compacted.ParentKind = "compaction"
	compacted.Store = false
	require.NoError(t, saveAIResponsesBinding(t.Context(), store, key, service.AIRoutingOriginRef{Kind: "compaction", ID: "cmp_artifact"}, compacted))
	require.NoError(t, saveAIResponsesControlBinding(t.Context(), store, key, ref, compacted))
	_, loaded, err := loadAIResponsesBinding(t.Context(), store, key, service.AIRoutingRequest{RequiresOrigin: true, OriginRefs: []service.AIRoutingOriginRef{ref}})
	require.NoError(t, err)
	require.Equal(t, "resp_prior", loaded.ResponseID)
	priorKey, _ := aiResponsesOriginKey(key, service.AIRoutingOriginRef{Kind: "response", ID: "resp_prior"})
	store.rows[priorKey] = "revoked"
	require.Error(t, saveAIResponsesControlBinding(t.Context(), store, key, ref, compacted))
}
