package ecr

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/stretchr/testify/require"
)

// TestEveryFormOfAnECRHostIsRead asserts that each host ECR serves a registry
// on is read as ECR, with its region and the endpoint it implies: IPv4,
// FIPS, dual-stack (<account>.dkr-ecr.<region>.on.aws), dual-stack FIPS,
// and China.
func TestEveryFormOfAnECRHostIsRead(t *testing.T) {
	for host, want := range map[string]Host{
		"123456789012.dkr.ecr.us-east-1.amazonaws.com":          {Region: "us-east-1"},
		"123456789012.dkr.ecr-fips.us-gov-west-1.amazonaws.com": {Region: "us-gov-west-1", FIPS: true},
		"123456789012.dkr-ecr.eu-west-1.on.aws":                 {Region: "eu-west-1", DualStack: true},
		"123456789012.DKR-ECR-FIPS.us-east-2.on.aws":            {Region: "us-east-2", DualStack: true, FIPS: true},
		"123456789012.dkr.ecr.cn-north-1.amazonaws.com.cn":      {Region: "cn-north-1"},
	} {
		got, ok := Parse(host)
		require.True(t, ok, host)
		require.Equal(t, want, got, host)
		region, ok := Region(host)
		require.True(t, ok)
		require.Equal(t, want.Region, region)
	}
	for _, host := range []string{
		"ghcr.io",
		"12345.dkr.ecr.us-east-1.amazonaws.com",
		"123456789012.dkr-ecr.us-east-1.amazonaws.com",
		"123456789012.dkr.ecr.us-east-1.on.aws",
		"123456789012.dkr-ecr.us-east-1.on.aws.evil.test",
	} {
		_, ok := Parse(host)
		require.False(t, ok, host)
	}
}

// TestADualStackHostAsksTheDualStackAPI asserts that the password for a
// dual-stack or FIPS registry host is asked of the matching API endpoint, so
// an IPv6-only install signs in where it pushes.
func TestADualStackHostAsksTheDualStackAPI(t *testing.T) {
	resolve := func(k Key) string {
		t.Helper()
		ep, err := ecr.NewDefaultEndpointResolverV2().ResolveEndpoint(context.Background(), ecr.EndpointParameters{
			Region:       aws.String(k.Region),
			UseDualStack: aws.Bool(endpointOptions(k).UseDualStackEndpoint == aws.DualStackEndpointStateEnabled),
			UseFIPS:      aws.Bool(endpointOptions(k).UseFIPSEndpoint == aws.FIPSEndpointStateEnabled),
		})
		require.NoError(t, err)
		return ep.URI.Host
	}
	require.Equal(t, "api.ecr.us-east-1.amazonaws.com", resolve(Key{Region: "us-east-1"}))
	require.Equal(t, "ecr.us-east-1.api.aws", resolve(Key{Region: "us-east-1", DualStack: true}))
	require.Contains(t, resolve(Key{Region: "us-east-1", FIPS: true}), "fips")
	require.Equal(t, ecr.EndpointResolverOptions{}, endpointOptions(Key{Region: "us-east-1", DualStack: true, FIPS: true, Endpoint: "http://127.0.0.1"}),
		"a test endpoint is used as given")
}
