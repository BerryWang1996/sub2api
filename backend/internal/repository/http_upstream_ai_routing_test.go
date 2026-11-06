package repository

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestHTTPUpstreamAIRoutingGuardBeforeNetwork(t *testing.T) {
	for _, mode := range []string{"http", "tls-nil", "tls-http", "tls-https"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int64
			handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusOK)
			})
			server := httptest.NewUnstartedServer(handler)
			if mode == "tls-https" {
				server.StartTLS()
			} else {
				server.Start()
			}
			t.Cleanup(server.Close)
			denied := &service.AIRoutingCapacityError{Check: service.AIRoutingCapabilityCheck{Reason: "execution_context_exceeded"}}
			checks := 0
			ctx := service.WithAIRoutingOutboundGuard(t.Context(), func(req *http.Request, accountID int64) error {
				checks++
				require.Equal(t, int64(41), accountID)
				replay, err := req.GetBody()
				require.NoError(t, err)
				body, err := io.ReadAll(replay)
				require.NoError(t, err)
				require.NoError(t, replay.Close())
				require.Equal(t, `{"model":"final","max_tokens":128}`, string(body))
				return denied
			})
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader(`{"model":"final","max_tokens":128}`))
			require.NoError(t, err)
			upstream := NewHTTPUpstream(nil)
			var response *http.Response
			if mode == "http" {
				response, err = upstream.Do(req, "", 41, 1)
			} else {
				var profile *tlsfingerprint.Profile
				if mode != "tls-nil" {
					profile = &tlsfingerprint.Profile{Name: "must-not-acquire-client"}
				}
				response, err = upstream.DoWithTLS(req, "", 41, 1, profile)
			}
			require.Same(t, denied, err)
			require.Nil(t, response)
			require.Equal(t, 1, checks)
			require.Zero(t, calls.Load())
			body, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			require.NoError(t, req.Body.Close())
			require.Equal(t, `{"model":"final","max_tokens":128}`, string(body), "guard must not consume or alter the signed body")
		})
	}
}

func TestHTTPUpstreamAIRoutingGuardRechecksRebuiltRequest(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)
	denied := &service.AIRoutingCapacityError{Check: service.AIRoutingCapabilityCheck{Reason: "execution_output_limit_exceeded"}}
	checks := 0
	ctx := service.WithAIRoutingOutboundGuard(t.Context(), func(req *http.Request, _ int64) error {
		checks++
		replay, err := req.GetBody()
		require.NoError(t, err)
		body, err := io.ReadAll(replay)
		require.NoError(t, err)
		require.NoError(t, replay.Close())
		if strings.Contains(string(body), "2048") {
			return denied
		}
		return nil
	})
	upstream := NewHTTPUpstream(nil)
	first, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader(`{"max_tokens":128}`))
	require.NoError(t, err)
	resp, err := upstream.Do(first, "", 41, 1)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	retry, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader(`{"max_tokens":2048}`))
	require.NoError(t, err)
	resp, err = upstream.Do(retry, "", 41, 1)
	require.Same(t, denied, err)
	require.Nil(t, resp)
	require.NoError(t, retry.Body.Close())
	require.Equal(t, 2, checks)
	require.Equal(t, int64(1), calls.Load())
}

func TestHTTPUpstreamAIRoutingGuardRejectsRedirects(t *testing.T) {
	for _, tlsMode := range []bool{false, true} {
		for _, status := range []int{301, 302, 303, 307, 308} {
			t.Run(strconv.FormatBool(tlsMode)+"/"+strconv.Itoa(status), func(t *testing.T) {
				var initialCalls, targetCalls atomic.Int64
				var targetMethod atomic.Value
				const targetPath = "/v1beta/models/other:generateContent"
				server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if req.URL.Path == targetPath {
						targetCalls.Add(1)
						targetMethod.Store(req.Method)
						w.WriteHeader(http.StatusOK)
						return
					}
					initialCalls.Add(1)
					w.Header().Set("Location", targetPath)
					w.WriteHeader(status)
				}))
				if tlsMode {
					server.StartTLS()
				} else {
					server.Start()
				}
				t.Cleanup(server.Close)
				upstream, ok := NewHTTPUpstream(nil).(*httpUpstreamService)
				require.True(t, ok)
				client := server.Client()
				entry := installAIRoutingTestHTTPClient(upstream, client, tlsMode, 41)
				checks := 0
				ctx := service.WithAIRoutingOutboundGuard(t.Context(), func(_ *http.Request, accountID int64) error {
					checks++
					require.Equal(t, int64(41), accountID)
					return nil
				})
				if status == http.StatusFound {
					// Guarded execution must return a typed error even if another
					// caller also asks to stop redirects with ErrUseLastResponse.
					ctx = service.WithHTTPUpstreamRedirectsDisabled(ctx)
				}
				send := func(requestCtx context.Context) (*http.Response, error) {
					req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, server.URL+"/v1beta/models/chosen:generateContent", strings.NewReader(`{"contents":[{"parts":[{"text":"hello"}]}]}`))
					require.NoError(t, err)
					if tlsMode {
						return upstream.DoWithTLS(req, "", 41, 1, &tlsfingerprint.Profile{Name: "cached-test-client"})
					}
					return upstream.Do(req, "", 41, 1)
				}
				resp, err := send(ctx)
				require.Nil(t, resp)
				var refused *service.AIRoutingCapacityError
				require.ErrorAs(t, err, &refused)
				require.ErrorIs(t, err, refused)
				require.True(t, service.IsAIRoutingCapacityError(err))
				require.Equal(t, "execution_redirect_unsupported", refused.Check.Reason)
				require.Equal(t, 1, checks)
				require.Equal(t, int64(1), initialCalls.Load())
				require.Zero(t, targetCalls.Load(), "neither a changed URL model nor a downgraded GET may be sent")
				require.Zero(t, atomic.LoadInt64(&entry.inFlight))
				require.Nil(t, client.CheckRedirect, "the cached client is not mutated")

				resp, err = send(t.Context())
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
				require.Equal(t, http.StatusOK, resp.StatusCode)
				require.Equal(t, int64(1), targetCalls.Load(), "legacy requests retain redirect behavior")
				if status == 307 || status == 308 {
					require.Equal(t, http.MethodPost, targetMethod.Load())
				} else {
					require.Equal(t, http.MethodGet, targetMethod.Load())
				}
				require.Equal(t, 1, checks)
				require.Zero(t, atomic.LoadInt64(&entry.inFlight))
			})
		}
	}
}

