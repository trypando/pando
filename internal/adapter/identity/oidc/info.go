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
			{Key: "scopes", Label: "Scopes", Type: "string", Advanced: true, Default: "openid email profile",
				Help: "Space-separated. Add groups if your provider needs a scope to send them."},
			{Key: "groups_claim", Label: "Groups claim", Type: "string", Default: "groups",
				Help: "The claim that lists the person's groups. Each one becomes a synced group in Pando."},
			{Key: "subject_claim", Label: "Subject claim", Type: "string", Advanced: true, Default: "sub",
				Help: "The claim that identifies a person and never changes. Microsoft Entra ID needs oid."},
			{Key: "username_claim", Label: "Username claim", Type: "string", Advanced: true, Default: "preferred_username",
				Help: "The claim holding the person's username in Pando."},
			{Key: "email_claim", Label: "Email claim", Type: "string", Advanced: true, Default: "email",
				Help: "The claim holding the person's email address."},
			{Key: "name_claim", Label: "Name claim", Type: "string", Advanced: true, Default: "name",
				Help: "The claim holding the person's display name. When it is empty, given_name and family_name are used."},
			{Key: "hosted_domain", Label: "Google Workspace domain", Type: "string", Presets: []string{"google"},
				Help:        "Only accept Google accounts from this domain. Without it, anyone with a Google account can sign in.",
				Placeholder: "example.com"},
			{Key: "prompt", Label: "Prompt", Type: "select", Default: "",
				Help: "Ask the provider to always show its sign-in form or account chooser.",
				Options: []api.Option{
					{Value: "", Label: "Provider's choice"},
					{Value: "login", Label: "Always ask for credentials"},
					{Value: "select_account", Label: "Always offer an account choice"},
				}},
			{Key: "disable_userinfo", Label: "Skip the userinfo endpoint", Type: "bool", Advanced: true,
				Help: "Use only the ID token's claims. By default Pando fills in missing claims from userinfo."},
			{Key: "session_max_lifetime", Label: "Session length", Type: "string", Advanced: true, Default: "12h",
				Help: "How long a sign-in lasts. Without SCIM this is also how long access can outlive its removal at the provider."},
		},
		Presets: []api.Preset{
			{ID: "okta", Label: "Okta",
				Values: map[string]string{"issuer": "https://{your-okta-domain}", "groups_claim": "groups",
					"scopes": "openid email profile groups"}},
			{ID: "entra", Label: "Microsoft Entra ID",
				Help: "Groups arrive as object IDs, and Entra does not vouch for email addresses, so accounts are never linked by email.",
				Values: map[string]string{"issuer": "https://login.microsoftonline.com/{tenant-id}/v2.0",
					"subject_claim": "oid", "groups_claim": "groups", "scopes": "openid email profile User.Read"}},
			{ID: "google", Label: "Google Workspace",
				Help: "Google sends no groups, so access is given through Pando's own groups.",
				Values: map[string]string{"issuer": "https://accounts.google.com",
					"scopes": "openid email profile"}},
			{ID: "keycloak", Label: "Keycloak",
				Values: map[string]string{"issuer": "https://{keycloak-host}/realms/{realm}", "groups_claim": "groups"}},
			{ID: "authentik", Label: "Authentik",
				Help: "Authentik sends group names in the profile scope.",
				Values: map[string]string{"issuer": "https://{authentik-host}/application/o/{application-slug}/",
					"groups_claim": "groups"}},
		},
		OtherPreset: "Another provider",
		Steps:       steps(),
	}
}
