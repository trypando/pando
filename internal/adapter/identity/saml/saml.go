// Package saml is the SAML 2.0 identity adapter (R-043).
//
// Service-provider-initiated sign-in over the HTTP-Redirect binding, with the
// response returned over HTTP-POST. Every response must be signed — the
// Response, the Assertion, or both — by a certificate from the provider's
// metadata; an unsigned one is refused whatever it says. Sign-ins the provider
// starts on its own (IdP-initiated) are refused unless an administrator turns
// them on, because nothing ties such a response to the browser presenting it.
//
// Authentication only (R-044): a verified assertion becomes a Subject, and
// core decides everything else.
package saml

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	crewjam "github.com/crewjam/saml"
	xrv "github.com/mattermost/xml-roundtrip-validator"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// Kind is the adapter's kind string.
const Kind = "saml"

// DefaultSessionLifetime is the session length when the configuration does not
// say. Without SCIM it is also the revocation window (R-050).
const DefaultSessionLifetime = 12 * time.Hour

// ClockSkew is how far the provider's clock and Pando's may disagree, in
// either direction, before an assertion is refused as not yet valid or
// expired. Three minutes, set once for every provider: the library holds it
// as a package variable.
const ClockSkew = 3 * time.Minute

func init() {
	crewjam.MaxClockSkew = ClockSkew
	// How old an assertion may be when it arrives. The library's 90 seconds
	// does not allow for skew at all, so a provider whose clock runs a little
	// behind Pando's would fail every sign-in.
	crewjam.MaxIssueDelay = 90*time.Second + ClockSkew
}

// Config is what an administrator sets.
type Config struct {
	// One of these: where the provider publishes its metadata, or the
	// metadata itself.
	MetadataURL string `json:"idp_metadata_url,omitempty"`
	MetadataXML string `json:"idp_metadata_xml,omitempty"`

	// NameIDFormat asks for a NameID format: unspecified (the default),
	// email, persistent or transient. Transient is refused: it changes at
	// every sign-in, so it cannot identify anyone.
	NameIDFormat string `json:"name_id_format,omitempty"`

	// Which attribute carries what. Empty tries the names Okta, Entra, Google,
	// Keycloak and Authentik use.
	SubjectAttribute  string `json:"subject_attribute,omitempty"`
	UsernameAttribute string `json:"username_attribute,omitempty"`
	EmailAttribute    string `json:"email_attribute,omitempty"`
	NameAttribute     string `json:"name_attribute,omitempty"`
	GroupsAttribute   string `json:"groups_attribute,omitempty"`

	// TrustEmail says the provider's email attribute is controlled by its
	// administrators, not by the people signing in. SAML has no
	// email_verified, so without this Pando never links an account by email.
	TrustEmail bool `json:"trust_email,omitempty"`

	// AllowIDPInitiated accepts sign-ins started from the provider's own
	// dashboard. Off by default: such a response is not tied to the browser
	// that presents it, so it can sign someone in as someone else.
	AllowIDPInitiated bool `json:"allow_idp_initiated,omitempty"`

	SessionMaxLifetime string `json:"session_max_lifetime,omitempty"`
}

// Adapter is the SAML identity adapter.
type Adapter struct {
	cfg      Config
	lifetime time.Duration
	client   *http.Client

	mu      sync.Mutex
	idp     *crewjam.EntityDescriptor
	fetched time.Time
}

// New builds an unconfigured adapter.
func New() *Adapter {
	return &Adapter{client: &http.Client{Timeout: 15 * time.Second}, lifetime: DefaultSessionLifetime}
}

// WithHTTPClient replaces the client used to fetch metadata, for tests.
func (a *Adapter) WithHTTPClient(c *http.Client) *Adapter { a.client = c; return a }

func (a *Adapter) Kind() string           { return Kind }
func (a *Adapter) Category() api.Category { return api.CategoryIdentity }

