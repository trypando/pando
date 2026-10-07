package proxy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

func TestAnUpstreamWithoutARuntimeRegistryIsUnavailable(t *testing.T) {
	u := NewRuntimeUpstreams(nil)
	_, err := u.Primary(context.Background(), app(), specWith(spec.Workload{
		Name: "web", Primary: true, Ports: []spec.Port{{Number: 3000, Protocol: "http"}},
	}))
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "no runtimes")
}
