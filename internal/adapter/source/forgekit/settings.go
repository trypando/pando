package forgekit

import (
	"encoding/json"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// The methods a connection can authenticate with (api.SourceCapabilities.Method).
const (
	MethodToken            = "token"
	MethodSSH              = "ssh"
	MethodApp              = "app"
	MethodOAuth            = "oauth"
	MethodServicePrincipal = "service_principal"
)

// The credential fields the shared methods use. A forge adds its own beside
// them (a GitHub App's private key).
const (
	FieldToken         = "token"
	FieldSSHPrivateKey = "ssh_private_key"
	FieldSSHPassphrase = "ssh_passphrase"
	FieldClientSecret  = "client_secret"
)

// Settings are the settings every source adapter shares. A forge's own
// settings struct embeds it.
type Settings struct {
	Method string `json:"method"`

	// Host is the forge's host: "github.com", "gitlab.acme.internal".
	Host string `json:"host"`

	// Scope limits the connection to an owner, group, workspace or project
	// on the host. Empty is the whole host.
	Scope string `json:"scope"`

	// APIURL overrides where the forge's API is, for a self-managed one
	// whose API is not where the forge's default says.
	APIURL string `json:"api_url"`

	// Username goes beside a token, for hosts that want one.
	Username string `json:"username"`

	KnownHosts string `json:"known_hosts"`
	CABundle   string `json:"ca_bundle"`

	ClientID string `json:"client_id"`

	// Credentials are filled in by core from adapter_credentials (O-20);
	// never stored in config.
	Credentials map[string]secret.Value `json:"credentials"`
}

// Decode reads raw configuration into a forge's settings struct, which
// embeds Settings.
func Decode(raw json.RawMessage, into any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return errs.Wrap(errs.ValidInvalid, "The connection's settings could not be read.", err)
	}
	return nil
}

// Credential is one credential field, or the zero value.
func (s Settings) Credential(field string) secret.Value {
	return s.Credentials[field]
}

// Validate checks the shared settings for the method chosen. methods lists
// the methods the forge offers.
func (s *Settings) Validate(provider string, methods []string, defaultHost string) error {
	if s.Method == "" {
		s.Method = methods[0]
	}
	ok := false
	for _, m := range methods {
		if m == s.Method {
			ok = true
		}
	}
	if !ok {
		return errs.Newf(errs.ValidInvalid,
			"%q is not a way Pando can connect to %s. Choose one of: %s.",
			s.Method, provider, strings.Join(methods, ", "))
	}
	if s.Host == "" {
		s.Host = defaultHost
	}
	s.Host = HostOf(s.Host)
	if s.Host == "" {
		return errs.Newf(errs.ValidInvalid, "A %s connection needs a host, such as %s.", provider, firstNonEmpty(defaultHost, "git.example.com"))
	}
	if s.CABundle != "" {
		if _, err := CABundle(s.CABundle); err != nil {
			return err
		}
	}
	switch s.Method {
	case MethodSSH:
		if err := CheckSSHKey(s.Credential(FieldSSHPrivateKey), s.Credential(FieldSSHPassphrase)); err != nil {
			return err
		}
		if err := CheckKnownHosts(s.KnownHosts); err != nil {
			return err
		}
	case MethodToken:
		if s.Credential(FieldToken).IsZero() {
			return errs.Newf(errs.ValidInvalid, "A %s token connection needs a token.", provider).
				WithRemedy("Create an access token with permission to read repositories and paste it here.")
		}
	}
	return nil
}

// CA is the CA bundle setting, read. Validate has already refused a bad one.
func (s Settings) CA() []byte {
	b, _ := CABundle(s.CABundle)
	return b
}

// Form fields the shared methods take. A forge's KindInfo lists the ones it
// uses, with its own labels where the forge has a name for the thing.

func MethodField(options ...api.Option) api.Field {
	return api.Field{
		Key: "method", Label: "How Pando signs in", Type: "select", Required: true,
		Options: options, Default: options[0].Value,
	}
}

func HostField(def string) api.Field {
	f := api.Field{
		Key: "host", Label: "Host", Type: "string",
		Help: "The host repositories are on. Change it for a self-managed server.",
	}
	if def != "" {
		f.Default = def
		f.Placeholder = def
	} else {
		f.Required = true
		f.Placeholder = "git.example.com"
	}
	return f
}

func ScopeField(label, help, placeholder string) api.Field {
	return api.Field{
		Key: "scope", Label: label, Type: "string", Help: help, Placeholder: placeholder,
	}
}

func TokenField(label, help string, shown ...string) api.Field {
	return api.Field{
		Key: FieldToken, Label: label, Type: "string", Credential: true, Help: help,
		ShownWhen: shownWhen(shown, MethodToken),
	}
}

func UsernameField(help, placeholder string, shown ...string) api.Field {
	return api.Field{
		Key: "username", Label: "Username", Type: "string", Help: help, Placeholder: placeholder,
		ShownWhen: shownWhen(shown, MethodToken),
	}
}

// SSHFields are the private key, its passphrase and the host keys.
func SSHFields(knownHostsHelp string) []api.Field {
	when := &api.Condition{Key: "method", Values: []string{MethodSSH}}
	return []api.Field{
		{Key: FieldSSHPrivateKey, Label: "SSH private key", Type: "string", Multiline: true, Credential: true,
			Help: "The private half of a deploy key or access key added to the repository. Pando reads the repository with it and never writes.", ShownWhen: when},
		{Key: FieldSSHPassphrase, Label: "Key passphrase", Type: "string", Credential: true,
			Help: "Only if the key has one.", ShownWhen: when},
		{Key: "known_hosts", Label: "Known hosts", Type: "string", Multiline: true,
			Help: knownHostsHelp, ShownWhen: when},
	}
}

// OAuthFields are the OAuth application's client ID and secret.
func OAuthFields(help string) []api.Field {
	when := &api.Condition{Key: "method", Values: []string{MethodOAuth}}
	return []api.Field{
		{Key: "client_id", Label: "OAuth client ID", Type: "string", Help: help, ShownWhen: when},
		{Key: FieldClientSecret, Label: "OAuth client secret", Type: "string", Credential: true,
			Help: "Needed for browser authorization. Device authorization works without one.", ShownWhen: when},
	}
}

func APIURLField(def string) api.Field {
	return api.Field{
		Key: "api_url", Label: "API address", Type: "string", Advanced: true,
		Default: def, Help: "Where the host's API is, if not where Pando would look for it.",
	}
}

func CAField() api.Field {
	return api.Field{
		Key: "ca_bundle", Label: "Certificate authority", Type: "string", Multiline: true, Advanced: true,
		Default: "The system's trusted certificates",
		Help:    "PEM certificates to trust for a self-managed host on a private certificate authority, beside the system's.",
	}
}

func shownWhen(methods []string, def string) *api.Condition {
	if len(methods) == 0 {
		methods = []string{def}
	}
	return &api.Condition{Key: "method", Values: methods}
}
