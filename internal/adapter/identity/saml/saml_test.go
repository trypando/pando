package saml_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/identity/saml"
	"github.com/trypando/pando/internal/errs"
)

const idpEntity = "https://idp.example.com/saml"

// idpMetadata is identity provider metadata with a real certificate.
func idpMetadata(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "idp"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert := strings.TrimSpace(strings.NewReplacer("-----BEGIN CERTIFICATE-----", "", "-----END CERTIFICATE-----", "", "\n", "").
		Replace(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))))
	return fmt.Sprintf(`<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" entityID="%s">
  <IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">
    <KeyDescriptor use="signing"><KeyInfo xmlns="http://www.w3.org/2000/09/xmldsig#"><X509Data><X509Certificate>%s</X509Certificate></X509Data></KeyInfo></KeyDescriptor>
    <SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="https://idp.example.com/sso"/>
  </IDPSSODescriptor>
</EntityDescriptor>`, idpEntity, cert)
}

func adapter(t *testing.T, extra map[string]any) *saml.Adapter {
	t.Helper()
	cfg := map[string]any{"idp_metadata_xml": idpMetadata(t)}
	for k, v := range extra {
		cfg[k] = v
	}
	raw, _ := json.Marshal(cfg)
	a := saml.New()
	require.NoError(t, a.Configure(context.Background(), raw))
	return a
}

var endpoints = api.Endpoints{
	CallbackURL: "https://pando.example.com/api/v1/auth/providers/idp_x/callback",
	EntityID:    "https://pando.example.com/api/v1/auth/providers/idp_x/metadata",
}

func TestSAMLConfigurationIsCheckedWhenSaved(t *testing.T) {
	md := idpMetadata(t)
	for name, cfg := range map[string]map[string]any{
		"no metadata":  {},
		"both":         {"idp_metadata_xml": md, "idp_metadata_url": "https://idp.example.com/md"},
		"not metadata": {"idp_metadata_xml": "<html>hello</html>"},
		"http url":     {"idp_metadata_url": "http://idp.example.com/md"},
		"transient":    {"idp_metadata_xml": md, "name_id_format": "transient"},
		"unknown":      {"idp_metadata_xml": md, "name_id_format": "kerberos"},
		"bad lifetime": {"idp_metadata_xml": md, "session_max_lifetime": "a while"},
		"sp not idp":   {"idp_metadata_xml": `<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" entityID="x"><SPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol"/></EntityDescriptor>`},
	} {
		raw, _ := json.Marshal(cfg)
		err := saml.New().Configure(context.Background(), raw)
		require.Error(t, err, name)
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), name)
	}
	require.NoError(t, adapter(t, nil).HealthCheck(context.Background()))
}

// A federation's aggregate lists many IdPs. Taking the first would trust
// whichever is listed first (GHSA-fvc9-q662-6qhg), so one with several is
// refused, nested aggregates included; one with a single IdP still works.
func TestR043_SAMLRefusesAnAggregateWithSeveralIdentityProviders(t *testing.T) {
	entity := func() string {
		md := idpMetadata(t)
		return strings.Replace(md, ` xmlns="urn:oasis:names:tc:SAML:2.0:metadata"`, "", 1)
	}
	configure := func(xml string) error {
		raw, _ := json.Marshal(map[string]any{"idp_metadata_xml": xml})
		return saml.New().Configure(context.Background(), raw)
	}
	const open, close = `<EntitiesDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata">`, `</EntitiesDescriptor>`

	require.NoError(t, configure(open+entity()+close), "one IdP")
	require.NoError(t, configure(open+`<EntitiesDescriptor>`+entity()+`</EntitiesDescriptor>`+close), "one nested IdP")

	for name, xml := range map[string]string{
		"two":    open + entity() + entity() + close,
		"nested": open + entity() + `<EntitiesDescriptor>` + entity() + `</EntitiesDescriptor>` + close,
		"none":   open + `<EntityDescriptor entityID="x"><SPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol"/></EntityDescriptor>` + close,
		// An IdP picked from an aggregate is checked as a lone one is.
		"no signing key": open + regexp.MustCompile(`(?s)<KeyDescriptor.*?</KeyDescriptor>`).ReplaceAllString(entity(), "") + close,
	} {
		err := configure(xml)
		require.Error(t, err, name)
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), name)
	}
}

