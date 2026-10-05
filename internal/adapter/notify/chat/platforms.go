package chat

import (
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
)

// Slack posts to a Slack incoming webhook.
var Slack = Platform{
	Kind:        "slack",
	Name:        "Slack",
	Description: "Posts the events a subscription chooses to a Slack channel, through an incoming webhook.",
	IDPrefix:    "ntf_",
	Placeholder: "https://hooks.slack.com/services/…",
	Help:        "The incoming webhook URL Slack gives you for the channel.",
	Hosts:       []string{"hooks.slack.com", "hooks.slack-gov.com"},
	Format:      slackMessage,
}

// Teams posts to a Microsoft Teams channel through a Workflows webhook.
var Teams = Platform{
	Kind:        "teams",
	Name:        "Microsoft Teams",
	Description: "Posts the events a subscription chooses to a Microsoft Teams channel, as an adaptive card, through a Workflows webhook.",
	IDPrefix:    "ntf_",
	Placeholder: "https://….logic.azure.com/workflows/…",
	Help:        "The URL of a Teams workflow that posts a webhook's adaptive card to the channel.",
	Format:      teamsMessage,
}

// Discord posts to a Discord channel webhook.
var Discord = Platform{
	Kind:        "discord",
	Name:        "Discord",
	Description: "Posts the events a subscription chooses to a Discord channel, through a channel webhook.",
	IDPrefix:    "ntf_",
	Placeholder: "https://discord.com/api/webhooks/…",
	Help:        "The webhook URL from the Discord channel's Integrations settings.",
	Hosts:       []string{"discord.com", "discordapp.com"},
	Format:      discordMessage,
}

// slackEscape escapes the three characters Slack's mrkdwn gives meaning.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func slackMessage(n api.Notification) any {
	text := "*" + slackEscape(n.Subject) + "*"
	if n.Body != "" {
		text += "\n" + slackEscape(n.Body)
	}
	blocks := []any{
		map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": clip(text, 3000)}},
	}
	if len(n.Fields) > 0 {
		var fields []any
		for _, f := range n.Fields {
			if len(fields) == 10 { // Slack's limit per section
				break
			}
			fields = append(fields, map[string]any{"type": "mrkdwn",
				"text": clip("*"+slackEscape(f.Label)+"*\n"+slackEscape(f.Value), 2000)})
		}
		blocks = append(blocks, map[string]any{"type": "section", "fields": fields})
	}
	if n.Link != "" {
		blocks = append(blocks, map[string]any{"type": "context", "elements": []any{
			map[string]any{"type": "mrkdwn", "text": "<" + n.Link + "|Open in Pando>"},
		}})
	}
	// text is what a notification on a phone shows; blocks are the message.
	// Escaped too: Slack reads <!channel> in it as well.
	return map[string]any{"text": clip(slackEscape(n.Subject), 3000), "blocks": blocks}
}

func teamsMessage(n api.Notification) any {
	body := []any{
		map[string]any{"type": "TextBlock", "text": n.Subject, "weight": "Bolder", "size": "Medium", "wrap": true},
	}
	if n.Body != "" {
		body = append(body, map[string]any{"type": "TextBlock", "text": n.Body, "wrap": true})
	}
	if len(n.Fields) > 0 {
		facts := make([]any, 0, len(n.Fields))
		for _, f := range n.Fields {
			facts = append(facts, map[string]any{"title": f.Label, "value": f.Value})
		}
		body = append(body, map[string]any{"type": "FactSet", "facts": facts})
	}
	card := map[string]any{
		"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
		"type":    "AdaptiveCard",
		"version": "1.4",
		"body":    body,
	}
	if n.Link != "" {
		card["actions"] = []any{map[string]any{"type": "Action.OpenUrl", "title": "Open in Pando", "url": n.Link}}
	}
	return map[string]any{
		"type": "message",
		"attachments": []any{map[string]any{
			"contentType": "application/vnd.microsoft.card.adaptive",
			"content":     card,
		}},
	}
}

func discordMessage(n api.Notification) any {
	embed := map[string]any{
		"title":       clip(n.Subject, 256),
		"description": clip(n.Body, 4096),
	}
	if n.Link != "" {
		embed["url"] = n.Link
	}
	var fields []any
	for _, f := range n.Fields {
		if len(fields) == 25 { // Discord's limit per embed
			break
		}
		fields = append(fields, map[string]any{"name": clip(f.Label, 256), "value": clip(f.Value, 1024), "inline": true})
	}
	if len(fields) > 0 {
		embed["fields"] = fields
	}
	return map[string]any{
		"username": "Pando",
		"embeds":   []any{embed},
		// Never ping @everyone because an app's name or a message said so.
		"allowed_mentions": map[string]any{"parse": []any{}},
	}
}

// clip shortens s to at most n bytes on a rune boundary, marking the cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