// Configure validates settings. Metadata given inline is parsed now, so a
// paste that is not metadata fails at save; metadata by URL is fetched at the
// first use and HealthCheck, so a provider that is down does not block saving.
func (a *Adapter) Configure(_ context.Context, raw json.RawMessage) error {
	var cfg Config
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return errs.Wrap(errs.ValidInvalid, "The SAML provider's settings could not be read.", err)
		}
	}
	cfg.MetadataURL = strings.TrimSpace(cfg.MetadataURL)
	cfg.MetadataXML = strings.TrimSpace(cfg.MetadataXML)

	var idp *crewjam.EntityDescriptor
	switch {
	case cfg.MetadataURL == "" && cfg.MetadataXML == "":
		return errs.New(errs.ValidInvalid, "A SAML provider needs its metadata, as a URL or pasted in.").
			WithRemedy("Your provider publishes it as the application's \"IdP metadata\" or \"Federation metadata\". " +
				"Give its URL, or paste the XML.")
	case cfg.MetadataURL != "" && cfg.MetadataXML != "":
		return errs.New(errs.ValidInvalid, "Give the SAML provider's metadata as a URL or pasted in, not both.")
	case cfg.MetadataXML != "":
		parsed, err := parseMetadata([]byte(cfg.MetadataXML))
		if err != nil {
			return err
		}
		idp = parsed
	default:
		u, err := url.Parse(cfg.MetadataURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && !isLoopback(u.Hostname())) {
			return errs.Newf(errs.ValidInvalid, "%q is not a usable metadata URL. Use the https:// address your provider gives.", cfg.MetadataURL)
		}
	}

	switch cfg.NameIDFormat {
	case "", "unspecified", "email", "persistent":
	case "transient":
		return errs.New(errs.ValidInvalid,
			"A transient NameID changes at every sign-in, so Pando cannot use it to recognize anyone.").
			WithRemedy("Use unspecified, email or persistent, or set a subject attribute.")
	default:
		return errs.Newf(errs.ValidInvalid, "name_id_format %q is not one Pando knows. Use unspecified, email or persistent.", cfg.NameIDFormat)
	}

	lifetime := DefaultSessionLifetime
	if cfg.SessionMaxLifetime != "" {
		d, err := time.ParseDuration(cfg.SessionMaxLifetime)
		if err != nil || d <= 0 {
			return errs.Newf(errs.ValidInvalid, "session_max_lifetime %q is not a duration such as \"12h\".", cfg.SessionMaxLifetime)
		}
		lifetime = d
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg = cfg
	a.lifetime = lifetime
	a.idp = idp
	a.fetched = time.Now()
	return nil
}

// HealthCheck makes sure the provider's metadata can be read and names a
// sign-in address and a signing certificate.
func (a *Adapter) HealthCheck(ctx context.Context) error {
	idp, err := a.metadata(ctx, true)
	if err != nil {
		return err
	}
	return checkIDP(idp)
}

// metadata returns the provider's metadata, refetching a URL daily so a
// certificate rollover the provider publishes in advance is followed.
func (a *Adapter) metadata(ctx context.Context, fresh bool) (*crewjam.EntityDescriptor, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cfg.MetadataURL == "" || (a.idp != nil && !fresh && time.Since(a.fetched) < 24*time.Hour) {
		if a.idp == nil {
			return nil, errs.New(errs.StateInvalid, "This SAML provider has no metadata configured.")
		}
		return a.idp, nil
	}
	idp, err := a.fetch(ctx)
	if err != nil {
		if a.idp != nil && !fresh {
			return a.idp, nil
		}
		return nil, err
	}
	a.idp = idp
	a.fetched = time.Now()
	return idp, nil
}

