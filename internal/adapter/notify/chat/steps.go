package chat

import "github.com/trypando/pando/internal/adapter/api"

// Each platform's guided setup is one step: make the webhook, copy its URL.

func slackSteps() []api.Step {
	return webhookSteps("webhook", "Webhook", "Create an incoming webhook",
		api.Link{Label: "Open Slack apps", URL: "https://api.slack.com/apps"},
		"In Slack's app settings, choose **Create an app**, then **Blank app**, and pick your workspace.",
		"Open **Incoming Webhooks** and turn on **Activate Incoming Webhooks**.",
		"Choose **Add New Webhook to Workspace**, pick the channel, then **Allow**.",
		"Copy the URL under **Webhook URLs for Your Workspace**.")
}

func teamsSteps() []api.Step {
	return webhookSteps("workflow", "Workflow", "Create a webhook workflow",
		api.Link{Label: "Open Teams", URL: "https://teams.microsoft.com/"},
		"In Teams, select **More options** next to a standard or shared channel, then **Workflows**.",
		"Choose the **Send webhook alerts to a channel** template.",
		"Check the team and channel, then select **Save**.",
		"Copy the webhook URL from the workflow's details page.")
}

func discordSteps() []api.Step {
	return webhookSteps("webhook", "Webhook", "Create a channel webhook",
		api.Link{Label: "Open Discord", URL: "https://discord.com/app"},
		"In Discord, open **Server Settings** → **Integrations**.",
		"Choose **Create Webhook**, or **New Webhook** under **Webhooks**.",
		"Pick the channel it posts to.",
		"Choose **Copy Webhook URL**.")
}

// webhookSteps is the one step, asking for the webhook URL.
func webhookSteps(id, label, title string, link api.Link, do ...string) []api.Step {
	actions := make([]api.Action, len(do))
	for i, t := range do {
		actions[i] = api.Action{Text: t}
	}
	return []api.Step{{ID: id, Label: label, Title: title, Do: actions,
		Links: []api.Link{link}, Fields: []string{"webhook_url"}}}
}
