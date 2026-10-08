package oci

import (
	"context"
	"regexp"
	"strings"

	"github.com/trypando/pando/internal/ecr"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// CredentialKind is how an app authenticates to its image's registry.
type CredentialKind string

const (
	// CredentialBasic is a username and a password or token: Docker Hub, GHCR,
	// GitLab, Quay, Harbor, and most registries.
	CredentialBasic CredentialKind = "basic"

	// CredentialECR is an AWS access key. ECR's own registry passwords expire
	// after twelve hours, so the key is kept and a password minted from it for
	// every read and every pull.
	CredentialECR CredentialKind = "ecr"
)

// Credential is an app's registry credential, as stored (O-3: it belongs to
// the app). Every field is sealed at rest; the secret ones are secret.Value so
// they cannot reach a log line (R-194).
type Credential struct {
	Kind CredentialKind `json:"kind"`

	Username string       `json:"username,omitempty"`
	Password secret.Value `json:"password,omitempty"`

	AccessKeyID     string       `json:"access_key_id,omitempty"`
	SecretAccessKey secret.Value `json:"secret_access_key,omitempty"`
	// Region is the ECR region. Empty reads it from the registry host.
	Region string `json:"region,omitempty"`
}

// Auth is what one pull authenticates with: resolved, short-lived, for one
// registry.
type Auth struct {
	Registry string
	Username string
	Password secret.Value
	// IdentityToken is what `docker login` keeps for a registry signed in to
	// through a browser, in place of a password.
	IdentityToken secret.Value
}

// Validate checks that a credential is complete.
func (c Credential) Validate() error {
	switch c.Kind {
	case CredentialBasic:
		if strings.TrimSpace(c.Username) == "" || c.Password.IsZero() {
			return errs.New(errs.ValidInvalid,
				"A registry credential needs a username and a password or access token.").
				WithRemedy("Give the username you sign in to the registry with, and a token that can read the image. On GitHub that is a personal access token with the read:packages scope.")
		}
	case CredentialECR:
		if strings.TrimSpace(c.AccessKeyID) == "" || c.SecretAccessKey.IsZero() {
			return errs.New(errs.ValidInvalid,
				"An ECR credential needs an AWS access key ID and its secret access key.").
				WithRemedy("Create an access key for an IAM user whose policy allows ecr:GetAuthorizationToken, ecr:BatchGetImage and ecr:GetDownloadUrlForLayer on the repository.")
		}
		if c.Region != "" && !regionPattern.MatchString(c.Region) {
			return errs.Newf(errs.ValidInvalid, "%q is not an AWS region, such as us-east-1.", c.Region)
		}
	default:
		return errs.Newf(errs.ValidInvalid,
			"%q is not a kind of registry credential Pando knows. Valid answers: basic (a username and a token) or ecr (AWS access keys).", c.Kind)
	}
	return nil
}

// Fields is the credential as stored, one sealed value per field.
func (c Credential) Fields() map[string]secret.Value {
	out := map[string]secret.Value{"kind": secret.New(string(c.Kind))}
	put := func(k string, v secret.Value) {
		if !v.IsZero() {
			out[k] = v
		}
	}
	put("username", secret.New(c.Username))
	put("password", c.Password)
	put("access_key_id", secret.New(c.AccessKeyID))
	put("secret_access_key", c.SecretAccessKey)
	put("region", secret.New(c.Region))
	return out
}

// CredentialFields is every field a credential may store, so replacing one
// can remove what the previous kind left behind.
var CredentialFields = []string{"kind", "username", "password", "access_key_id", "secret_access_key", "region"}

// CredentialFrom reads a credential back from its stored fields. It reports
// false when there is none.
func CredentialFrom(fields map[string]secret.Value) (Credential, bool) {
	kind, ok := fields["kind"]
	if !ok {
		return Credential{}, false
	}
	return Credential{
		Kind:            CredentialKind(kind.Reveal()),
		Username:        fields["username"].Reveal(),
		Password:        fields["password"],
		AccessKeyID:     fields["access_key_id"].Reveal(),
		SecretAccessKey: fields["secret_access_key"],
		Region:          fields["region"].Reveal(),
	}, true
}

// Resolver turns a stored credential into the auth for one pull.
type Resolver struct {
	// ECREndpoint overrides the ECR API endpoint, for tests.
	ECREndpoint string
}

// Resolve returns the auth for reading reference with c. For ECR it mints a
// fresh registry password each time; one minted earlier may have expired.
func (r Resolver) Resolve(ctx context.Context, c Credential, reference string) (*Auth, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	registry, err := Registry(reference)
	if err != nil {
		return nil, err
	}

	switch c.Kind {
	case CredentialBasic:
		return &Auth{Registry: registry, Username: c.Username, Password: c.Password}, nil

	case CredentialECR:
		region := c.Region
		if region == "" {
			var ok bool
			if region, ok = ECRRegion(registry); !ok {
				return nil, errs.Newf(errs.ValidInvalid,
					"This app has AWS credentials for ECR, but its image is on %s, which is not an ECR registry.", registry).
					WithRemedy("Use an image from an ECR registry such as 123456789012.dkr.ecr.us-east-1.amazonaws.com/team/app, or replace the credential with a username and token for " + registry + ".")
			}
		}
		user, pass, err := ecr.Mint(ctx, ecr.Key{AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey,
			Region: region, Endpoint: r.ECREndpoint}, "the app's")
		if err != nil {
			if e := errs.As(err); e != nil && e.Code == errs.ValidInvalid {
				e.Remedy = "Check that the access key is active and that its policy allows ecr:GetAuthorizationToken, then replace the credential in the app's settings."
			}
			return nil, err
		}
		return &Auth{Registry: registry, Username: user, Password: pass}, nil
	}
	return nil, errs.Newf(errs.Internal, "unhandled credential kind %q", c.Kind)
}

var regionPattern = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-\d+$`)

// ECRRegion reads the region from an ECR registry host:
// 123456789012.dkr.ecr.us-east-1.amazonaws.com is us-east-1.
func ECRRegion(registry string) (string, bool) { return ecr.Region(registry) }
