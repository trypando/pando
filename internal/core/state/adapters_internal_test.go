package state

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/errs"
)

// TestR105_EveryRuleAnAdapterBreaksIsNamed asserts issue #220's third point
// for each refusal the database can give a save: a broken rule is a
// validation error with a remedy, and only a failure of the database itself
// is internal.
func TestR105_EveryRuleAnAdapterBreaksIsNamed(t *testing.T) {
	c := AdapterConfig{ID: "rte_cf", Category: "routing", Kind: "cloudflare"}
	cases := []struct {
		pg       *pgconn.PgError
		contains string
	}{
		{&pgconn.PgError{Code: "23505", ConstraintName: "adapter_configs_one_ai_adapter_per_kind"}, "one AI adapter per provider"},
		{&pgconn.PgError{Code: "23505", ConstraintName: "adapter_configs_one_default_per_category"}, "became the default"},
		{&pgconn.PgError{Code: "23514", ConstraintName: "adapter_configs_registry_url_no_credential"}, "username or password"},
		{&pgconn.PgError{Code: "23514", ConstraintName: "adapter_configs_audit_sink_url_no_credential"}, "username or password"},
		{&pgconn.PgError{Code: "23514", ConstraintName: "adapter_configs_config_check"}, "break the rule adapter_configs_config_check"},
		{&pgconn.PgError{Code: "23505", ConstraintName: "adapter_configs_some_index"}, "the rule adapter_configs_some_index allows only one"},
	}
	for _, tc := range cases {
		t.Run(tc.pg.ConstraintName, func(t *testing.T) {
			var e *errs.Error
			require.True(t, errors.As(saveRefusal(c, fmt.Errorf("insert: %w", tc.pg)), &e))
			require.Equal(t, errs.ValidInvalid, e.Code)
			require.Contains(t, e.Message, tc.contains)
			require.NotEmpty(t, e.Remedy)
		})
	}

	for _, err := range []error{errors.New("connection reset"), &pgconn.PgError{Code: "57014"}} {
		var e *errs.Error
		require.True(t, errors.As(saveRefusal(c, err), &e))
		require.Equal(t, errs.Internal, e.Code, "a failure of the database itself is not the request's to fix")
		require.Equal(t, saveFailed, e.Message)
	}
}
