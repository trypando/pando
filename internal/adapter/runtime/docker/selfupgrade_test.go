package docker

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR355_AnUpgradedPandoStillReportsTheTagItWasDeployedBy asserts R-355
// across a second upgrade: the swap creates the new container from the digest
// it verified, and Self reports the tag the deployment named, kept in a label,
// rather than that digest — which the next upgrade would refuse as a pin.
func TestR355_AnUpgradedPandoStillReportsTheTagItWasDeployedBy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		image  string
		labels map[string]string
		want   string
	}{
		{"as the deployment started it", "trypando/pando:latest", nil, "trypando/pando:latest"},
		{"as an upgrade recreated it", "trypando/pando@sha256:150e1fec4c6e5cc211335c6e2dd793ea35215c91141ba03a243d09f594e3003f",
			map[string]string{labelDeployedImage: "trypando/pando:latest"}, "trypando/pando:latest"},
		{"pinned by the deployment itself", "trypando/pando@sha256:150e1fec4c6e5cc211335c6e2dd793ea35215c91141ba03a243d09f594e3003f",
			nil, "trypando/pando@sha256:150e1fec4c6e5cc211335c6e2dd793ea35215c91141ba03a243d09f594e3003f"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, a := newFakeDaemon(t, map[string]any{"proxy_container": "pando-self"})
			f.on("GET /containers/pando-self/json", respond(http.StatusOK, map[string]any{
				"Id":     "pando-self",
				"Config": map[string]any{"Image": tc.image, "Labels": tc.labels},
			}))
			self, err := a.Self(context.Background())
			require.NoError(t, err)
			require.Equal(t, tc.want, self.Image)
		})
	}
}
