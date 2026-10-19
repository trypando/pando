package saml

import "github.com/trypando/pando/internal/adapter/api"

// Attribute names a preset sets because the provider's own defaults are not
// among the ones Pando tries, or mean something else there.
const (
	entraGroups       = "http://schemas.microsoft.com/ws/2008/06/identity/claims/groups"
	authentikGroups   = "http://schemas.xmlsoap.org/claims/Group"
	authentikUsername = "http://schemas.goauthentik.io/2021/02/saml/username"
	// authentik sends the full name as the claim Microsoft uses for a username.
	authentikName = "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/name"
)

// Info describes this kind of adapter for the console and CLI forms.
func Info() api.KindInfo {
	return api.KindInfo{
		Category:    api.CategoryIdentity,
		Kind:        Kind,
		Name:        "SAML 2.0",
		Description: "Sign in through a SAML 2.0 identity provider. Pando publishes its own metadata for the provider to import.",
		IDPrefix:    "idp_",
		Fields: []api.Field{
			{Key: "idp_metadata_url", Label: "Provider metadata URL", Type: "string",
				Help:        "Where the provider publishes this application's metadata. Pando re-reads it daily, so certificate rollovers are followed.",
				Placeholder: "https://example.okta.com/app/…/sso/saml/metadata"},
			{Key: "idp_metadata_xml", Label: "Provider metadata XML", Type: "string", Multiline: true,
				Help: "Or paste the metadata instead of giving a URL."},
			{Key: "name_id_format", Label: "NameID format", Type: "select", Default: "",
				Help: "What Pando asks the provider to identify people by. The NameID must never change for a person.",
				Options: []api.Option{
					{Value: "", Label: "Unspecified", Description: "Whatever the provider is set to send."},
					{Value: "persistent", Label: "Persistent", Description: "An opaque ID that never changes."},
					{Value: "email", Label: "Email address"},
				}},
			{Key: "groups_attribute", Label: "Groups attribute", Type: "string",
				Help: "The attribute listing the person's groups. Empty tries groups, memberOf, member and Microsoft's groups claim."},
			{Key: "subject_attribute", Label: "Subject attribute", Type: "string",
				Help: "Identify people by this attribute instead of the NameID."},
			{Key: "email_attribute", Label: "Email attribute", Type: "string",
				Help: "Empty tries email, mail and Microsoft's emailaddress claim."},
			{Key: "name_attribute", Label: "Name attribute", Type: "string",
				Help: "Empty tries displayName, name, and first and last name."},
			{Key: "username_attribute", Label: "Username attribute", Type: "string",
				Help: "The attribute holding the person's username in Pando. Empty tries username, uid, login and Microsoft's name claim."},
			{Key: "trust_email", Label: "The provider's email addresses are verified", Type: "bool",
				Help: "Only if people cannot set their own email in the provider. Needed to link accounts by email."},
			{Key: "allow_idp_initiated", Label: "Allow sign-in from the provider's dashboard", Type: "bool", Advanced: true,
				Help: "Off is safer: a sign-in the provider starts is not tied to the browser that presents it."},
			{Key: "session_max_lifetime", Label: "Session length", Type: "string", Default: "12h", Advanced: true,
				Help: "How long a sign-in lasts. Without SCIM this is also how long access can outlive its removal at the provider."},
		},
		Presets: []api.Preset{
			{ID: "okta", Label: "Okta",
				Help:   "The Metadata URL is on the app's Sign On tab.",
				Values: map[string]string{"subject_attribute": "okta_user_id", "groups_attribute": "groups"}},
			{ID: "entra", Label: "Microsoft Entra ID",
				Help:   "The App Federation Metadata Url is under SAML Certificates on the app's Single sign-on page.",
				Values: map[string]string{"groups_attribute": entraGroups}},
			{ID: "google", Label: "Google Workspace",
				Help:   "Google offers the IdP metadata as a download when you add the app, not as a URL.",
				Values: map[string]string{"name_id_format": "email", "groups_attribute": "groups", "trust_email": "true"}},
			{ID: "keycloak", Label: "Keycloak",
				Help: "The metadata is at /realms/<realm>/protocol/saml/descriptor on your Keycloak host.",
				Values: map[string]string{"idp_metadata_url": "https://keycloak.example.com/realms/myrealm/protocol/saml/descriptor",
					"groups_attribute": "groups", "name_id_format": "persistent"}},
			{ID: "authentik", Label: "Authentik",
				Help: "Copy download URL, under Related objects on the provider's page, gives the metadata URL.",
				Values: map[string]string{"name_id_format": "persistent", "groups_attribute": authentikGroups,
					"username_attribute": authentikUsername, "name_attribute": authentikName}},
		},
		OtherPreset: "Another provider",
		Steps:       steps(),
	}
}
