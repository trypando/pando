package httpapi

import (
	"encoding/json"
	"net/http"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/log"
)

// JSON writes a successful response.
func JSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

// Error writes the error envelope (design 00 §3.2).
//
// An error arriving without an envelope becomes INTERNAL and its detail is
// logged rather than returned: an unenveloped error is by definition one nobody
// wrote a user-facing message for, and guessing one would leak implementation
// detail into a response body.
func Error(w http.ResponseWriter, r *http.Request, err error) {
	ctx := r.Context()

	e := errs.As(err)
	if e == nil {
		log.From(ctx).Error("unenveloped error reached the API boundary", untrustedError(err))
		e = errs.New(errs.Internal, "Something went wrong. The error has been logged.")
	}
	e = e.WithRequestID(RequestIDFrom(ctx))

	if e.Status() >= 500 {
		log.From(ctx).Error("request failed", log.Untrusted("code", string(e.Code)), untrustedError(err))
	} else {
		log.From(ctx).Info("request rejected", log.Untrusted("code", string(e.Code)))
	}

	JSON(w, e.Status(), e)
}

// untrustedError is zap.Error for an error whose text may carry request input:
// a path parameter that reached a lookup comes back inside its not-found error.
// The code goes through log.Untrusted too — it is always a catalog constant, but
// an analyzer tracking err cannot see that past errs.As.
func untrustedError(err error) zap.Field {
	return log.Untrusted("error", err.Error())
}
