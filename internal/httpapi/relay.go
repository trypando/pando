package httpapi

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/id"
	"github.com/trypando/pando/internal/log"
)

// relayHeader marks a request one replica passed to another. The replica that
// receives it serves it itself, whatever it would otherwise do, so a relay is
// one hop and never a loop. A client that sets it gets the receiving replica's
// own view, which is all it could get anyway.
const relayHeader = "Pando-Replica-Relay"

// DeployLogOwner says where a deploy's live log is: the base URL of the replica
// running it, or "" when that is this replica or no live replica holds it
// (issue #72). Nil with one replica.
type DeployLogOwner func(ctx context.Context, deploymentID string) (string, error)

// relayDeployLog passes a deploy-log request to the replica that holds the log,
// and reports whether it did.
//
// Passed on whole, credentials included, and authorized again there: the
// replica that holds the log decides for itself who may read it. Streaming,
// so each line arrives as it is written rather than when the deploy ends.
func (s *Server) relayDeployLog(w http.ResponseWriter, r *http.Request, depID string) bool {
	// Only a well-formed deployment ID — a prefix and a ULID, no control
	// characters — is relayed, and so only one reaches the log line below.
	// Anything else is not a deployment, and this replica answers it.
	if !id.Is(id.Deployment, depID) {
		return false
	}
	if s.LogOwner == nil || r.Header.Get(relayHeader) != "" || s.Logs.Has(depID) {
		return false
	}
	base, err := s.LogOwner(r.Context(), depID)
	if err != nil || base == "" {
		return false
	}
	target, err := url.Parse(base)
	if err != nil || target.Host == "" {
		return false
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = r.URL.Path
			pr.Out.URL.RawPath = r.URL.RawPath
			pr.Out.Host = r.Host
			pr.Out.Header.Set(relayHeader, "1")
		},
		// Flush every write: this is a server-sent event stream.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			log.From(req.Context()).Warn("could not reach the replica running the deploy",
				zap.String("deployment_id", depID), zap.String("replica", target.Host), zap.Error(err))
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
	return true
}
