package https

import "github.com/trypando/pando/internal/adapter/api"

// steps guide setup: which SIEM first, then that SIEM's own steps. Another
// SIEM is asked for everything.
func steps() []api.Step {
	splunk, datadog, elastic, sumo, azure := []string{"splunk_hec"}, []string{"datadog"}, []string{"elastic"}, []string{"sumo_logic"}, []string{"azure_monitor"}
	other := []string{api.PresetOther}
	return []api.Step{
		{ID: "splunk_token", Label: "Token", Title: "Create an HEC token", Presets: splunk,
			Do: []api.Action{
				{Text: "In Splunk, open **Settings** → **Data inputs** → **HTTP Event Collector**."},
				{Text: "Under **Global Settings**, set **All Tokens** to **Enabled**."},
				{Text: "Choose **New Token**, finish it, and copy its value."},
			},
			Links:  []api.Link{{Label: "Splunk HEC", URL: "https://help.splunk.com/en/splunk-enterprise/get-started/get-data-in/9.4/get-data-with-http-event-collector/set-up-and-use-http-event-collector-in-splunk-web"}},
			Fields: []string{"token"}},
		{ID: "splunk_url", Label: "Endpoint", Title: "Enter the HEC endpoint", Presets: splunk,
			Do: []api.Action{
				{Text: "Use your Splunk host on the HEC port, usually `8088`, ending `/services/collector/event`."},
				{Text: "On Splunk Cloud, use `https://http-inputs-<stack>.splunkcloud.com/services/collector/event`."},
			},
			Fields: []string{"url"}},
		{ID: "datadog_key", Label: "API key", Title: "Create an API key", Presets: datadog,
			Do:     []api.Action{{Text: "In Datadog, open **Organization Settings** → **API Keys** and choose **New Key**."}},
			Links:  []api.Link{{Label: "Open Datadog API keys", URL: "https://app.datadoghq.com/organization-settings/api-keys"}},
			Fields: []string{"token"}},
		{ID: "datadog_site", Label: "Site", Title: "Choose your Datadog site", Presets: datadog,
			Do:     []api.Action{{Text: "Outside US1, change the host to your site's intake host, such as `http-intake.logs.datadoghq.eu`."}},
			Links:  []api.Link{{Label: "Datadog sites", URL: "https://docs.datadoghq.com/getting_started/site/"}},
			Fields: []string{"url"}},
		{ID: "elastic_key", Label: "API key", Title: "Create an API key", Presets: elastic,
			Do: []api.Action{
				{Text: "In Kibana, open **API keys** and choose **Create API key**."},
				{Text: "Grant `create_doc` and `auto_configure` on the data stream."},
				{Text: "Copy the **Encoded** value."},
			},
			Links:  []api.Link{{Label: "Elastic API keys", URL: "https://www.elastic.co/docs/deploy-manage/api-keys/elasticsearch-api-keys"}},
			Fields: []string{"token"}},
		{ID: "elastic_url", Label: "Endpoint", Title: "Enter the bulk endpoint", Presets: elastic,
			Do:     []api.Action{{Text: "Use your Elasticsearch endpoint, then a data stream such as `logs-pando.audit-default`, then `/_bulk`."}},
			Fields: []string{"url"}},
		{ID: "sumo_source", Label: "HTTP source", Title: "Add an HTTP source", Presets: sumo,
			Do: []api.Action{
				{Text: "In Sumo Logic, open **Data Management** → **Data Collection** → **Collection**."},
				{Text: "Choose **Add Source** beside a hosted collector, then **HTTP Logs & Metrics**."},
				{Text: "Copy its **HTTP Source Address**."},
			},
			Links:  []api.Link{{Label: "Sumo Logic HTTP source", URL: "https://www.sumologic.com/help/docs/send-data/hosted-collectors/http-source/logs-metrics/"}},
			Fields: []string{"secret_url"}},
		{ID: "azure_rule", Label: "Collection rule", Title: "Create a data collection rule", Presets: azure,
			Do: []api.Action{
				{Text: "In Azure, create a data collection rule with a stream named `Custom-PandoAudit_CL`."},
				{Text: "Give the app that mints the token the **Monitoring Metrics Publisher** role on it."},
				{Text: "Use the rule's logs ingestion endpoint and immutable ID in the URL."},
			},
			Links:  []api.Link{{Label: "Azure Logs Ingestion API", URL: "https://learn.microsoft.com/en-us/azure/azure-monitor/logs/logs-ingestion-api-overview"}},
			Fields: []string{"url"}},
		{ID: "azure_token", Label: "Token", Title: "Get an access token", Presets: azure,
			Do: []api.Action{
				{Text: "Get a Microsoft Entra ID access token for `https://monitor.azure.com`."},
				{Text: "Replace it before it expires: Pando does not refresh it."},
			},
			Fields: []string{"token"}},
		{ID: "endpoint", Label: "Endpoint", Title: "Enter the ingest endpoint", Presets: other,
			Fields: []string{"body", "url", "secret_url"}},
		{ID: "token", Label: "Token", Title: "Enter the token", Presets: other,
			Fields: []string{"token", "auth_header", "auth_scheme"}},
		{ID: "connection", Label: "Connection", Title: "Adjust the connection",
			Fields: []string{"ca_certificate", "extra_headers"}},
		{ID: "events", Label: "Events", Title: "Choose which events to send",
			Fields: []string{"actions", "exclude"}},
	}
}
