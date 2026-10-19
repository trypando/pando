package saml_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/beevik/etree"
	dsig "github.com/russellhaering/goxmldsig"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/identity/saml"
	"github.com/trypando/pando/internal/errs"
)

// signingIdP is an identity provider with a real key: its metadata carries the
// certificate, and it signs the assertions it issues, so what these tests
// check is what a real provider's response goes through.
type signingIdP struct {
	key  *rsa.PrivateKey
	cert *x509.Certificate
}

func newSigningIdP(t *testing.T) *signingIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "idp"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return &signingIdP{key: key, cert: cert}
}

func (p *signingIdP) metadata() string {
	return fmt.Sprintf(`<EntitiesDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata">
<EntityDescriptor entityID="%s">
  <IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">
    <KeyDescriptor use="signing"><KeyInfo xmlns="http://www.w3.org/2000/09/xmldsig#"><X509Data><X509Certificate>%s</X509Certificate></X509Data></KeyInfo></KeyDescriptor>
    <SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="https://idp.example.com/sso"/>
  </IDPSSODescriptor>
</EntityDescriptor>
</EntitiesDescriptor>`, idpEntity, base64.StdEncoding.EncodeToString(p.cert.Raw))
}

// issue builds a response to requestID ("" for one the provider started)
// naming nameID in the given format, with attributes, and signs its assertion.
func (p *signingIdP) issue(t *testing.T, requestID, nameID, format string, attrs map[string][]string) string {
	t.Helper()
	now := time.Now().UTC()
	ts, until := now.Format(time.RFC3339), now.Add(5*time.Minute).Format(time.RFC3339)
	var statements strings.Builder
	for name, values := range attrs {
		fmt.Fprintf(&statements, `<saml:Attribute Name="%s">`, name)
		for _, v := range values {
			fmt.Fprintf(&statements, `<saml:AttributeValue>%s</saml:AttributeValue>`, v)
		}
		statements.WriteString(`</saml:Attribute>`)
	}
	inResponse := ""
	if requestID != "" {
		inResponse = fmt.Sprintf(` InResponseTo="%s"`, requestID)
	}
	formatAttr := ""
	if format != "" {
		formatAttr = fmt.Sprintf(` Format="%s"`, format)
	}
	doc := etree.NewDocument()
	require.NoError(t, doc.ReadFromString(fmt.Sprintf(`<samlp:Response xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol" xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion" ID="r-%[8]d" Version="2.0" IssueInstant="%[1]s" Destination="%[2]s"%[3]s>
<saml:Issuer>%[4]s</saml:Issuer>
<samlp:Status><samlp:StatusCode Value="urn:oasis:names:tc:SAML:2.0:status:Success"/></samlp:Status>
<saml:Assertion xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion" ID="a-%[8]d" Version="2.0" IssueInstant="%[1]s"><saml:Issuer>%[4]s</saml:Issuer><saml:Subject><saml:NameID%[9]s>%[5]s</saml:NameID><saml:SubjectConfirmation Method="urn:oasis:names:tc:SAML:2.0:cm:bearer"><saml:SubjectConfirmationData%[3]s Recipient="%[2]s" NotOnOrAfter="%[6]s"/></saml:SubjectConfirmation></saml:Subject><saml:Conditions NotBefore="%[1]s" NotOnOrAfter="%[6]s"><saml:AudienceRestriction><saml:Audience>%[7]s</saml:Audience></saml:AudienceRestriction></saml:Conditions><saml:AttributeStatement>%[10]s</saml:AttributeStatement></saml:Assertion>
</samlp:Response>`, ts, endpoints.CallbackURL, inResponse, idpEntity, nameID, until, endpoints.EntityID,
		now.UnixNano(), formatAttr, statements.String())))

	assertion := doc.Root().FindElement("./Assertion")
	require.NotNil(t, assertion)
	ctx := dsig.NewDefaultSigningContext(dsig.TLSCertKeyStore(tls.Certificate{
		Certificate: [][]byte{p.cert.Raw}, PrivateKey: p.key, Leaf: p.cert,
	}))
	ctx.Canonicalizer = dsig.MakeC14N10ExclusiveCanonicalizerWithPrefixList("")
	signed, err := ctx.SignEnveloped(assertion)
	require.NoError(t, err)
	doc.Root().RemoveChild(assertion)
	doc.Root().AddChild(signed)
	out, err := doc.WriteToString()
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString([]byte(out))
}

func signingAdapter(t *testing.T, p *signingIdP, extra map[string]any) *saml.Adapter {
	t.Helper()
	cfg := map[string]any{"idp_metadata_xml": p.metadata()}
	for k, v := range extra {
		cfg[k] = v
	}
	raw, _ := json.Marshal(cfg)
	a := saml.New()
	require.NoError(t, a.Configure(context.Background(), raw))
	return a
}

