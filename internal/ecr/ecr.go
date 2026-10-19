// Package ecr mints an Amazon ECR registry password from an AWS access key.
//
// ECR has no long-lived registry password: GetAuthorizationToken trades an
// access key for one that lasts twelve hours. Two places need that — an image
// app pulling with its own credential (core/oci, issue #41) and the install's
// image registry adapter pushing builds (adapter/imageregistry/ecr) — so it
// lives here, in a leaf package core and the adapters can both import, as
// internal/registrylimit does.
package ecr

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ecr"

	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// host matches an ECR registry's host, in each form ECR serves it
// (docs.aws.amazon.com/general/latest/gr/ecr.html):
//
//	123456789012.dkr.ecr.us-east-1.amazonaws.com       IPv4
//	123456789012.dkr.ecr-fips.us-east-1.amazonaws.com  IPv4, FIPS
//	123456789012.dkr-ecr.us-east-1.on.aws              dual-stack
//	123456789012.dkr-ecr-fips.us-east-1.on.aws         dual-stack, FIPS
//	123456789012.dkr.ecr.cn-north-1.amazonaws.com.cn   China
var host = regexp.MustCompile(`^\d{12}\.(?:dkr\.ecr(-fips)?\.([a-z0-9-]+)\.amazonaws\.com(?:\.cn)?|dkr-ecr(-fips)?\.([a-z0-9-]+)\.on\.aws)$`)

// Host is what an ECR registry host says about where to ask for a password.
type Host struct {
	Region string

	// DualStack is a host on ECR's dual-stack (IPv4 and IPv6) endpoint, so
	// the password is asked of the API's dual-stack endpoint too: an install
	// that reaches the registry only over IPv6 reaches nothing else.
	DualStack bool

	// FIPS is a host on a FIPS endpoint, so the API's FIPS endpoint is used.
	FIPS bool
}

// Parse reads an ECR registry host.
func Parse(registry string) (Host, bool) {
	m := host.FindStringSubmatch(strings.ToLower(registry))
	if m == nil {
		return Host{}, false
	}
	if m[2] != "" {
		return Host{Region: m[2], FIPS: m[1] != ""}, true
	}
	return Host{Region: m[4], DualStack: true, FIPS: m[3] != ""}, true
}

// Region reads the region from an ECR registry host:
// 123456789012.dkr.ecr.us-east-1.amazonaws.com is us-east-1, and so is
// 123456789012.dkr-ecr.us-east-1.on.aws.
func Region(registry string) (string, bool) {
	h, ok := Parse(registry)
	return h.Region, ok
}

// Key is an AWS access key, and where to ask.
type Key struct {
	AccessKeyID     string
	SecretAccessKey secret.Value
	Region          string

	// DualStack and FIPS choose the API endpoint, as the registry host does
	// (Parse).
	DualStack bool
	FIPS      bool

	// Endpoint overrides the ECR API endpoint, for tests.
	Endpoint string
}

// Mint asks ECR for a registry password. whose names the key's owner in the
// refusal — "the app's", "the image registry's" — so the message says which
// credential to replace.
func Mint(ctx context.Context, k Key, whose string) (string, secret.Value, error) {
	opts := ecr.Options{
		Region:      k.Region,
		Credentials: credentials.NewStaticCredentialsProvider(k.AccessKeyID, k.SecretAccessKey.Reveal(), ""),
	}
	opts.EndpointOptions = endpointOptions(k)
	if k.Endpoint != "" {
		opts.BaseEndpoint = aws.String(k.Endpoint)
	}
	out, err := ecr.New(opts).GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return "", secret.Value{}, errs.Wrap(errs.ValidInvalid,
			fmt.Sprintf("AWS refused %s access key when Pando asked ECR in %s for a registry password.", whose, k.Region), err)
	}
	if len(out.AuthorizationData) == 0 || out.AuthorizationData[0].AuthorizationToken == nil {
		return "", secret.Value{}, errs.Newf(errs.AdapterFailed,
			"ECR in %s answered without a registry password.", k.Region)
	}
	raw, err := base64.StdEncoding.DecodeString(*out.AuthorizationData[0].AuthorizationToken)
	if err != nil {
		return "", secret.Value{}, errs.Wrap(errs.AdapterFailed, "ECR returned a registry password Pando could not read.", err)
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok {
		return "", secret.Value{}, errs.New(errs.AdapterFailed, "ECR returned a registry password Pando could not read.")
	}
	return user, secret.New(pass), nil
}

// endpointOptions chooses the API endpoint the registry host implies. None
// with an Endpoint override: the SDK refuses a custom endpoint combined with
// either.
func endpointOptions(k Key) ecr.EndpointResolverOptions {
	var o ecr.EndpointResolverOptions
	if k.Endpoint != "" {
		return o
	}
	if k.DualStack {
		o.UseDualStackEndpoint = aws.DualStackEndpointStateEnabled
	}
	if k.FIPS {
		o.UseFIPSEndpoint = aws.FIPSEndpointStateEnabled
	}
	return o
}