func (a *Adapter) fetch(ctx context.Context) (*crewjam.EntityDescriptor, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.MetadataURL, nil)
	if err != nil {
		return nil, errs.Wrap(errs.ValidInvalid, "The metadata URL could not be used.", err)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, errs.Wrap(errs.AdapterUnavailable, fmt.Sprintf("Pando could not fetch the SAML metadata at %s.", a.cfg.MetadataURL), err).
			WithRemedy("Check the URL, and that Pando can reach it.")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, errs.Newf(errs.AdapterUnavailable, "Fetching the SAML metadata at %s returned HTTP %d.", a.cfg.MetadataURL, resp.StatusCode).
			WithRemedy("Check the URL. Some providers only publish metadata once the application is saved.")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, errs.Wrap(errs.AdapterUnavailable, "The SAML metadata could not be read.", err)
	}
	return parseMetadata(body)
}

// parseMetadata reads an EntityDescriptor, or the first one with an IdP role
// inside an EntitiesDescriptor.
func parseMetadata(body []byte) (*crewjam.EntityDescriptor, error) {
	bad := func(err error) error {
		return errs.Wrap(errs.ValidInvalid, "That is not SAML identity provider metadata.", err).
			WithRemedy("Use the IdP metadata XML your provider publishes for this application. It starts with <EntityDescriptor.")
	}
	if err := xrv.Validate(bytes.NewReader(body)); err != nil {
		return nil, bad(err)
	}
	entity := &crewjam.EntityDescriptor{}
	err := xml.Unmarshal(body, entity)
	if err != nil && strings.Contains(err.Error(), "EntitiesDescriptor") {
		entities := &crewjam.EntitiesDescriptor{}
		if err := xml.Unmarshal(body, entities); err != nil {
			return nil, bad(err)
		}
		for i, e := range entities.EntityDescriptors {
			if len(e.IDPSSODescriptors) > 0 {
				return &entities.EntityDescriptors[i], nil
			}
		}
		return nil, bad(errors.New("no entity with an IDPSSODescriptor"))
	}
	if err != nil {
		return nil, bad(err)
	}
	if err := checkIDP(entity); err != nil {
		return nil, err
	}
	return entity, nil
}

func checkIDP(idp *crewjam.EntityDescriptor) error {
	if len(idp.IDPSSODescriptors) == 0 {
		return errs.New(errs.ValidInvalid, "This metadata describes a service provider, not an identity provider.").
			WithRemedy("Use the identity provider's metadata, not Pando's.")
	}
	sp := crewjam.ServiceProvider{IDPMetadata: idp}
	if sp.GetSSOBindingLocation(crewjam.HTTPRedirectBinding) == "" {
		return errs.New(errs.ValidInvalid,
			"The provider's metadata has no single sign-on address for the HTTP-Redirect binding, which Pando uses.")
	}
	signing := false
	for _, d := range idp.IDPSSODescriptors {
		for _, k := range d.KeyDescriptors {
			if (k.Use == "" || k.Use == "signing") && len(k.KeyInfo.X509Data.X509Certificates) > 0 {
				signing = true
			}
		}
	}
	if !signing {
		return errs.New(errs.ValidInvalid,
			"The provider's metadata has no signing certificate, so Pando could not check anything it signs.")
	}
	return nil
}

func (a *Adapter) serviceProvider(idp *crewjam.EntityDescriptor, e api.Endpoints) (*crewjam.ServiceProvider, error) {
	acs, err := url.Parse(e.CallbackURL)
	if err != nil || e.CallbackURL == "" {
		return nil, errs.New(errs.Internal, "The SAML sign-in has no assertion consumer address.")
	}
	meta, err := url.Parse(e.EntityID)
	if err != nil || e.EntityID == "" {
		return nil, errs.New(errs.Internal, "The SAML sign-in has no entity ID.")
	}
	format := crewjam.UnspecifiedNameIDFormat
	switch a.cfg.NameIDFormat {
	case "email":
		format = crewjam.EmailAddressNameIDFormat
	case "persistent":
		format = crewjam.PersistentNameIDFormat
	}
	return &crewjam.ServiceProvider{
		EntityID:          e.EntityID,
		MetadataURL:       *meta,
		AcsURL:            *acs,
		IDPMetadata:       idp,
		AuthnNameIDFormat: format,
		AllowIDPInitiated: false,
	}, nil
}

type flow struct {
	RequestID string `json:"r"`
}