// signIn begins a sign-in and returns a response to it from the provider.
func signIn(t *testing.T, a *saml.Adapter, p *signingIdP, nameID, format string, attrs map[string][]string) (api.Subject, error) {
	t.Helper()
	r, err := a.Begin(context.Background(), api.BeginRequest{Endpoints: endpoints, State: "s"})
	require.NoError(t, err)
	var f struct {
		R string `json:"r"`
	}
	require.NoError(t, json.Unmarshal(r.Flow, &f))
	return a.Authenticate(context.Background(), api.Credential{
		Callback: url.Values{"SAMLResponse": {p.issue(t, f.R, nameID, format, attrs)}}, Flow: r.Flow, Endpoints: endpoints,
	})
}

// TestR043_ASignedSAMLAssertionBecomesASubject asserts the mapping from what
// Okta, Entra and Google send to who Pando is told someone is.
func TestR043_ASignedSAMLAssertionBecomesASubject(t *testing.T) {
	p := newSigningIdP(t)

	// Entra's claim URIs, and a name from its parts.
	s, err := signIn(t, signingAdapter(t, p, nil), p, "0a21f0f2", "urn:oasis:names:tc:SAML:2.0:nameid-format:persistent",
		map[string][]string{
			"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress": {"dana@example.com"},
			"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/givenname":    {"Dana"},
			"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/surname":      {"Scully"},
			"http://schemas.microsoft.com/ws/2008/06/identity/claims/groups":     {"g1", "g2"},
		})
	require.NoError(t, err)
	require.Equal(t, "0a21f0f2", s.ExternalID)
	require.Equal(t, "dana@example.com", s.Email)
	require.False(t, s.EmailVerified, "SAML vouches for no email unless the provider is trusted to")
	require.Equal(t, "Dana Scully", s.DisplayName)
	require.Equal(t, "0a21f0f2", s.Username, "the NameID when no username attribute is sent")
	require.Equal(t, []string{"g1", "g2"}, s.Groups)
	require.NotEmpty(t, s.OneTimeID, "an assertion is used once")
	require.True(t, s.OneTimeUntil.After(time.Now()))
	require.Equal(t, []string{"0a21f0f2"}, s.Attributes["NameID"])

	// Configured names win, and a trusted provider's email is verified.
	a := signingAdapter(t, p, map[string]any{"subject_attribute": "uid", "groups_attribute": "teams",
		"name_attribute": "cn", "email_attribute": "mail", "username_attribute": "login", "trust_email": true})
	s, err = signIn(t, a, p, "transient-ignored", "", map[string][]string{
		"uid": {"u-42"}, "teams": {"eng"}, "cn": {"Fox Mulder"}, "mail": {"fox@example.com"}, "login": {"fox"},
	})
	require.NoError(t, err)
	require.Equal(t, "u-42", s.ExternalID)
	require.Equal(t, []string{"eng"}, s.Groups)
	require.Equal(t, "Fox Mulder", s.DisplayName)
	require.Equal(t, "fox", s.Username)
	require.True(t, s.EmailVerified)

	// A configured subject attribute that is missing is refused, by name.
	_, err = signIn(t, a, p, "x", "", map[string][]string{"mail": {"x@example.com"}})
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, `"uid"`)

	// An email-format NameID is the email when no attribute carries one.
	s, err = signIn(t, signingAdapter(t, p, map[string]any{"name_id_format": "email"}), p, "walter@example.com",
		"urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress", nil)
	require.NoError(t, err)
	require.Equal(t, "walter@example.com", s.Email)
}

// TestR043_SAMLRefusesATransientNameIDEvenSigned asserts that a NameID that
// changes at every sign-in is refused: it cannot identify anyone twice.
func TestR043_SAMLRefusesATransientNameIDEvenSigned(t *testing.T) {
	p := newSigningIdP(t)
	_, err := signIn(t, signingAdapter(t, p, nil), p, "_tmp123", "urn:oasis:names:tc:SAML:2.0:nameid-format:transient", nil)
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, "transient")
}

// TestR043_SAMLAcceptsProviderStartedSignInOnlyWhenAllowed asserts that a
// signed response nobody asked for is refused by default and accepted once an
// administrator allows IdP-initiated sign-in.
func TestR043_SAMLAcceptsProviderStartedSignInOnlyWhenAllowed(t *testing.T) {
	p := newSigningIdP(t)
	resp := p.issue(t, "", "u1", "", nil)
	_, err := signingAdapter(t, p, nil).Authenticate(context.Background(), api.Credential{
		Callback: url.Values{"SAMLResponse": {resp}}, Endpoints: endpoints})
	require.Error(t, err)

	s, err := signingAdapter(t, p, map[string]any{"allow_idp_initiated": true}).Authenticate(context.Background(),
		api.Credential{Callback: url.Values{"SAMLResponse": {p.issue(t, "", "u1", "", nil)}}, Endpoints: endpoints})
	require.NoError(t, err)
	require.Equal(t, "u1", s.ExternalID)

	// And a response to a different request than this flow's is refused.
	a := signingAdapter(t, p, nil)
	r, err := a.Begin(context.Background(), api.BeginRequest{Endpoints: endpoints, State: "s"})
	require.NoError(t, err)
	_, err = a.Authenticate(context.Background(), api.Credential{
		Callback: url.Values{"SAMLResponse": {p.issue(t, "id-someone-elses", "u1", "", nil)}}, Flow: r.Flow, Endpoints: endpoints})
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, "did not verify")
}

