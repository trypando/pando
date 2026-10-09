package deploy

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestR256_TheDeployLimitComesFromHostPolicy asserts how the queue reads
// max_concurrent_deploys (issue #93): what policy says, the default for zero,
// and the last limit read when policy cannot be read — never no limit, and
// never a stopped queue.
func TestR256_TheDeployLimitComesFromHostPolicy(t *testing.T) {
	var (
		n   int
		err error
	)
	limit := policyLimit(func(context.Context) (int, error) { return n, err }, 3, zap.NewNop())
	ctx := context.Background()

	n = 5
	require.Equal(t, 5, limit(ctx), "what policy says")
	n = 0
	require.Equal(t, DefaultConcurrency(), limit(ctx), "zero is one per CPU, at least two")
	n = 7
	require.Equal(t, 7, limit(ctx))

	err = errors.New("the policy row could not be read")
	require.Equal(t, 7, limit(ctx), "a failed read keeps the last limit")
	n, err = 2, nil
	require.Equal(t, 2, limit(ctx), "and the next good read takes over")

	first := policyLimit(func(context.Context) (int, error) { return 0, errors.New("down") }, 4, zap.NewNop())
	require.Equal(t, 4, first(ctx), "a failure before any read keeps the limit the queue started with")
}
