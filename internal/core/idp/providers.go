package idp

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
	"github.com/trypando/pando/internal/secret"
)

// ProviderView is a provider as an administrator sees it: its settings, what
// to register with it, and how quickly it can revoke.
type ProviderView struct {
	state.IdentityProvider

	// Credentials names the secrets that are set, never their values.
	Credentials []string `json:"credentials"`

	// CallbackURL and EntityID are what the provider is told about Pando:
	// the redirect URI for OIDC, the ACS URL and audience for SAML.
	CallbackURL string `json:"callback_url,omitempty"`
	EntityID    string `json:"entity_id,omitempty"`
	MetadataURL string `json:"metadata_url,omitempty"`

	// SCIMBaseURL is where a SCIM client sends pushes, with the token.
	SCIMBaseURL string `json:"scim_base_url,omitempty"`

	// Revocation is R-047 made visible: the lifetime, the mode, and the
	// window that results, per provider.
	Revocation *Revocation `json:"revocation,omitempty"`

	// Problem is why the provider cannot be used as configured, when it
	// cannot. A provider with a problem stays listed so it can be fixed.
	Problem string `json:"problem,omitempty"`
}

// KindInfos describes the external kinds, for forms.
func (s *Service) KindInfos() []api.KindInfo {
	out := make([]api.KindInfo, 0, len(s.Kinds))
	for _, k := range s.Kinds {
		out = append(out, k.Info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// ListProviders returns every provider with its endpoints and revocation.
func (s *Service) ListProviders(ctx context.Context, o Origin) ([]ProviderView, error) {
	providers, err := s.Providers.List(ctx)
	if err != nil {
		return nil, err
	}
	fields, err := s.Credentials.Fields(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ProviderView, 0, len(providers))
	for _, p := range providers {
		out = append(out, s.view(ctx, o, p, fields[p.ID]))
	}
	return out, nil
}

// Provider returns one provider's view.
func (s *Service) Provider(ctx context.Context, o Origin, providerID string) (ProviderView, error) {
	p, found, err := s.Providers.ByID(ctx, providerID)
	if err != nil {
		return ProviderView{}, err
	}
	if !found {
		return ProviderView{}, errs.New(errs.NotFound, "There is no identity provider with that ID.")
	}
	fields, err := s.Credentials.Fields(ctx)
	if err != nil {
		return ProviderView{}, err
	}
	return s.view(ctx, o, p, fields[p.ID]), nil
}

func (s *Service) view(ctx context.Context, o Origin, p state.IdentityProvider, creds []string) ProviderView {
	v := ProviderView{IdentityProvider: p, Credentials: creds}
	if v.Credentials == nil {
		v.Credentials = []string{}
	}
	// Local accounts are on or off by host policy, not by their row: one
	// setting, disable_password_sign_in, so the Policy screen, the Sign-in
	// screen and the sign-in page cannot disagree about it.
	if p.Kind == "local" {
		if doc, err := s.Policy.Load(ctx); err == nil {
			v.Enabled = !doc.DisablePasswordSignIn
		}
	}
	if p.Kind != "local" {
		e := s.Endpoints(o, p.ID)
		v.CallbackURL = e.CallbackURL
		if p.Kind == "saml" {
			v.EntityID = e.EntityID
			v.MetadataURL = e.EntityID
		}
		v.SCIMBaseURL = s.SCIMBaseURL(o)
	}
	a, _, err := s.Adapter(ctx, p.ID)
	if err != nil {
		v.Problem = errs.As(err).Message
		return v
	}
	r := revocation(a, p)
	v.Revocation = &r
	return v
}

// ProviderInput is a create or update request. Credentials are write-only:
// stored encrypted, never returned (R-190). An empty credential value on
// update removes it; an absent one leaves it.
type ProviderInput struct {
	// ID is a new provider's ID, chosen ahead so the addresses Endpoints
	// builds from it can be registered with the provider before it is saved.
	// Optional; ignored on update.
	ID                    string            `json:"id,omitempty"`
	Kind                  string            `json:"kind"`
	Name                  *string           `json:"name"`
	Config                json.RawMessage   `json:"config"`
	Credentials           map[string]string `json:"credentials"`
	Enabled               *bool             `json:"enabled"`
	JITProvisioning       *bool             `json:"jit_provisioning"`
	LinkByEmail           *bool             `json:"link_by_email"`
	SCIMIdentityAttribute *string           `json:"scim_identity_attribute"`
}

// credentialFields are the settings of a kind that are secrets, by the
// kind's own description of itself.
func (s *Service) credentialFields(kind string) map[string]bool {
	out := map[string]bool{}
	for _, f := range s.Kinds[kind].Info.Fields {
		if f.Credential {
			out[f.Key] = true
		}
	}
	return out
}

// validate configures a throwaway adapter with the settings and credentials,
// so a mistake is refused when it is made rather than at someone's sign-in.
func (s *Service) validate(ctx context.Context, kind string, cfg json.RawMessage, creds map[string]secret.Value) error {
	k, ok := s.Kinds[kind]
	if !ok {
		return errs.Newf(errs.ValidInvalid, "Pando has no %q identity provider.", kind).
			WithRemedy("Use oidc for OpenID Connect, or saml for SAML 2.0.")
	}
	raw, err := withCredentials(cfg, creds)
	if err != nil {
		return errs.Wrap(errs.ValidInvalid, "The provider's settings must be a JSON object.", err)
	}
	return k.New().Configure(ctx, raw)
}

func cleanConfig(cfg json.RawMessage, secrets map[string]bool) (json.RawMessage, error) {
	if len(cfg) == 0 {
		return json.RawMessage(`{}`), nil
	}
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(cfg, &m); err != nil {
		return nil, errs.Wrap(errs.ValidInvalid, "The provider's settings must be a JSON object.", err)
	}
	for k := range m {
		if k == "credentials" || secrets[k] {
			return nil, errs.Newf(errs.ValidInvalid, "%q is a secret, and a secret cannot go in a provider's settings.", k).
				WithRemedy("Send it in \"credentials\" instead, where it is stored encrypted and never shown again.")
		}
	}
	return json.Marshal(m)
}

func (s *Service) checkSCIMAttribute(v *string) error {
	if v == nil || *v == "" || *v == "externalId" || *v == "userName" {
		return nil
	}
	return errs.Newf(errs.ValidInvalid, "scim_identity_attribute %q is not externalId or userName.", *v)
}

// CreateProvider adds a provider. Off unless enabled is set: a provider that
// appears on the sign-in page before anyone has tested it is a button that
// fails for everyone who presses it.
func (s *Service) CreateProvider(ctx context.Context, p authz.Principal, in ProviderInput, o Origin) (ProviderView, error) {
	in.Kind = strings.TrimSpace(in.Kind)
	if in.Name == nil || strings.TrimSpace(*in.Name) == "" {
		return ProviderView{}, errs.New(errs.ValidInvalid, "An identity provider needs a name.").
			WithRemedy("Use the name people know it by, such as Okta: it is what the sign-in page shows.")
	}
	if err := s.checkSCIMAttribute(in.SCIMIdentityAttribute); err != nil {
		return ProviderView{}, err
	}
	if err := s.checkNewID(ctx, in.ID); err != nil {
		return ProviderView{}, err
	}
	cfg, err := cleanConfig(in.Config, s.credentialFields(in.Kind))
	if err != nil {
		return ProviderView{}, err
	}
	creds, err := s.newCredentials(in.Kind, in.Credentials)
	if err != nil {
		return ProviderView{}, err
	}
	if err := s.validate(ctx, in.Kind, cfg, creds); err != nil {
		return ProviderView{}, err
	}
	if in.Enabled == nil {
		off := false
		in.Enabled = &off
	}
	created, err := s.Providers.Create(ctx, in.Kind, state.ProviderSettings{
		ID: in.ID, Name: in.Name, Config: cfg, Enabled: in.Enabled, JITProvisioning: in.JITProvisioning,
		LinkByEmail: in.LinkByEmail, SCIMIdentityAttribute: in.SCIMIdentityAttribute,
	})
	if err != nil {
		return ProviderView{}, err
	}
	for k, v := range creds {
		if err := s.Credentials.Put(ctx, created.ID, k, v); err != nil {
			_ = s.Providers.Delete(ctx, created.ID)
			return ProviderView{}, err
		}
	}
	s.audit(ctx, principalEvent(p, "identity_provider.create", "identity_provider", created.ID, map[string]any{
		"kind": created.Kind, "name": created.Name, "enabled": created.Enabled,
		"jit_provisioning": created.JITProvisioning, "link_by_email": created.LinkByEmail,
		"credentials": keys(creds),
	}))
	return s.Provider(ctx, o, created.ID)
}

// newCredentials checks a new provider's credentials are ones its kind takes,
// and keeps those given a value.
func (s *Service) newCredentials(kind string, given map[string]string) (map[string]secret.Value, error) {
	creds := map[string]secret.Value{}
	allowed := s.credentialFields(kind)
	for k, v := range given {
		if !allowed[k] {
			return nil, errs.Newf(errs.ValidInvalid, "%q is not a credential a %s provider takes.", k, kind)
		}
		if v != "" {
			creds[k] = secret.New(v)
		}
	}
	return creds, nil
}

// checkNewID checks an ID chosen ahead for a new provider: well formed, not
// the local provider's, and not taken. Empty is fine; Pando chooses one.
func (s *Service) checkNewID(ctx context.Context, providerID string) error {
	if providerID == "" {
		return nil
	}
	if !id.Is(id.IdentityAdpt, providerID) || providerID == state.LocalAdapterID {
		return errs.Newf(errs.ValidInvalid, "%q is not an identity provider ID. One is idp_ and a ULID, such as idp_01J9ZQ5Y8W3V7T2R6N4M1K0H8G.", providerID).
			WithRemedy("Leave id out and Pando chooses one.")
	}
	_, taken, err := s.Providers.ByID(ctx, providerID)
	if err != nil {
		return err
	}
	if taken {
		return state.ProviderIDTaken(providerID)
	}
	return nil
}

// UpdateProvider changes a provider. Settings replace the stored ones whole
// when given; credentials change field by field.
func (s *Service) UpdateProvider(ctx context.Context, p authz.Principal, providerID string, in ProviderInput, o Origin) (ProviderView, error) {
	cur, found, err := s.Providers.ByID(ctx, providerID)
	if err != nil {
		return ProviderView{}, err
	}
	if !found {
		return ProviderView{}, errs.New(errs.NotFound, "There is no identity provider with that ID.")
	}
	if cur.Kind == "local" && (len(in.Config) > 0 || len(in.Credentials) > 0 || in.JITProvisioning != nil ||
		in.LinkByEmail != nil || in.SCIMIdentityAttribute != nil || in.Enabled != nil) {
		return ProviderView{}, errs.New(errs.ValidInvalid, "Local accounts have no settings to change here, apart from the name.").
			WithRemedy("To turn off password sign-in, set disable_password_sign_in in host policy.")
	}
	if in.Name != nil && strings.TrimSpace(*in.Name) == "" {
		return ProviderView{}, errs.New(errs.ValidInvalid, "An identity provider needs a name.")
	}
	if err := s.checkSCIMAttribute(in.SCIMIdentityAttribute); err != nil {
		return ProviderView{}, err
	}

	cfg := cur.Config
	changed := []string{}
	if len(in.Config) > 0 {
		cfg, err = cleanConfig(in.Config, s.credentialFields(cur.Kind))
		if err != nil {
			return ProviderView{}, err
		}
		changed = append(changed, "config")
	}
	creds := map[string]secret.Value{}
	if cur.Kind != "local" {
		existing, err := s.Credentials.Resolve(ctx, cur.ID)
		if err != nil {
			return ProviderView{}, err
		}
		for k, v := range existing {
			creds[k] = v
		}
		allowed := s.credentialFields(cur.Kind)
		for k, v := range in.Credentials {
			if !allowed[k] {
				return ProviderView{}, errs.Newf(errs.ValidInvalid, "%q is not a credential a %s provider takes.", k, cur.Kind)
			}
			if v == "" {
				delete(creds, k)
			} else {
				creds[k] = secret.New(v)
			}
			changed = append(changed, "credentials."+k)
		}
		if err := s.validate(ctx, cur.Kind, cfg, creds); err != nil {
			return ProviderView{}, err
		}
	}
	if in.Enabled != nil && !*in.Enabled && cur.Enabled {
		if err := s.refuseLastWayIn(ctx, cur.ID); err != nil {
			return ProviderView{}, err
		}
	}

	for k, v := range in.Credentials {
		if err := s.Credentials.Put(ctx, cur.ID, k, secret.New(v)); err != nil {
			return ProviderView{}, err
		}
	}
	var cfgArg json.RawMessage
	if len(in.Config) > 0 {
		cfgArg = cfg
	}
	// Always written, so updated_at moves and the adapter is rebuilt with
	// new credentials even when nothing else changed.
	updated, err := s.Providers.Update(ctx, cur.ID, state.ProviderSettings{
		Name: in.Name, Config: cfgArg, Enabled: in.Enabled, JITProvisioning: in.JITProvisioning,
		LinkByEmail: in.LinkByEmail, SCIMIdentityAttribute: in.SCIMIdentityAttribute,
	})
	if err != nil {
		return ProviderView{}, err
	}
	detail := map[string]any{"changed": changed}
	if in.Name != nil {
		detail["name"] = updated.Name
	}
	if in.Enabled != nil {
		detail["enabled"] = updated.Enabled
	}
	if in.JITProvisioning != nil {
		detail["jit_provisioning"] = updated.JITProvisioning
	}
	if in.LinkByEmail != nil {
		detail["link_by_email"] = updated.LinkByEmail
	}
	if in.SCIMIdentityAttribute != nil {
		detail["scim_identity_attribute"] = updated.SCIMIdentityAttribute
	}
	s.audit(ctx, principalEvent(p, "identity_provider.update", "identity_provider", cur.ID, detail))
	return s.Provider(ctx, o, cur.ID)
}

// refuseLastWayIn refuses turning off the last external provider while
// password sign-in is off — that would leave the sign-in page with nothing on
// it, and only `pando admin` as a way back.
func (s *Service) refuseLastWayIn(ctx context.Context, providerID string) error {
	doc, err := s.Policy.Load(ctx)
	if err != nil {
		return err
	}
	if !doc.DisablePasswordSignIn {
		return nil
	}
	providers, err := s.Providers.List(ctx)
	if err != nil {
		return err
	}
	for _, other := range providers {
		if other.ID != providerID && other.Kind != "local" && other.Enabled {
			return nil
		}
	}
	return errs.New(errs.ValidInvalid,
		"Password sign-in is off, and this is the last identity provider people can sign in with.").
		WithRemedy("Turn password sign-in back on in host policy first, or turn on another provider.")
}

// DeleteProvider removes a provider nobody has signed in through.
func (s *Service) DeleteProvider(ctx context.Context, p authz.Principal, providerID string) error {
	cur, found, err := s.Providers.ByID(ctx, providerID)
	if err != nil {
		return err
	}
	if !found {
		return errs.New(errs.NotFound, "There is no identity provider with that ID.")
	}
	if cur.Enabled {
		if err := s.refuseLastWayIn(ctx, cur.ID); err != nil {
			return err
		}
	}
	if err := s.Providers.Delete(ctx, providerID); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.built, providerID)
	s.mu.Unlock()
	s.audit(ctx, principalEvent(p, "identity_provider.delete", "identity_provider", providerID,
		map[string]any{"kind": cur.Kind, "name": cur.Name}))
	return nil
}

// RotateSCIMToken turns SCIM on for a provider, or replaces its token. The
// token is returned once and kept only as a digest: whoever holds it can
// create, suspend and regroup that provider's accounts.
func (s *Service) RotateSCIMToken(ctx context.Context, p authz.Principal, providerID string) (secret.Value, error) {
	cur, found, err := s.Providers.ByID(ctx, providerID)
	if err != nil {
		return secret.Value{}, err
	}
	if !found || cur.Kind == "local" {
		return secret.Value{}, errs.New(errs.NotFound, "There is no external identity provider with that ID.")
	}
	token := "pando_scim_" + randomToken(32)
	if err := s.Providers.SetSCIMToken(ctx, providerID, Digest(token)); err != nil {
		return secret.Value{}, err
	}
	action := "identity_provider.scim.enable"
	if cur.SCIMEnabled {
		action = "identity_provider.scim.rotate"
	}
	s.audit(ctx, principalEvent(p, action, "identity_provider", providerID, nil))
	return secret.New(token), nil
}

// DisableSCIM removes a provider's SCIM token. Accounts and groups it pushed
// stay; sign-in claims own group membership again from the next sign-in.
func (s *Service) DisableSCIM(ctx context.Context, p authz.Principal, providerID string) error {
	if err := s.Providers.SetSCIMToken(ctx, providerID, ""); err != nil {
		return err
	}
	s.audit(ctx, principalEvent(p, "identity_provider.scim.disable", "identity_provider", providerID, nil))
	return nil
}

// CheckProvider asks the provider whether it answers: its discovery document
// or its metadata.
func (s *Service) CheckProvider(ctx context.Context, providerID string) error {
	a, _, err := s.Adapter(ctx, providerID)
	if err != nil {
		return err
	}
	return a.HealthCheck(ctx)
}

// ServiceMetadata is Pando's metadata for a provider — SAML SP metadata.
// Public: a provider fetches it without credentials.
func (s *Service) ServiceMetadata(ctx context.Context, o Origin, providerID string) (*api.Metadata, error) {
	a, p, err := s.Adapter(ctx, providerID)
	if err != nil {
		return nil, err
	}
	md, err := a.ServiceMetadata(ctx, s.Endpoints(o, p.ID))
	if err != nil {
		return nil, err
	}
	if md == nil {
		return nil, errs.New(errs.NotFound, "This identity provider has no metadata. Only SAML providers do.")
	}
	return md, nil
}

func keys(m map[string]secret.Value) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