func TestR043_SAMLPublishesServiceProviderMetadata(t *testing.T) {
	md, err := adapter(t, nil).ServiceMetadata(context.Background(), endpoints)
	require.NoError(t, err)
	body := string(md.Body)
	require.Contains(t, body, `entityID="`+endpoints.EntityID+`"`)
	require.Contains(t, body, endpoints.CallbackURL)
	require.Contains(t, body, "HTTP-POST")
	require.NotContains(t, body, "HTTP-Artifact")
	require.Contains(t, body, `WantAssertionsSigned="true"`)
}

func TestR043_SAMLBeginsWithARequestAndRelayState(t *testing.T) {
	r, err := adapter(t, nil).Begin(context.Background(), api.BeginRequest{Endpoints: endpoints, State: "st_4te-x"})
	require.NoError(t, err)
	u, err := url.Parse(r.URL)
	require.NoError(t, err)
	require.Equal(t, "idp.example.com", u.Host)
	require.Equal(t, "st_4te-x", u.Query().Get("RelayState"))
	require.NotEmpty(t, u.Query().Get("SAMLRequest"))
	require.Contains(t, string(r.Flow), `"r":"id-`)
}

func response(inResponseTo, nameID string) string {
	now := time.Now().UTC()
	ts := now.Format(time.RFC3339)
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf(`<samlp:Response xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol" xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion" ID="r1" Version="2.0" IssueInstant="%[1]s" Destination="%[2]s" InResponseTo="%[3]s">
  <saml:Issuer>%[4]s</saml:Issuer>
  <samlp:Status><samlp:StatusCode Value="urn:oasis:names:tc:SAML:2.0:status:Success"/></samlp:Status>
  <saml:Assertion ID="a1" Version="2.0" IssueInstant="%[1]s">
    <saml:Issuer>%[4]s</saml:Issuer>
    <saml:Subject><saml:NameID>%[5]s</saml:NameID>
      <saml:SubjectConfirmation Method="urn:oasis:names:tc:SAML:2.0:cm:bearer"><saml:SubjectConfirmationData InResponseTo="%[3]s" Recipient="%[2]s" NotOnOrAfter="%[6]s"/></saml:SubjectConfirmation>
    </saml:Subject>
    <saml:Conditions NotBefore="%[1]s" NotOnOrAfter="%[6]s"><saml:AudienceRestriction><saml:Audience>%[7]s</saml:Audience></saml:AudienceRestriction></saml:Conditions>
  </saml:Assertion>
</samlp:Response>`, ts, endpoints.CallbackURL, inResponseTo, idpEntity, nameID, now.Add(5*time.Minute).Format(time.RFC3339), endpoints.EntityID)))
}

// TestR043_SAMLRefusesAnUnsignedResponse asserts the property everything else
// rests on: a response nobody signed says nothing, however right it looks.
func TestR043_SAMLRefusesAnUnsignedResponse(t *testing.T) {
	a := adapter(t, nil)
	r, err := a.Begin(context.Background(), api.BeginRequest{Endpoints: endpoints, State: "s"})
	require.NoError(t, err)
	var f struct {
		R string `json:"r"`
	}
	require.NoError(t, json.Unmarshal(r.Flow, &f))
	_, err = a.Authenticate(context.Background(), api.Credential{
		Callback: url.Values{"SAMLResponse": {response(f.R, "admin@example.com")}}, Flow: r.Flow, Endpoints: endpoints,
	})
	require.Error(t, err)
	require.Equal(t, errs.AuthInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "signature")
}

// TestR043_SAMLRefusesUnsolicitedResponsesByDefault asserts that IdP-initiated
// sign-in is off unless an administrator turns it on.
func TestR043_SAMLRefusesUnsolicitedResponsesByDefault(t *testing.T) {
	_, err := adapter(t, nil).Authenticate(context.Background(), api.Credential{
		Callback: url.Values{"SAMLResponse": {response("", "x")}}, Endpoints: endpoints,
	})
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, "dashboard")

	// Allowed, it still has to be signed.
	_, err = adapter(t, map[string]any{"allow_idp_initiated": true}).Authenticate(context.Background(), api.Credential{
		Callback: url.Values{"SAMLResponse": {response("", "x")}}, Endpoints: endpoints,
	})
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, "signature")
}

func TestSAMLPresetsNameOnlyRealFields(t *testing.T) {
	info := saml.Info()
	keys := map[string]bool{}
	for _, f := range info.Fields {
		keys[f.Key] = true
	}
	for _, p := range info.Presets {
		for k := range p.Values {
			require.True(t, keys[k], "preset %s sets %s, which is not a field", p.ID, k)
		}
	}
}
