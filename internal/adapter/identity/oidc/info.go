package oidc

import "github.com/trypando/pando/internal/adapter/api"

// Info describes this kind of adapter for the console and CLI forms.
//
// The presets are where each provider's quirks live, stated once: Entra's
// sub claim differs per application, so it matches on oid; Google sends no
// groups and admits any Google account unless the domain is fixed. A preset is
// only settings — choosing one is the same as typing them.
func Info() api.KindInfo {
	return api.KindInfo{
		Category:    api.CategoryIdentity,
		Kind:        Kind,
		Name:        "OpenID Connect",
		Description: "Sign in through any OpenID Connect provider: Okta, Microsoft Entra ID, Google Workspace, Keycloak, Authentik and others.",
		IDPrefix:    "idp_",
		Fields: []api.Field{
			{Key: "issuer", Label: "Issuer URL", Type: "string", Required: true,
				Help:        "The provider's issuer. Pando reads <issuer>/.well-known/openid-configuration for the rest.",
				Placeholder: "https://example.okta.com"},
			{Key: "client_id", Label: "Client ID", Type: "string", Required: true,
				Help: "The client ID of the web application you registered for Pando."},
			{Key: "client_secret", Label: "Client secret", Type: "string", Credential: true,
				Help: "The application's client secret. Stored encrypted and never shown again."},
			{Key: "scopes", Label: "Scopes", Type: "string", Default: "openid email profile",
				Help: "Space-separated. Add groups if your provider needs a scope to send them."},
			{Key: "groups_claim", Label: "Groups claim", Type: "string", Default: "groups",
				Help: "The claim that lists the person's groups. Each one becomes a synced group in Pando."},
			{Key: "subject_claim", Label: "Subject claim", Type: "string", Default: "sub",
				Help: "The claim that identifies a person and never changes. Microsoft Entra ID needs oid."},
			{Key: "username_claim", Label: "Username claim", Type: "string", Default: "preferred_username",
				Help: "The claim holding the person's username in Pando."},
			{Key: "email_claim", Label: "Email claim", Type: "string", Default: "email",
				Help: "The claim holding the person's email address."},
			{Key: "name_claim", Label: "Name claim", Type: "string", Default: "name",
				Help: "The claim holding the person's display name. When it is empty, given_name and family_name are used."},
			{Key: "hosted_domain", Label: "Google Workspace domain", Type: "string",
				Help:        "Only accept Google accounts from this domain. Without it, anyone with a Google account can sign in.",
				Placeholder: "example.com"},
			{Key: "prompt", Label: "Prompt", Type: "select", Default: "",
				Help: "Ask the provider to always show its sign-in form or account chooser.",
				Options: []api.Option{
					{Value: "", Label: "Provider's choice"},
					{Value: "login", Label: "Always ask for credentials"},
					{Value: "select_account", Label: "Always offer an account choice"},
				}},
			{Key: "disable_userinfo", Label: "Skip the userinfo endpoint", Type: "bool",
				Help: "Use only the ID token's claims. By default Pando fills in missing claims from userinfo."},
			{Key: "session_max_lifetime", Label: "Session length", Type: "string", Default: "12h",
				Help: "How long a sign-in lasts. Without SCIM this is also how long access can outlive its removal at the provider."},
		},
		Presets: []api.Preset{
			{ID: "okta", Label: "Okta",
				Help: "In Okta, create an OIDC Web Application, set its sign-in redirect URI to the one Pando shows, " +
					"and assign it to people. Add a groups claim (Filter: Matches regex .*) to send groups.",
				Values: map[string]string{"issuer": "https://{your-okta-domain}", "groups_claim": "groups",
					"scopes": "openid email profile groups"}},
			{ID: "entra", Label: "Microsoft Entra ID",
				Help: "In Entra, register an application with a Web redirect URI set to the one Pando shows, create a " +
					"client secret, and add the groups claim under Token configuration. Groups arrive as object IDs; link each " +
					"to a Pando group to give it a name. Entra does not vouch for email addresses, so accounts are never linked by email.",
				Values: map[string]string{"issuer": "https://login.microsoftonline.com/{tenant-id}/v2.0",
					"subject_claim": "oid", "groups_claim": "groups", "scopes": "openid email profile User.Read"}},
			{ID: "google", Label: "Google Workspace",
				Help: "In Google Cloud, create an OAuth client of type Web application with the redirect URI Pando shows. " +
					"Groups are not synced from Google, so give access through Pando groups.",
				Values: map[string]string{"issuer": "https://accounts.google.com", "hosted_domain": "{your-domain.com}",
					"scopes": "openid email profile"}},
			{ID: "keycloak", Label: "Keycloak",
				Help: "In the realm, create an OpenID Connect client with client authentication on and the redirect URI Pando " +
					"shows. Add a Group Membership mapper named groups, with Full group path off.",
				Values: map[string]string{"issuer": "https://{keycloak-host}/realms/{realm}", "groups_claim": "groups"}},
			{ID: "authentik", Label: "Authentik",
				Help: "Create an OAuth2/OpenID Provider with a confidential client and the redirect URI Pando shows, and an " +
					"application using it, and choose a Signing Key on the provider. Authentik sends groups in the profile scope.",
				Values: map[string]string{"issuer": "https://{authentik-host}/application/o/{application-slug}/",
					"groups_claim": "groups"}},
		},
	}
}
