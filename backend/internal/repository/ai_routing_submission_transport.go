package repository

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type aiBackgroundSubmissionTransport struct {
	base      http.RoundTripper
	accountID int64
}

func withAIBackgroundSubmissionTransport(client *http.Client, req *http.Request, accountID int64) *http.Client {
	if !service.HasAIRoutingBackgroundSubmission(req.Context()) {
		return client
	}
	copy := *client
	base := copy.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	copy.Transport = &aiBackgroundSubmissionTransport{base: base, accountID: accountID}
	return &copy
}

func (t *aiBackgroundSubmissionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := service.BeginAIRoutingBackgroundSubmission(req, t.accountID); err != nil {
		return nil, err
	}
	// net/http may internally replay idempotent POST bodies on a failed pooled
	// connection. Submission uncertainty must instead remain a durable job.
	wire := req.Clone(req.Context())
	wire.GetBody = nil
	return t.base.RoundTrip(wire)
}
