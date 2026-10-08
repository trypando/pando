package httpapi

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/authz"
	corepolicy "github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// unreachableAdapters is an adapter store whose database cannot be reached:
// the pool connects lazily, to a port nothing listens on.
func unreachableAdapters(t *testing.T) *state.Adapters {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://pando@127.0.0.1:1/pando?connect_timeout=1")
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return state.NewAdapters(&state.DB{Pool: pool})
}

// TestR385_AnAdapterSaveThatCannotTellIfItIsASinkIsRefused asserts the
// gate's failure side: when the stored adapters cannot be read, a save under
// another category cannot tell whether it would overwrite an audit sink, so
// it is refused rather than let through unchecked.
func TestR385_AnAdapterSaveThatCannotTellIfItIsASinkIsRefused(t *testing.T) {
	srv := &Server{Logger: zap.NewNop(), Authz: authz.New(nil, nil, nil), Adapters: unreachableAdapters(t)}

	sink, err := srv.isAuditSink(context.Background(), "as_siem")
	require.Error(t, err)
	require.False(t, sink)

	w := asSystem(srv.handleCreateAdapter, http.MethodPost, "/api/v1/adapters",
		`{"id":"as_siem","category":"ai","kind":"anthropic","name":"x"}`, nil)
	require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
	requireCode(t, w, errs.Internal)
}

// failingPolicy is a policy store that cannot be read.
type failingPolicy struct{}

func (failingPolicy) Load(context.Context) (corepolicy.Document, error) {
	return corepolicy.Document{}, errors.New("connection reset")
}
func (failingPolicy) Save(context.Context, corepolicy.Document, string) error { return nil }

// TestR390_APolicyChangeIsNotSavedWithoutWhatItReplaces asserts the policy
// save reads what applied before, for the event's changes, and that a read
// that fails stops the save rather than recording a change with no "from".
func TestR390_APolicyChangeIsNotSavedWithoutWhatItReplaces(t *testing.T) {
	srv := &Server{Logger: zap.NewNop(), Authz: authz.New(nil, nil, nil), PolicyStore: failingPolicy{}}
	w := asSystem(srv.handlePutPolicy, http.MethodPut, "/api/v1/policy", `{}`, nil)
	require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
}
