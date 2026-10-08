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

var host = regexp.MustCompile(`^\d{12}\.dkr\.ecr(-fips)?\.([a-z0-9-]+)\.amazonaws\.com(\.cn)?$`)

// Region reads the region from an ECR registry host:
// 123456789012.dkr.ecr.us-east-1.amazonaws.com is us-east-1.
func Region(registry string) (string, bool) {
	m := host.FindStringSubmatch(strings.ToLower(registry))
	if m == nil {
		return "", false
	}
	return m[2], true
}

// Key is an AWS access key, and where to ask.
type Key struct {
	AccessKeyID     string
	SecretAccessKey secret.Value
	Region          string

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
