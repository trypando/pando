package docker

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/moby/moby/api/types/registry"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/secret"
)

// The daemon reads pull credentials as base64url JSON in its own field names;
// anything else is an anonymous pull with no error to say so. Decoded here with
// the daemon's own decoder (issue #41).
func TestAPullCredentialIsEncodedTheWayTheDaemonReadsIt(t *testing.T) {
	header, err := registryAuthHeader(&api.RegistryAuth{
		Registry: "123456789012.dkr.ecr.us-east-1.amazonaws.com", Username: "AWS", Password: secret.New("minted+/=token"),
	})
	require.NoError(t, err)

	raw, err := base64.URLEncoding.DecodeString(header)
	require.NoError(t, err, "base64url, as registry.AuthHeader requires")
	var got registry.AuthConfig
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Equal(t, "AWS", got.Username)
	require.Equal(t, "minted+/=token", got.Password)
	require.Equal(t, "123456789012.dkr.ecr.us-east-1.amazonaws.com", got.ServerAddress)

	// And the plan that carries it never prints it (R-194).
	require.NotContains(t, fmt.Sprintf("%+v", api.WorkloadPlan{PullAuth: &api.RegistryAuth{Password: secret.New("minted+/=token")}}), "minted")
}