// Begin builds an AuthnRequest for the HTTP-Redirect binding, with core's
// state as the RelayState.
func (a *Adapter) Begin(ctx context.Context, req api.BeginRequest) (*api.Redirect, error) {
	if req.State == "" {
		return nil, errs.New(errs.Internal, "A sign-in was started without a state.")
	}
	idp, err := a.metadata(ctx, false)
	if err != nil {
		return nil, err
	}
	sp, err := a.serviceProvider(idp, req.Endpoints)
	if err != nil {
		return nil, err
	}
	authn, err := sp.MakeAuthenticationRequest(sp.GetSSOBindingLocation(crewjam.HTTPRedirectBinding),
		crewjam.HTTPRedirectBinding, crewjam.HTTPPostBinding)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not build the SAML request.", err)
	}
	// RelayState goes into the query unescaped by the library, which is why
	// core's state is URL-safe base64.
	u, err := authn.Redirect(url.QueryEscape(req.State), sp)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not build the SAML request.", err)
	}
	body, _ := json.Marshal(flow{RequestID: authn.ID})
	return &api.Redirect{URL: u.String(), Flow: body}, nil
}

// Authenticate verifies a SAMLResponse posted to the assertion consumer
// service: signature, issuer, audience, recipient, destination, validity
// window and — for a sign-in Pando started — that it answers Pando's request.
func (a *Adapter) Authenticate(ctx context.Context, c api.Credential) (api.Subject, error) {
	encoded := c.Callback.Get("SAMLResponse")
	if encoded == "" {
		return api.Subject{}, errs.New(errs.AuthInvalid, "The identity provider sent Pando back without a SAML response.").
			WithRemedy("Start again from Pando's sign-in page. If it keeps happening, check that the provider posts to the ACS URL Pando shows.")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return api.Subject{}, errs.New(errs.AuthInvalid, "The SAML response could not be decoded.")
	}

	idp, err := a.metadata(ctx, false)
	if err != nil {
		return api.Subject{}, err
	}
	sp, err := a.serviceProvider(idp, c.Endpoints)
	if err != nil {
		return api.Subject{}, err
	}

	var possible []string
	if len(c.Flow) == 0 {
		if !a.cfg.AllowIDPInitiated {
			return api.Subject{}, errs.New(errs.AuthInvalid,
				"This sign-in was started from the identity provider's dashboard, and this installation only accepts sign-ins that start at Pando.").
				WithRemedy("Open Pando and choose this provider on its sign-in page. An administrator can allow sign-in from the provider's dashboard in the provider's settings.")
		}
		sp.AllowIDPInitiated = true
	} else {
		var f flow
		if err := json.Unmarshal(c.Flow, &f); err != nil || f.RequestID == "" {
			return api.Subject{}, errs.New(errs.AuthInvalid, "This sign-in could not be matched to the one Pando started.")
		}
		possible = []string{f.RequestID}
	}

	assertion, err := sp.ParseXMLResponse(raw, possible, sp.AcsURL)
	if err != nil {
		return api.Subject{}, responseError(err)
	}
	return a.subject(assertion)
}

// responseError says why a response was refused. The library's own message
// is "Authentication failed" whatever happened; the reason is in PrivateErr,
// and an administrator testing a provider needs it.
func responseError(err error) error {
	var bad crewjam.ErrBadStatus
	if errors.As(err, &bad) {
		return errs.Newf(errs.AuthInvalid, "The identity provider did not sign you in. Its SAML status was %s.", bad.Status).
			WithRemedy("If you are not assigned to the application in the identity provider, ask whoever manages it to assign you to Pando.")
	}
	reason := err.Error()
	var invalid *crewjam.InvalidResponseError
	if errors.As(err, &invalid) && invalid.PrivateErr != nil {
		reason = invalid.PrivateErr.Error()
		if errors.As(invalid.PrivateErr, &bad) {
			return responseError(bad)
		}
	}
	return errs.Newf(errs.AuthInvalid, "The SAML response did not verify: %s", reason).
		WithRemedy("Check that the provider's metadata in Pando is current, that the ACS URL and entity ID registered with the " +
			"provider are exactly the ones Pando shows, and that the clocks on Pando's host and the provider agree.")
}

