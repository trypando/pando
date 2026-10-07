package local_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestO54_ALocalModelIsNotGivenTheLookupTools asserts the [P] choice in
// design 10 §10.6: the local adapter does not say it looks things up, so
// core pre-searches the request's words for it instead.
func TestO54_ALocalModelIsNotGivenTheLookupTools(t *testing.T) {
	a, _ := withConfig(t, nil)
	caps, err := a.Capabilities(context.Background())
	require.NoError(t, err)
	require.False(t, caps.LooksUp)
}
