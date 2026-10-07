package errs_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

func TestStatusMappingByPrefix(t *testing.T) {
	for code, want := range map[errs.Code]int{
		errs.AuthRequired:                http.StatusUnauthorized,
		errs.AuthTokenOrphaned:           http.StatusUnauthorized,
		errs.PermDenied:                  http.StatusForbidden,
		errs.PolicySourceNotAllowed:      http.StatusForbidden,
		errs.ValidPrimaryWorkload:        http.StatusBadRequest,
		errs.PlanSlotUnfilled:            http.StatusConflict,
		errs.StateBackupDecisionRequired: http.StatusConflict,
		errs.CapacityWouldOversubscribe:  http.StatusConflict,
		errs.AdapterUnavailable:          http.StatusBadGateway,
		errs.BuildFailed:                 http.StatusUnprocessableEntity,
		errs.BackupIncomplete:            http.StatusUnprocessableEntity,
		errs.NotFound:                    http.StatusNotFound,
		errs.Internal:                    http.StatusInternalServerError,
		errs.RateLimited:                 http.StatusTooManyRequests,
	} {
		require.Equal(t, want, errs.New(code, "x").Status(), "wrong status for %s", code)
	}
}

// An unknown code must not be given a plausible-looking status. Guessing hides
// the bug that produced it.
func TestUnknownCodeIs500(t *testing.T) {
	require.Equal(t, http.StatusInternalServerError, errs.New(errs.Code("MADE_UP"), "x").Status())
}

// R-132 promises a readable plan-time failure. The envelope has to carry enough
// for the console to render the remedy, and details to name each unfilled slot.
func TestR132_SlotUnfilledCarriesRemedyAndDetails(t *testing.T) {
	err := errs.New(errs.PlanSlotUnfilled, "This app needs a Redis, and one hasn't been chosen yet.").
		WithRemedy("Choose how to fill the REDIS_URL slot: provision one inside this app, connect to an existing Redis, or paste a connection string.").
		WithDetail("slots", []map[string]string{{"key": "REDIS_URL", "type": "redis"}}).
		WithRequestID("req_01HQ8")

	require.Equal(t, http.StatusConflict, err.Status())

	var got map[string]any
	b, e := json.Marshal(err)
	require.NoError(t, e)
	require.NoError(t, json.Unmarshal(b, &got))

	require.Equal(t, "PLAN_SLOT_UNFILLED", got["code"])
	require.NotEmpty(t, got["remedy"])
	require.NotEmpty(t, got["details"])
	require.Equal(t, "req_01HQ8", got["request_id"])
}

// The internal cause is for logs. Serializing it leaks implementation detail
// into a response body.
func TestWrappedCauseIsNotSerialized(t *testing.T) {
	cause := fmt.Errorf("dial tcp 10.0.0.5:5432: connection refused")
	err := errs.Wrap(errs.AdapterUnavailable, "The Docker runtime isn't responding.", cause)

	b, e := json.Marshal(err)
	require.NoError(t, e)
	require.NotContains(t, string(b), "10.0.0.5")

	require.Contains(t, err.Error(), "connection refused", "the cause should still reach a log")
	require.ErrorIs(t, err, cause)
}

func TestAsAndCodeOf(t *testing.T) {
	err := errs.New(errs.NotFound, "no such app")
	wrapped := fmt.Errorf("looking up app: %w", err)

	require.Equal(t, errs.NotFound, errs.CodeOf(wrapped))
	require.Equal(t, err, errs.As(wrapped))

	require.Equal(t, errs.Internal, errs.CodeOf(fmt.Errorf("bare error")))
	require.Nil(t, errs.As(fmt.Errorf("bare error")))
}

// R-194 reaches the error envelope too: a secret in Details must not serialize.
func TestR194_SecretInDetailsDoesNotSerialize(t *testing.T) {
	err := errs.New(errs.AdapterFailed, "connection failed").
		WithDetail("credential", secret.New("hunter2-THE-ACTUAL-SECRET"))

	b, e := json.Marshal(err)
	require.NoError(t, e)
	require.NotContains(t, string(b), "hunter2-THE-ACTUAL-SECRET")
	require.Contains(t, string(b), secret.Redacted)
}
