// Package oci is the image registry adapter for any registry that signs in
// with a username and password, or not at all: the Distribution registry
// docker-compose.registry.yml runs, Harbor, GitHub Container Registry,
// Google Artifact Registry, Zot. Each creates a repository on the first push
// to it, so each app can have its own.
package oci

import (
	"context"
	"encoding/json"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/imageregistry/registrykit"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Kind is the adapter's kind string.
const Kind = "oci"

// Adapter is one OCI registry.
type Adapter struct {
	registrykit.Registry
}

// New returns an unconfigured adapter.
func New() *Adapter { return &Adapter{} }

type config struct {
	registrykit.Settings
	Username    string                  `json:"username"`
	Credentials map[string]secret.Value `json:"credentials"`
}

func (a *Adapter) Kind() string           { return Kind }
func (a *Adapter) Category() api.Category { return api.CategoryImageRegistry }

// Configure reads the registry's settings and its password.
func (a *Adapter) Configure(_ context.Context, raw json.RawMessage) error {
	var cfg config
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return errs.Wrap(errs.ValidInvalid, "The image registry's settings could not be read.", err)
		}
	}
	password := cfg.Credentials["password"]
	if (cfg.Username == "") != password.IsZero() {
		return errs.New(errs.ValidInvalid,
			"The image registry's credential is incomplete: it needs both a username and a password.").
			WithRemedy("Set both, or neither for a registry Pando reaches anonymously.")
	}
	var auth registrykit.AuthFunc
	if cfg.Username != "" {
		user := cfg.Username
		auth = func(_ context.Context, host string) (*api.RegistryAuth, error) {
			return &api.RegistryAuth{Registry: host, Username: user, Password: password}, nil
		}
	}
	reg, err := registrykit.Open(cfg.Settings, true, auth)
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
		Name:     "OCI registry",
		Description: "A registry that signs in with a username and password, or not at all: Distribution, Harbor, " +
			"GitHub Container Registry, Artifact Registry, Zot. Each app gets a repository of its own.",
		IDPrefix: "reg_",
		Fields: []api.Field{
			registrykit.URLField("https://registry.internal:5000",
				"The registry, and a path to push under if you want one."),
			{Key: "username", Label: "Username", Type: "string",
				Help: "What the registry signs Pando in with. Empty for a registry Pando reaches anonymously."},
			{Key: "password", Label: "Password", Type: "string", Credential: true,
				Help: "Stored encrypted, and never shown again."},
			{Key: "layout", Label: "Where images go", Type: "select", Advanced: true, Default: string(registrykit.LayoutPerApp),
				Options: []api.Option{
					{Value: string(registrykit.LayoutPerApp), Label: "A repository per app",
						Description: "Each app has its own, so a deleted app's images are found and removed together."},
					{Value: string(registrykit.LayoutSingle), Label: "One repository for everything",
						Description: "For a registry where a repository has to exist before anything is pushed to it."},
				}},
			{Key: "insecure", Label: "Allow plain HTTP", Type: "bool", Advanced: true, Default: "false",
				Help: "Only for a registry on a private network. Every host that pulls must also list it as an insecure registry."},
			registrykit.AlwaysField(),
		},
	}
}

var _ api.ImageRegistryAdapter = (*Adapter)(nil)
