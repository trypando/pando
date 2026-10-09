package httpapi

import (
	"context"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/id"
	"github.com/trypando/pando/internal/log"
)

type ctxKeyRequestID struct{}

// RequestIDHeader is returned on every response, including errors.
const RequestIDHeader = "X-Request-Id"

// RequestID stamps every request with an identifier and puts it in the context
// and the response. The error envelope carries it too, so a user reporting a
// failure hands over one string that finds the log line.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := id.New(id.Request)
		w.Header().Set(RequestIDHeader, rid)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID{}, rid)))
	})
}

// RequestIDFrom returns the request ID in ctx, or "".
func RequestIDFrom(ctx context.Context) string {
	rid, _ := ctx.Value(ctxKeyRequestID{}).(string)
	return rid
}

// Logger threads the request's logger through context, carrying request_id
// (design 00 §3.3). principal_id is added by the authentication middleware in
// phase 1, and app_id by handlers that resolve one.
func Logger(base *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			ctx := log.Into(r.Context(), base.With(zap.String("request_id", RequestIDFrom(r.Context()))))

			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r.WithContext(ctx))

			log.From(ctx).Info("request",
				zap.String("method", r.Method),
				// The path is whatever the caller typed, and it reaches here
				// percent-decoded.
				log.Untrusted("path", r.URL.Path),
				zap.Int("status", rec.status),
				zap.Duration("took", time.Since(started)),
			)
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.written {
		return
	}
	r.status = code
	r.written = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.written = true
	// Forwarded as an io.Writer rather than an http.ResponseWriter. CodeQL
	// resolves every io.Writer.Write in the binary to this method, so a
	// ResponseWriter.Write here made it a reflected-XSS sink for bytes that
	// never reach a response: `pando exec` writing a session to the user's
	// terminal was the reported path. A handler's own write is still a sink
	// where the handler makes it, which is where an XSS would be.
	var out io.Writer = r.ResponseWriter
	return out.Write(b)
}

// Unwrap exposes the underlying writer so http.ResponseController can reach
// Flush and Hijack. The proxy depends on both (R-170), and a recorder that
// hides them would break streaming in a way that only shows up at runtime.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
