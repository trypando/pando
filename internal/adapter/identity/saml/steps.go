package saml

import "github.com/trypando/pando/internal/adapter/api"

// copyMetadataURL is the last step for a provider that publishes its metadata.
const copyMetadataURL = "Copy the metadata URL"

// steps walk through registering Pando with each provider, then reading its
// metadata back.
func steps() []api.Step {
	okta, entra, google, keycloak, authentik := []string{"okta"}, []string{"entra"}, []string{"google"}, []string{"keycloak"}, []string{"authentik"}
	other := []string{api.PresetOther}
	return []api.Step{
		// Okta
		{ID: "okta_app", Label: "App", Title: "Create a SAML app in Okta", Presets: okta,
			Do: []api.Action{
				{Text: "In the Admin Console, open **Applications** → **Applications** and choose **Create App Integration**."},
				{Text: "Choose **SAML 2.0**, then **Next**, and name the app."},
				{Text: "Set **Single sign-on URL** to `{callback_url}`."},
				{Text: "Set **Audience URI (SP Entity ID)** to `{entity_id}`."},
				{Text: "Set **Name ID format** to **Persistent**."},
			},
			Links:  []api.Link{{Label: "Okta SAML app integrations", URL: "https://help.okta.com/en-us/content/topics/apps/apps_app_integration_wizard_saml.htm"}},
			Fields: []string{"name_id_format"}},
		{ID: "okta_attributes", Label: "Attributes", Title: "Send email, name and groups", Presets: okta,
			Do: []api.Action{
				{Text: "Under **Attribute Statements**, add these:", Table: &api.Table{
					Columns: []string{"Name", "Value"},
					Rows: [][]string{{"okta_user_id", "`user.getInternalProperty(\"id\")`"}, {"email", "user.email"},
						{"firstName", "user.firstName"}, {"lastName", "user.lastName"}},
				}},
				{Text: "Under **Group Attribute Statements**, add `groups` with the filter **Matches regex** `.*`."},
				{Text: "Choose **Next**, then **Finish**."},
			},
			Fields: []string{"groups_attribute"}},
		{ID: "okta_metadata", Label: "Metadata", Title: copyMetadataURL, Presets: okta,
			Do: []api.Action{
				{Text: "On the **Assignments** tab, assign the people or groups who may sign in."},
				{Text: "On the **Sign On** tab, copy the **Metadata URL**."},
			},
			Fields: []string{"idp_metadata_url"}},

		// Microsoft Entra ID
		{ID: "entra_app", Label: "App", Title: "Create an enterprise application", Presets: entra,
			Do: []api.Action{
				{Text: "In the Microsoft Entra admin center, open **Entra ID** → **Enterprise apps** → **All applications**."},
				{Text: "Choose **New application**, then **Create your own application**."},
				{Text: "Name it, choose **Integrate any other application you don't find in the gallery (Non-gallery)**, and choose **Create**."},
				{Text: "Open **Single sign-on** and choose **SAML**."},
				{Text: "In **Basic SAML Configuration**, choose **Edit**."},
				{Text: "Set **Identifier (Entity ID)** to `{entity_id}`."},
				{Text: "Set **Reply URL (Assertion Consumer Service URL)** to `{callback_url}`, then choose **Save**."},
			},
			Links:  []api.Link{{Label: "Entra SAML single sign-on", URL: "https://learn.microsoft.com/en-us/entra/identity/enterprise-apps/add-application-portal-setup-sso"}},
			Fields: []string{"name_id_format"}},
		{ID: "entra_groups", Label: "Groups", Title: "Add a group claim", Presets: entra,
			Do: []api.Action{
				{Text: "In **Attributes & Claims**, choose **Edit**, then **Add a group claim**."},
				{Text: "Choose **Groups assigned to the application**."},
				{Text: "Set **Source attribute** to **Cloud-only group display names**, then choose **Save**."},
			},
			Links:  []api.Link{{Label: "Entra group claims", URL: "https://learn.microsoft.com/en-us/entra/identity/hybrid/connect/how-to-connect-fed-group-claims"}},
			Fields: []string{"groups_attribute", "trust_email"}},
		{ID: "entra_metadata", Label: "Metadata", Title: copyMetadataURL, Presets: entra,
			Do: []api.Action{
				{Text: "Under **Users and groups**, choose **Add user/group** and assign the people or groups who may sign in."},
				{Text: "On **Single sign-on**, under **SAML Certificates**, copy the **App Federation Metadata Url**."},
			},
			Fields: []string{"idp_metadata_url"}},

		// Google Workspace
		{ID: "google_app", Label: "App", Title: "Add a custom SAML app", Presets: google,
			Do: []api.Action{
				{Text: "In the Admin console, open **Menu** → **Apps** → **Web and mobile apps**."},
				{Text: "Choose **Add App**, then **Add custom SAML app**, name it, and choose **Continue**."},
				{Text: "Download the **IDP metadata** and choose **Continue**."},
				{Text: "Open the downloaded file and paste its contents here."},
			},
			Links: []api.Link{
				{Label: "Open Web and mobile apps", URL: "https://admin.google.com/ac/apps/unified"},
				{Label: "Google custom SAML apps", URL: "https://knowledge.workspace.google.com/admin/apps/set-up-your-own-custom-saml-app"},
			},
			Fields: []string{"idp_metadata_xml"}},
		{ID: "google_sp", Label: "Service provider", Title: "Enter Pando's details", Presets: google,
			Do: []api.Action{
				{Text: "Set **ACS URL** to `{callback_url}`."},
				{Text: "Set **Entity ID** to `{entity_id}`."},
				{Text: "Set **Name ID format** to **EMAIL** and **Name ID** to **Basic Information > Primary email**, then choose **Continue**."},
			},
			Fields: []string{"name_id_format"}},
		{ID: "google_attributes", Label: "Attributes", Title: "Map the attributes", Presets: google,
			Do: []api.Action{
				{Text: "Choose **Add mapping** for each of these:", Table: &api.Table{
					Columns: []string{"Google Directory attribute", "App attribute"},
					Rows:    [][]string{{"Primary email", "email"}, {"First name", "firstName"}, {"Last name", "lastName"}},
				}},
				{Text: "Under **Group membership (optional)**, choose the groups to send and set **App attribute** to `groups`."},
				{Text: "Choose **Finish**."},
				{Text: "Open **User access**, choose **On for everyone**, and choose **Save**."},
			},
			Fields: []string{"groups_attribute", "trust_email"}},

		// Keycloak
		{ID: "keycloak_client", Label: "Client", Title: "Create a SAML client", Presets: keycloak,
			Do: []api.Action{
				{Text: "In the realm's admin console, open **Clients** and choose **Create client**."},
				{Text: "Set **Client type** to **SAML** and **Client ID** to `{entity_id}`, then choose **Next**."},
				{Text: "Set **Valid redirect URIs** to `{callback_url}`, then choose **Save**."},
				{Text: "On the **Settings** tab, set **Name ID format** to **persistent** and choose **Save**."},
				{Text: "On the **Keys** tab, turn off **Client signature required**."},
			},
			Links:  []api.Link{{Label: "Keycloak SAML clients", URL: "https://www.keycloak.org/docs/latest/server_admin/index.html#_client-saml-configuration"}},
			Fields: []string{"name_id_format"}},
		{ID: "keycloak_mappers", Label: "Attributes", Title: "Send email, name and groups", Presets: keycloak,
			Do: []api.Action{
				{Text: "On the **Client scopes** tab, open the client's **-dedicated** scope."},
				{Text: "Choose **Add predefined mapper** and add **X500 email**, **X500 givenName** and **X500 surname**."},
				{Text: "Choose **Add mapper** → **By configuration** → **User Property**, with **Property** `username` and **SAML Attribute Name** `username`."},
				{Text: "Choose **Add mapper** → **By configuration** → **Group list**, with **Group attribute name** `groups`, and turn off **Full group path**."},
			},
			Fields: []string{"groups_attribute", "trust_email"}},
		{ID: "keycloak_metadata", Label: "Metadata", Title: "Enter the realm's metadata URL", Presets: keycloak,
			Do:     []api.Action{{Text: "Use your Keycloak host and realm in `https://<host>/realms/<realm>/protocol/saml/descriptor`."}},
			Fields: []string{"idp_metadata_url"}},

		// Authentik
		{ID: "authentik_app", Label: "Application", Title: "Create an application", Presets: authentik,
			Do: []api.Action{
				{Text: "In the Admin interface, open **Applications** → **Applications** and choose **New Application**."},
				{Text: "Name it and choose **Next**, then choose **SAML Provider** and **Next**."},
				{Text: "Set **ACS URL** to `{callback_url}` and **Audience** to `{entity_id}`."},
				{Text: "Under **Advanced protocol settings**, choose a **Signing Certificate** and leave **Sign assertions** on."},
				{Text: "Set **Service Provider Binding** to **Post**, then choose **Create Application**."},
			},
			Links:  []api.Link{{Label: "Authentik SAML providers", URL: "https://docs.goauthentik.io/add-secure-apps/providers/saml/create-saml-provider/"}},
			Fields: []string{"name_id_format"}},
		{ID: "authentik_metadata", Label: "Metadata", Title: copyMetadataURL, Presets: authentik,
			Do: []api.Action{
				{Text: "Open **Applications** → **Providers** and choose the provider."},
				{Text: "Under **Related objects**, beside **Metadata**, choose **Copy download URL**."},
			},
			Fields: []string{"idp_metadata_url"}},

		// Another provider
		{ID: "other_app", Label: "Application", Title: "Register Pando with your provider", Presets: other,
			Do: []api.Action{
				{Text: "Create a SAML 2.0 application."},
				{Text: "Set its ACS URL, or reply URL, to `{callback_url}`, with the HTTP-POST binding."},
				{Text: "Set its entity ID, or audience, to `{entity_id}`."},
				{Text: "Or import Pando's metadata from `{entity_id}`."},
				{Text: "Sign the response, the assertion, or both."},
				{Text: "Assign the people or groups who may sign in."},
			},
			Fields: []string{"name_id_format"}},
		{ID: "other_attributes", Label: "Attributes", Title: "Map the attributes", Presets: other,
			Do: []api.Action{{Text: "Send each person's email, name, username and groups as attributes."}},
			Fields: []string{"email_attribute", "name_attribute", "username_attribute", "groups_attribute",
				"subject_attribute", "trust_email"}},
		{ID: "other_metadata", Label: "Metadata", Title: "Enter the provider's metadata", Presets: other,
			Fields: []string{"idp_metadata_url", "idp_metadata_xml"}},
	}
}
