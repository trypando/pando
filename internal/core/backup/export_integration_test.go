//go:build integration

package backup

import (
	"context"
	"io"

	"github.com/trypando/pando/internal/secret"
)

// RestoreDatabase exposes the database step of a restore to the external
// integration test, which needs statetest — and statetest imports state,
// which imports this package.
func RestoreDatabase(ctx context.Context, databaseURL secret.Value, dump io.Reader) error {
	s := &Service{DatabaseURL: databaseURL}
	return s.restoreDatabase(ctx, dump)
}
