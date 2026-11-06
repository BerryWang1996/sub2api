package repository

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type aiSubmissionRoundTrip func(*http.Request) (*http.Response, error)

func (f aiSubmissionRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAIBackgroundSubmissionTransportCannotReplayUnknownPOST(t *testing.T) {
	calls, checks := 0, 0
	ctx := service.WithAIRoutingBackgroundSubmission(context.Background(), func(req *http.Request, account int64) error {
		checks++
		require.EqualValues(t, 7, account)
		copy, err := req.GetBody()
		require.NoError(t, err)
		body, err := io.ReadAll(copy)
		require.NoError(t, err)
		require.NoError(t, copy.Close())
		require.Equal(t, `{"model":"chosen","background":true}`, string(body))
		return nil
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://synthetic.invalid/v1/responses", strings.NewReader(`{"model":"chosen","background":true}`))
	require.NoError(t, err)
	base := &http.Client{Transport: aiSubmissionRoundTrip(func(wire *http.Request) (*http.Response, error) {
		calls++
		require.Nil(t, wire.GetBody, "underlying net/http must not replay the POST")
		return nil, errors.New("ambiguous upstream connection loss")
	})}
	client := withAIBackgroundSubmissionTransport(base, req, 7)
	_, err = client.Transport.RoundTrip(req)
	require.ErrorContains(t, err, "ambiguous")
	_, err = client.Transport.RoundTrip(req)
	require.True(t, service.IsAIRoutingCapacityError(err), "retry is a local refusal, never a second POST")
	require.Equal(t, 1, checks)
	require.Equal(t, 1, calls)
	require.NotNil(t, req.GetBody, "the caller's retained request remains intact")
	require.IsType(t, aiSubmissionRoundTrip(nil), base.Transport, "shared client is not mutated")
}

func TestAIBackgroundSubmissionFailsBeforeNetworkAndKeepsOrdinaryTransport(t *testing.T) {
	calls := 0
	base := &http.Client{Transport: aiSubmissionRoundTrip(func(*http.Request) (*http.Response, error) { calls++; return nil, nil })}
	ctx := service.WithAIRoutingBackgroundSubmission(context.Background(), func(*http.Request, int64) error { return errors.New("durable reserve failed") })
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://synthetic.invalid/v1/responses", strings.NewReader(`{}`))
	client := withAIBackgroundSubmissionTransport(base, req, 7)
	_, err := client.Transport.RoundTrip(req)
	require.True(t, service.IsAIRoutingCapacityError(err))
	require.Zero(t, calls)
	ordinary := req.WithContext(context.Background())
	require.Same(t, base, withAIBackgroundSubmissionTransport(base, ordinary, 7))
}