// Well-known attribute names, tried in order when the configuration names
// none: plain names, then Microsoft's claim URIs, then the eduPerson OIDs.
var (
	emailNames = []string{"email", "mail", "emailaddress", "Email",
		"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress",
		"urn:oid:0.9.2342.19200300.100.1.3"}
	nameNames = []string{"displayName", "name", "displayname",
		"http://schemas.microsoft.com/identity/claims/displayname",
		"urn:oid:2.16.840.1.113730.3.1.241"}
	givenNames = []string{"firstName", "givenName", "given_name",
		"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/givenname", "urn:oid:2.5.4.42"}
	familyNames = []string{"lastName", "surname", "sn", "family_name",
		"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/surname", "urn:oid:2.5.4.4"}
	usernameNames = []string{"username", "uid", "login",
		"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/name", "urn:oid:0.9.2342.19200300.100.1.1"}
	groupNames = []string{"groups", "Groups", "memberOf", "member",
		"http://schemas.microsoft.com/ws/2008/06/identity/claims/groups", "urn:oid:1.3.6.1.4.1.5923.1.5.1.1"}
)

func (a *Adapter) subject(assertion *crewjam.Assertion) (api.Subject, error) {
	attrs := map[string][]string{}
	for _, st := range assertion.AttributeStatements {
		for _, at := range st.Attributes {
			for _, v := range at.Values {
				value := strings.TrimSpace(v.Value)
				if v.NameID != nil {
					value = strings.TrimSpace(v.NameID.Value)
				}
				if value == "" {
					continue
				}
				attrs[at.Name] = append(attrs[at.Name], value)
				if at.FriendlyName != "" && at.FriendlyName != at.Name {
					attrs[at.FriendlyName] = append(attrs[at.FriendlyName], value)
				}
			}
		}
	}

	var nameID, nameIDFormat string
	if assertion.Subject != nil && assertion.Subject.NameID != nil {
		nameID = strings.TrimSpace(assertion.Subject.NameID.Value)
		nameIDFormat = assertion.Subject.NameID.Format
	}

	s := api.Subject{Attributes: map[string][]string{}}
	for k, v := range attrs {
		s.Attributes[k] = v
	}
	if nameID != "" {
		s.Attributes["NameID"] = []string{nameID}
	}

	if a.cfg.SubjectAttribute != "" {
		s.ExternalID = first(attrs, a.cfg.SubjectAttribute)
		if s.ExternalID == "" {
			return api.Subject{}, errs.Newf(errs.AuthInvalid,
				"The SAML assertion has no %q attribute, which this provider is set to identify people by.", a.cfg.SubjectAttribute).
				WithRemedy("Add the attribute to the application's attribute statements in the provider, or clear the subject attribute setting to use the NameID.")
		}
	} else {
		if nameIDFormat == string(crewjam.TransientNameIDFormat) {
			return api.Subject{}, errs.New(errs.AuthInvalid,
				"The identity provider sent a transient NameID, which changes at every sign-in, so Pando cannot tell who this is.").
				WithRemedy("Set the application's NameID format to persistent, email or unspecified in the provider, or set a subject attribute in Pando.")
		}
		s.ExternalID = nameID
		if s.ExternalID == "" {
			return api.Subject{}, errs.New(errs.AuthInvalid, "The SAML assertion names nobody: it has no NameID.")
		}
	}

	s.Email = pick(attrs, a.cfg.EmailAttribute, emailNames)
	if s.Email == "" && nameIDFormat == string(crewjam.EmailAddressNameIDFormat) {
		s.Email = nameID
	}
	s.EmailVerified = a.cfg.TrustEmail && s.Email != ""
	s.DisplayName = pick(attrs, a.cfg.NameAttribute, nameNames)
	if s.DisplayName == "" {
		s.DisplayName = strings.TrimSpace(pick(attrs, "", givenNames) + " " + pick(attrs, "", familyNames))
	}
	s.Username = pick(attrs, a.cfg.UsernameAttribute, usernameNames)
	if s.Username == "" {
		s.Username = nameID
	}
	if a.cfg.GroupsAttribute != "" {
		s.Groups = attrs[a.cfg.GroupsAttribute]
	} else {
		for _, n := range groupNames {
			if len(attrs[n]) > 0 {
				s.Groups = attrs[n]
				break
			}
		}
	}

	// Entra ID sends a link instead of the groups when a person is in more
	// than 150. SAML gives Pando no token to follow it with, and a person
	// whose groups are unknown is not a person in no groups (R-078), so the
	// sign-in is refused rather than leaving every group.
	if len(s.Groups) == 0 && len(attrs[entraGroupsLink]) > 0 {
		return api.Subject{}, errs.New(errs.AuthInvalid,
			"Microsoft Entra ID left this person's groups out of the SAML assertion because they are in more than 150, so Pando cannot tell which groups they are in.").
			WithRemedy("In the enterprise application's Attributes & Claims, edit the group claim to send only groups assigned " +
				"to the application, and assign Pando the groups that matter. Or sign in with OpenID Connect, which reads them from Microsoft Graph.")
	}

	s.OneTimeID = assertion.ID
	s.OneTimeUntil = time.Now().Add(10 * time.Minute)
	if assertion.Conditions != nil && !assertion.Conditions.NotOnOrAfter.IsZero() {
		s.OneTimeUntil = assertion.Conditions.NotOnOrAfter.Add(ClockSkew)
	}
	return s, nil
}

