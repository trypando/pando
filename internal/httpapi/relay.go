package httpapi

import (
	"context"
	"fmt"
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
		Transport: s.relayTransport(),
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

func (s *Server) relayTransport() http.RoundTripper {
	if s.RelayTransport != nil {
		return s.RelayTransport
	}
	return http.DefaultTransport
}

// relayDeployLogBody is relayDeployLog for a stream this replica has already
// started — a queued deploy whose place in the queue it was showing (issue
// #93). The owner's events are copied into it as they arrive; its status and
// headers are not, because they were sent. Reports whether it relayed.
func (s *Server) relayDeployLogBody(w http.ResponseWriter, r *http.Request, rc *http.ResponseController, depID string) bool {
	if !id.Is(id.Deployment, depID) || s.LogOwner == nil || s.Logs.Has(depID) {
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
		Transport: s.relayTransport(),
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = r.URL.Path
			pr.Out.URL.RawPath = r.URL.RawPath
			pr.Out.Host = r.Host
			pr.Out.Header.Set(relayHeader, "1")
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			log.From(req.Context()).Warn("could not reach the replica running a queued deploy",
				zap.String("replica", target.Host), zap.Error(err))
			fmt.Fprint(w, "data: Pando could not reach the Pando process running this deploy, so its log can't be shown here. The deploy's outcome will be on the deploy itself.\n\n")
			fmt.Fprint(w, "event: end\ndata: \n\n")
			_ = rc.Flush()
		},
	}
	proxy.ServeHTTP(&openStream{ResponseWriter: w, rc: rc, header: http.Header{}}, r)
	return true
}

// openStream is a response whose status and headers are already sent: the
// relay's are set aside, and only its body reaches the client.
type openStream struct {
	http.ResponseWriter
	rc     *http.ResponseController
	header http.Header
}

func (o *openStream) Header() http.Header { return o.header }
func (o *openStream) WriteHeader(int)     {}
func (o *openStream) Flush()              { _ = o.rc.Flush() }
