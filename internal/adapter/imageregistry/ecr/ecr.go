// Package ecr is the image registry adapter for Amazon ECR. ECR differs from
// an OCI registry in the two ways the category exists for: Pando signs in by
// trading an AWS access key for a password that lasts twelve hours, minted
// before each push and pull, and a repository has to exist before anything is
// pushed to it, so every build goes to one repository the operator created.
package ecr

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/imageregistry/registrykit"
	awsecr "github.com/trypando/pando/internal/ecr"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Kind is the adapter's kind string.
const Kind = "ecr"

// Adapter is one ECR repository.
type Adapter struct {
	registrykit.Registry

	// Endpoint overrides the ECR API endpoint, for tests.
	Endpoint string
}

// New returns an unconfigured adapter.
func New() *Adapter { return &Adapter{} }

type config struct {
	URL         string                  `json:"url"`
	AccessKeyID string                  `json:"access_key_id"`
	Always      bool                    `json:"always"`
	Credentials map[string]secret.Value `json:"credentials"`
}

func (a *Adapter) Kind() string           { return Kind }
func (a *Adapter) Category() api.Category { return api.CategoryImageRegistry }

// Configure reads the repository's address and the access key.
func (a *Adapter) Configure(_ context.Context, raw json.RawMessage) error {
	var cfg config
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return errs.Wrap(errs.ValidInvalid, "The ECR image registry's settings could not be read.", err)
		}
	}
	host, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(cfg.URL), "https://"), "/")
	region, ok := awsecr.Region(host)
	if !ok {
		return errs.Newf(errs.ValidInvalid, "The image registry's address is %q, which is not an ECR registry.", cfg.URL).
			WithRemedy("Use the repository's address as ECR shows it, such as 123456789012.dkr.ecr.us-east-1.amazonaws.com/pando, or choose the OCI registry kind for another registry.")
	}
	secretKey := cfg.Credentials["secret_access_key"]
	if cfg.AccessKeyID == "" || secretKey.IsZero() {
		return errs.New(errs.ValidInvalid, "The ECR image registry needs an AWS access key ID and its secret access key.").
			WithRemedy("Set both. The key's policy must allow ecr:GetAuthorizationToken and pushing to, pulling from and deleting in the repository.")
	}
	key := awsecr.Key{AccessKeyID: cfg.AccessKeyID, SecretAccessKey: secretKey, Region: region, Endpoint: a.Endpoint}
	auth := func(ctx context.Context, host string) (*api.RegistryAuth, error) {
		user, pass, err := awsecr.Mint(ctx, key, "the image registry's")
		if err != nil {
			if e := errs.As(err); e != nil && e.Code == errs.ValidInvalid {
				e.Remedy = "Check that the access key is active and that its policy allows ecr:GetAuthorizationToken, then replace it in the image registry adapter's settings."
			}
			return nil, err
		}
		return &api.RegistryAuth{Registry: host, Username: user, Password: pass}, nil
	}
	reg, err := registrykit.Open(registrykit.Settings{URL: cfg.URL, Layout: string(registrykit.LayoutSingle), Always: cfg.Always}, false, auth)
	if err != nil {
		return err
	}
	a.Registry = reg
	return nil
}

// Info describes the kind for the console's and the CLI's forms.
func Info() api.KindInfo {
	return api.KindInfo{
		Category: api.CategoryImageRegistry,
		Kind:     Kind,
		Name:     "Amazon ECR",
		Description: "An ECR repository you created. Every build goes into it, tagged by app and deployment, and " +
			"Pando gets a registry password from the access key before each push and pull.",
		IDPrefix: "reg_",
		Fields: []api.Field{
			registrykit.URLField("123456789012.dkr.ecr.us-east-1.amazonaws.com/pando",
				"The repository's address, as ECR shows it. It has to exist already."),
			{Key: "access_key_id", Label: "Access key ID", Type: "string", Required: true,
				Placeholder: "AKIA…"},
			{Key: "secret_access_key", Label: "Secret access key", Type: "string", Credential: true, Required: true,
				Help: "Stored encrypted, and never shown again."},
			registrykit.AlwaysField(),
		},
	}
}

var _ api.ImageRegistryAdapter = (*Adapter)(nil)