// entraGroupsLink is the attribute Entra ID sends in place of the groups
// claim when there are too many to fit.
const entraGroupsLink = "http://schemas.microsoft.com/claims/groups.link"

func first(attrs map[string][]string, name string) string {
	if v := attrs[name]; len(v) > 0 {
		return v[0]
	}
	return ""
}

func pick(attrs map[string][]string, configured string, known []string) string {
	if configured != "" {
		return first(attrs, configured)
	}
	for _, n := range known {
		if v := first(attrs, n); v != "" {
			return v
		}
	}
	return ""
}

// ServiceMetadata is Pando's service provider metadata: its entity ID, the
// assertion consumer service for HTTP-POST, and that assertions must be
// signed. An administrator gives the provider its URL, or the file.
func (a *Adapter) ServiceMetadata(_ context.Context, e api.Endpoints) (*api.Metadata, error) {
	sp, err := a.serviceProvider(&crewjam.EntityDescriptor{}, e)
	if err != nil {
		return nil, err
	}
	md := sp.Metadata()
	// Only HTTP-POST is answered. The library also advertises the Artifact
	// binding, which would need Pando to call the provider back.
	for i := range md.SPSSODescriptors {
		var kept []crewjam.IndexedEndpoint
		for _, acs := range md.SPSSODescriptors[i].AssertionConsumerServices {
			if acs.Binding == crewjam.HTTPPostBinding {
				kept = append(kept, acs)
			}
		}
		md.SPSSODescriptors[i].AssertionConsumerServices = kept
	}
	body, err := xml.MarshalIndent(md, "", "  ")
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not write the service provider metadata.", err)
	}
	return &api.Metadata{ContentType: "application/samlmetadata+xml", Body: append([]byte(xml.Header), body...)}, nil
}

// SessionPolicy declares this adapter's session behavior (R-047). As for
// OIDC: expiry-only unless the installation turns SCIM on for the provider,
// which core then reports as push (R-048, R-050).
func (a *Adapter) SessionPolicy() api.SessionPolicy {
	return api.SessionPolicy{MaxLifetime: a.lifetime, RevocationMode: api.RevocationExpiryOnly}
}

// SupportsPush is true: the providers this targets provision through SCIM.
func (a *Adapter) SupportsPush() bool { return true }

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

var _ api.IdentityAdapter = (*Adapter)(nil)
