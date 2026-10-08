package api_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

// TestAReadIsKeptForItsScopeAndNoLonger: inside a scope a key is read once;
// outside one, in another scope, under another key, or after a failed read,
// it is read again.
func TestAReadIsKeptForItsScopeAndNoLonger(t *testing.T) {
	reads := 0
	read := func() (int, error) { reads++; return reads, nil }
	type key struct{ name string }

	v, err := api.ScopedRead(context.Background(), key{"a"}, read)
	require.NoError(t, err)
	require.Equal(t, 1, v)
	v, _ = api.ScopedRead(context.Background(), key{"a"}, read)
	require.Equal(t, 2, v, "no scope: every call reads")

	scope := api.WithReadScope(context.Background())
	v, _ = api.ScopedRead(scope, key{"a"}, read)
	require.Equal(t, 3, v)
	v, _ = api.ScopedRead(scope, key{"a"}, read)
	require.Equal(t, 3, v, "kept for the scope")
	v, _ = api.ScopedRead(scope, key{"b"}, read)
	require.Equal(t, 4, v, "another key reads")
	v, _ = api.ScopedRead(api.WithReadScope(context.Background()), key{"a"}, read)
	require.Equal(t, 5, v, "another scope reads")

	failing := api.WithReadScope(context.Background())
	_, err = api.ScopedRead(failing, key{"a"}, func() (int, error) { return 0, errors.New("down") })
	require.Error(t, err)
	v, err = api.ScopedRead(failing, key{"a"}, read)
	require.NoError(t, err)
	require.Equal(t, 6, v, "a failed read is not kept")
}
