// Package generic is the source adapter for any git host (R-091): an HTTPS
// username and token, or an SSH key with the host's key pinned. It has no API
// to list repositories with, so an app from it is created by entering the
// repository's address.
package generic

import (
	"context"
	"encoding/json"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/source/forgekit"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Kind is the adapter's kind string.
const Kind = "git"

var methods = []string{forgekit.MethodToken, forgekit.MethodSSH}

// Adapter is one connection to a git host.
type Adapter struct {
	cfg        forgekit.Settings
	configured bool
}

// New returns an unconfigured adapter.
func New() *Adapter { return &Adapter{} }

func (a *Adapter) Kind() string           { return Kind }
func (a *Adapter) Category() api.Category { return api.CategorySource }

// Configure reads the connection's settings and credentials.
func (a *Adapter) Configure(_ context.Context, raw json.RawMessage) error {
	var cfg forgekit.Settings
	if err := forgekit.Decode(raw, &cfg); err != nil {
		return err
	}
	if err := cfg.Validate("git host", methods, ""); err != nil {
		return err
	}
	a.cfg, a.configured = cfg, true
	return nil
}

// HealthCheck succeeds once configured. Whether the credential still works is
// learned at the next clone, which reports it (R-146).
func (a *Adapter) HealthCheck(context.Context) error {
	if !a.configured {
		return errs.New(errs.AdapterUnavailable, "This git connection is not configured.")
	}
	return nil
}

func (a *Adapter) SourceCapabilities() api.SourceCapabilities {
	return api.SourceCapabilities{
		Method:     a.cfg.Method,
		Host:       a.cfg.Host,
		Scope:      a.cfg.Scope,
		Authorized: a.configured,
	}
}

// Covers scores the repository by host and path prefix.
func (a *Adapter) Covers(repoURL string) int {
	if !a.configured {
		return 0
	}
	u, err := forgekit.ParseRepoURL(repoURL)
	if err != nil {
		return 0
	}
	return forgekit.Covers([]string{a.cfg.Host}, forgekit.ScopeSegments(a.cfg.Scope), u.Host, u.Segments())
}

// GitCredential is the token or the key.
func (a *Adapter) GitCredential(_ context.Context, repoURL string) (api.GitCredential, error) {
	u, err := forgekit.ParseRepoURL(repoURL)
	if err != nil {
		return api.GitCredential{}, err
	}
	if a.cfg.Method == forgekit.MethodSSH {
		return forgekit.SSHCredential(u, "", a.cfg.Credential(forgekit.FieldSSHPrivateKey),
			a.cfg.Credential(forgekit.FieldSSHPassphrase), a.cfg.KnownHosts), nil
	}
	if u.SSH {
		return api.GitCredential{}, errs.Newf(errs.ValidInvalid,
			"The repository %s is an SSH address, and this connection signs in with a token over HTTPS.", repoURL).
			WithRemedy("Enter the repository's HTTPS address instead.")
	}
	username := a.cfg.Username
	if username == "" {
		username = "git"
	}
	return forgekit.TokenCredential(username, a.cfg.Credential(forgekit.FieldToken), a.cfg.CA()), nil
}

// Refresh has nothing to renew: a token or a key does not expire on a
// schedule Pando can see.
func (a *Adapter) Refresh(context.Context) (map[string]secret.Value, error) { return nil, nil }

func (a *Adapter) ListRepositories(context.Context, api.ListRepositoriesRequest) ([]api.Repository, error) {
	return nil, noAPI()
}

func (a *Adapter) ListBranches(context.Context, string) ([]string, error) {
	return nil, noAPI()
}

func (a *Adapter) BeginAuthorization(context.Context, api.AuthorizationRequest) (api.Authorization, error) {
	return api.Authorization{}, errs.New(errs.ValidInvalid,
		"A git connection signs in with a token or an SSH key; there is nothing to authorize.")
}

func (a *Adapter) CompleteAuthorization(context.Context, api.AuthorizationCompletion) (api.AuthorizationResult, error) {
	return api.AuthorizationResult{}, errs.New(errs.ValidInvalid,
		"A git connection signs in with a token or an SSH key; there is nothing to authorize.")
}

func noAPI() error {
	return errs.New(errs.ValidInvalid,
		"A plain git connection cannot list repositories: git has no way to ask a host what is on it.").
		WithRemedy("Enter the repository's address instead.")
}

// Info describes the kind for the console's and the CLI's forms.
func Info() api.KindInfo {
	fields := []api.Field{
		forgekit.MethodField(
			api.Option{Value: forgekit.MethodToken, Label: "Username and token",
				Description: "HTTPS, with an access token or password as the password."},
			api.Option{Value: forgekit.MethodSSH, Label: "SSH key",
				Description: "A deploy key. The host's public key is pinned."},
		),
		forgekit.HostField(""),
		forgekit.ScopeField("Path prefix",
			"Limit the connection to repositories under this path, such as acme. Empty is every repository on the host.",
			"acme"),
		forgekit.UsernameField("The account the token belongs to. Defaults to git.", "git"),
		forgekit.TokenField("Token or password", "Read access to the repositories is enough."),
	}
	fields = append(fields, forgekit.SSHFields("The host's public key, as ssh-keyscan <host> prints it.")...)
	fields = append(fields, forgekit.CAField())
	return api.KindInfo{
		Category:    api.CategorySource,
		Kind:        Kind,
		Name:        "Any git host",
		Description: "Clone from any git server with a username and token over HTTPS, or an SSH key. Repositories are entered by address.",
		IDPrefix:    "src_",
		Fields:      fields,
	}
}

var _ api.SourceAdapter = (*Adapter)(nil)