func TestHTTPUpstreamAIRoutingGuardRechecksGrokFallback(t *testing.T) {
	for _, mode := range []string{"policy", "cancel", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			originalCtx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx := context.WithoutCancel(originalCtx)
			denied := &service.AIRoutingCapacityError{Check: service.AIRoutingCapabilityCheck{Reason: "execution_policy_changed"}}
			if mode == "cancel" {
				denied.Check.Reason = "execution_cancelled"
			}
			checks, calls := 0, 0
			revoked := false
			if mode != "legacy" {
				ctx = service.WithAIRoutingOutboundGuard(ctx, func(req *http.Request, accountID int64) error {
					checks++
					require.Equal(t, int64(41), accountID)
					require.NoError(t, req.Context().Err(), "stream transports can detach cancellation")
					if revoked || originalCtx.Err() != nil {
						require.Equal(t, grokOfficialAPIHost, req.URL.Hostname())
						return denied
					}
					return nil
				})
			}
			upstream, ok := NewHTTPUpstream(nil).(*httpUpstreamService)
			require.True(t, ok)
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				require.NoError(t, req.Body.Close())
				status := http.StatusOK
				body := `{}`
				if calls == 1 {
					require.Equal(t, grokCLIProxyHost, req.URL.Hostname())
					if mode == "cancel" {
						cancel()
					} else {
						revoked = true
					}
					status, body = http.StatusForbidden, `{"error":"Access denied"}`
				} else {
					require.Equal(t, grokOfficialAPIHost, req.URL.Hostname())
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			})}
			entry := installAIRoutingTestHTTPClient(upstream, client, false, 41)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+grokCLIProxyHost+"/v1/responses", strings.NewReader(`{"model":"grok-test","input":"hello"}`))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer synthetic-test-token")
			replay := &aiCapacityTrackedBody{Reader: strings.NewReader(`{"model":"grok-test","input":"hello"}`)}
			req.GetBody = func() (io.ReadCloser, error) { return replay, nil }
			resp, err := upstream.Do(req, "", 41, 1)
			if mode == "legacy" {
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, resp.StatusCode)
				require.NoError(t, resp.Body.Close())
				require.Equal(t, 2, calls)
				require.Zero(t, checks)
			} else {
				require.Nil(t, resp)
				require.ErrorIs(t, err, denied)
				require.True(t, service.IsAIRoutingCapacityError(err))
				require.Equal(t, 1, calls, "revoked or cancelled execution cannot use the fallback")
				require.Equal(t, 2, checks)
			}
			require.True(t, replay.closed)
			require.Zero(t, atomic.LoadInt64(&entry.inFlight))
		})
	}
}

func installAIRoutingTestHTTPClient(upstream *httpUpstreamService, client *http.Client, tlsMode bool, accountID int64) *upstreamClientEntry {
	isolation := upstream.getIsolationMode()
	profile := service.HTTPUpstreamProfileDefault
	protocolMode := upstream.resolveProtocolMode(profile, directProxyKey, nil)
	settings := upstream.applyProfilePoolSettings(upstream.resolvePoolSettings(isolation, 1), profile)
	cacheKey := buildCacheKey(isolation, directProxyKey, accountID, protocolMode)
	poolKey := buildPoolKey(settings, protocolMode)
	if tlsMode {
		cacheKey = "tls:" + buildCacheKey(isolation, directProxyKey, accountID, upstreamProtocolModeDefault)
		poolKey = buildPoolKey(settings, upstreamProtocolModeDefault) + ":tls"
	}
	entry := &upstreamClientEntry{client: client, proxyKey: directProxyKey, poolKey: poolKey, protocolMode: protocolMode}
	upstream.clients[cacheKey] = entry
	return entry
}

type aiCapacityTrackedBody struct {
	io.Reader
	closed bool
}

func (b *aiCapacityTrackedBody) Close() error {
	b.closed = true
	return nil
}