// TestSAMLRefusesResponsesItCannotRead asserts the refusals before any
// signature is looked at.
func TestSAMLRefusesResponsesItCannotRead(t *testing.T) {
	p := newSigningIdP(t)
	a := signingAdapter(t, p, nil)
	r, err := a.Begin(context.Background(), api.BeginRequest{Endpoints: endpoints, State: "s"})
	require.NoError(t, err)
	for name, cb := range map[string]url.Values{
		"none":     {},
		"not b64":  {"SAMLResponse": {"%%%"}},
		"bad flow": {"SAMLResponse": {base64.StdEncoding.EncodeToString([]byte("<x/>"))}},
	} {
		flow := r.Flow
		if name == "bad flow" {
			flow = []byte("{}")
		}
		_, err := a.Authenticate(context.Background(), api.Credential{Callback: cb, Flow: flow, Endpoints: endpoints})
		require.Error(t, err, name)
		require.Equal(t, errs.AuthInvalid, errs.CodeOf(err), name)
	}
	_, err = a.Begin(context.Background(), api.BeginRequest{Endpoints: endpoints})
	require.Error(t, err, "a sign-in needs core's state")
}

// TestSAMLReadsMetadataFromAURL asserts metadata by URL: fetched when first
// needed and on a health check, and a URL that does not serve metadata says so.
func TestSAMLReadsMetadataFromAURL(t *testing.T) {
	p := newSigningIdP(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/md", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(p.metadata())) })
	mux.HandleFunc("/html", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html></html>")) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	configure := func(path string) *saml.Adapter {
		a := saml.New().WithHTTPClient(srv.Client())
		raw, _ := json.Marshal(map[string]any{"idp_metadata_url": srv.URL + path})
		require.NoError(t, a.Configure(context.Background(), raw))
		return a
	}
	require.NoError(t, configure("/md").HealthCheck(context.Background()))
	r, err := configure("/md").Begin(context.Background(), api.BeginRequest{Endpoints: endpoints, State: "s"})
	require.NoError(t, err)
	require.Contains(t, r.URL, "https://idp.example.com/sso")

	err = configure("/missing").HealthCheck(context.Background())
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, "HTTP 404")
	err = configure("/html").HealthCheck(context.Background())
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, "not SAML identity provider metadata")
}

// TestR078_SAMLEntraGroupsOverageRefusesTheSignIn asserts that when Entra ID
// sends a link in place of more than 150 groups, the sign-in is refused with
// what to change, rather than recording that the person is in no groups.
func TestR078_SAMLEntraGroupsOverageRefusesTheSignIn(t *testing.T) {
	p := newSigningIdP(t)
	_, err := signIn(t, signingAdapter(t, p, nil), p, "0a21f0f2", "", map[string][]string{
		"http://schemas.microsoft.com/claims/groups.link": {"https://graph.windows.net/t/users/u/getMemberObjects"},
	})
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, "more than 150")
	require.Contains(t, errs.As(err).Remedy, "groups assigned to the application")

	// A link alongside the groups themselves changes nothing.
	s, err := signIn(t, signingAdapter(t, p, nil), p, "0a21f0f2", "", map[string][]string{
		"http://schemas.microsoft.com/claims/groups.link":                {"https://graph.windows.net/x"},
		"http://schemas.microsoft.com/ws/2008/06/identity/claims/groups": {"g1"},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"g1"}, s.Groups)
}

// TestSAMLOktaPresetIdentifiesPeopleByTheirOktaUserID asserts the Okta preset
// identifies a person by Okta's user ID, which never changes, and not by the
// NameID, which is the application username and changes with it; and that it
// does not vouch for an email Okta may let people edit.
func TestSAMLOktaPresetIdentifiesPeopleByTheirOktaUserID(t *testing.T) {
	var okta map[string]string
	for _, pr := range saml.Info().Presets {
		if pr.ID == "okta" {
			okta = pr.Values
		}
	}
	require.NotNil(t, okta)
	require.NotContains(t, okta, "trust_email")
	require.NotContains(t, okta, "name_id_format", "Okta's persistent NameID is still the application username")

	extra := map[string]any{}
	for k, v := range okta {
		extra[k] = v
	}
	p := newSigningIdP(t)
	a := signingAdapter(t, p, extra)
	s, err := signIn(t, a, p, "dana@example.com", "", map[string][]string{
		"okta_user_id": {"00u1abcd"}, "email": {"dana@example.com"}, "groups": {"eng"},
	})
	require.NoError(t, err)
	require.Equal(t, "00u1abcd", s.ExternalID)
	require.Equal(t, "dana@example.com", s.Username)
	require.False(t, s.EmailVerified)

	// Renamed in Okta: the same person.
	s, err = signIn(t, a, p, "dana.scully@example.com", "", map[string][]string{"okta_user_id": {"00u1abcd"}})
	require.NoError(t, err)
	require.Equal(t, "00u1abcd", s.ExternalID)
}
