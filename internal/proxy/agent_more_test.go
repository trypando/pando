package proxy_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/proxy"
)

// unkeyedUpstream is a dialed upstream that names no pool key and no port,
// which a runtime may return: the URL's host is then the key, and port 80 is
// assumed.
type unkeyedUpstream struct {
	dial func(context.Context) (net.Conn, error)
}

func (u unkeyedUpstream) Primary(context.Context, state.App, *spec.AppSpec) (api.Upstream, error) {
	return api.Upstream{URL: "http://pando-app_01HQ8-web", Dial: u.dial}, nil
}

// TestR023_ADialedUpstreamWithoutAPoolKeyStillGoesThroughItsDial asserts
// that an upstream the runtime dials is reached by that Dial and nothing
// else, even when it gives no pool key or port: the container's name is never
// looked up on Pando's host.
func TestR023_ADialedUpstreamWithoutAPoolKeyStillGoesThroughItsDial(t *testing.T) {
	via := func(url string) proxy.Upstreams {
		inner := throughAgent(t)(url).(fixedUpstream)
		return unkeyedUpstream{dial: inner.dial}
	}
	front, _, _, got := harnessVia(t, activeUser("usr_alice"), func(s *store) {
		s.owner[appID] = "usr_alice"
	}, via)
	resp, err := http.Get(front.URL + "/")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "upstream ok", string(body))
	require.NotNil(t, got.header)
}
