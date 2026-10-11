package saml

import "github.com/trypando/pando/internal/adapter/api"

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
			{Key: "allow_idp_initiated", Label: "Allow sign-in from the provider's dashboard", Type: "bool",
				Help: "Off is safer: a sign-in the provider starts is not tied to the browser that presents it."},
			{Key: "session_max_lifetime", Label: "Session length", Type: "string", Default: "12h",
				Help: "How long a sign-in lasts. Without SCIM this is also how long access can outlive its removal at the provider."},
		},
		Presets: []api.Preset{
			{ID: "okta", Label: "Okta",
				Help: "In Okta, create a SAML 2.0 app. Single sign-on URL is Pando's ACS URL; Audience URI is Pando's entity ID. " +
					"Add attribute statements email, displayName, and a group attribute statement groups (Matches regex .*). " +
					"Then paste the app's Metadata URL here.",
				Values: map[string]string{"name_id_format": "persistent", "groups_attribute": "groups", "trust_email": "true"}},
			{ID: "entra", Label: "Microsoft Entra ID",
				Help: "In Entra, create an enterprise application with SAML. Identifier is Pando's entity ID, Reply URL is the ACS " +
					"URL. Add a group claim. Paste the App Federation Metadata Url here.",
				Values: map[string]string{"groups_attribute": "http://schemas.microsoft.com/ws/2008/06/identity/claims/groups"}},
			{ID: "google", Label: "Google Workspace",
				Help: "In the Admin console, add a custom SAML app. ACS URL and Entity ID are Pando's. Map Primary email to email " +
					"and group membership to groups. Download the IdP metadata and paste it here.",
				Values: map[string]string{"name_id_format": "email", "groups_attribute": "groups", "trust_email": "true"}},
			{ID: "keycloak", Label: "Keycloak",
				Help: "Create a SAML client whose Client ID is Pando's entity ID and whose valid redirect URI is the ACS URL. " +
					"Add a Group list mapper named groups. The metadata is at /realms/<realm>/protocol/saml/descriptor.",
				Values: map[string]string{"idp_metadata_url": "https://{keycloak-host}/realms/{realm}/protocol/saml/descriptor",
					"groups_attribute": "groups", "name_id_format": "persistent"}},
			{ID: "authentik", Label: "Authentik",
				Help: "Create a SAML Provider with Pando's ACS URL and entity ID as audience, and an application using it. " +
					"Its metadata can be downloaded from the provider page.",
				Values: map[string]string{"groups_attribute": "http://schemas.xmlsoap.org/claims/Group"}},
		},
	}
}
