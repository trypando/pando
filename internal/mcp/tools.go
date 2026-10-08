package mcp

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
)

// The tool catalog (design 04 §3).
//
// One tool per endpoint, and the mapping is deliberately boring: a tool that
// composed several calls would be a capability the CLI and console do not have,
// which is the thing R-261 forbids. If an agent needs a workflow, it makes the
// calls.
//
// Exec, secret value reads, grant mutation, policy mutation and user deletion
// are absent. Not because this list is the boundary — host policy is (O-12) —
// but because offering a tool the policy will refuse wastes the agent's turn
// and teaches it that Pando's tools fail randomly.

type tool struct {
	Name        string
	Description string
	Schema      map[string]any

	// request turns arguments into an API call.
	request func(args map[string]any) (method, path string, body any, err error)
}

// Bytes is a request body sent as-is rather than encoded as JSON, for the
// endpoints whose body is a file.
type Bytes struct {
	ContentType string
	Data        []byte
}

func stringArg(args map[string]any, key string, required bool) (string, error) {
	v, ok := args[key]
	if !ok || v == nil {
		if required {
			return "", fmt.Errorf("%s is required", key)
		}
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", key)
	}
	if required && s == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return s, nil
}

// appPath builds a path under an app, escaping the ID.
//
// Escaped even though app IDs are Pando's own prefixed ULIDs: the ID here comes
// from an agent, which means it comes from a model, which means it can be
// anything at all.
func appPath(id, suffix string) string {
	return "/apps/" + url.PathEscape(id) + suffix
}

// decisionRequest is approving or rejecting a deploy waiting for approval
// (R-154): POST .../deployments/{id}/<action>, with an optional comment.
func decisionRequest(action string) func(args map[string]any) (string, string, any, error) {
	return func(args map[string]any) (string, string, any, error) {
		appID, err := stringArg(args, "app_id", true)
		if err != nil {
			return "", "", nil, err
		}
		depID, err := stringArg(args, "deployment_id", true)
		if err != nil {
			return "", "", nil, err
		}
		body := map[string]any{}
		if comment, _ := stringArg(args, "comment", false); comment != "" {
			body["comment"] = comment
		}
		return "POST", appPath(appID, "/deployments/"+url.PathEscape(depID)+action), body, nil
	}
}

// pagedGet is a GET of a paged list with the given optional string arguments
// passed through as query parameters (design 04 §1).
func pagedGet(path string, args map[string]any, keys ...string) (string, string, any, error) {
	q := url.Values{}
	for _, key := range keys {
		v, err := stringArg(args, key, false)
		if err != nil {
			return "", "", nil, err
		}
		if v != "" {
			q.Set(key, v)
		}
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return "GET", path, nil, nil
}

func schema(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
}

// registryCredentialSchema describes a registry credential (issue #41).
func registryCredentialSchema(description string) map[string]any {
	return map[string]any{
		"type": "object",
		"description": description + " Either kind basic with username and password (a token " +
			"that can read the image), or kind ecr with access_key_id, secret_access_key and " +
			"optionally region, for AWS ECR.",
		"properties": map[string]any{
			"kind":              map[string]any{"type": "string", "enum": []string{"basic", "ecr"}},
			"username":          str("With basic: the registry username."),
			"password":          str("With basic: the password or access token."),
			"access_key_id":     str("With ecr: the AWS access key ID."),
			"secret_access_key": str("With ecr: the AWS secret access key."),
			"region":            str("With ecr: the region. Optional; read from the registry host."),
		},
		"required": []string{"kind"},
	}
}

func str(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func strList(description string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": description}
}

// stringListArg reads an optional array of strings.
func stringListArg(args map[string]any, key string) ([]string, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return nil, nil
	}
	raw, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array of strings", key)
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("%s must be an array of strings", key)
		}
		if s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

var toolList = []tool{
	{
		Name: "pando_list_apps",
		Description: "List the apps you can manage, with their current state, newest first, a page at a time. " +
			"`total` is how many match; pass `next_cursor` back as `cursor` for the next page.",
		Schema: schema(map[string]any{
			"q":      str("Only apps whose name or slug contains this."),
			"cursor": str("The next_cursor from a previous page."),
		}),
		request: func(args map[string]any) (string, string, any, error) {
			return pagedGet("/apps", args, "q", "cursor")
		},
	},
	{
		Name: "pando_get_app",
		Description: "Get one app: its name, source, state and pinned spec. Once the app has been " +
			"through detection, `detection` says where that has got to — `status`, and `stage` while " +
			"it is running — which is why an app in draft is still in draft.",
		Schema: schema(map[string]any{"app_id": str("The app's ID.")}, "app_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return "GET", appPath(id, ""), nil, nil
		},
	},
	{
		Name: "pando_create_app",
		Description: "Create an app from a git repository (source_url), from an image that is " +
			"already built (image), or from files you will send next (upload: true, then " +
			"pando_upload_source). Give exactly one. Returns immediately with the app in draft " +
			"while Pando works out how to run it; call pando_get_detection next, with " +
			"wait_seconds to wait for it to finish. For an upload, detection starts when " +
			"pando_upload_source is followed by pando_rerun_detection.",
		Schema: schema(map[string]any{
			"name":       str("A short name for the app."),
			"source_url": str("The repository URL."),
			"ref":        str("Branch or tag. Optional, with source_url."),
			"image":      str("An image reference such as ghcr.io/acme/web:1.4, run as it is."),
			"upload": map[string]any{
				"type":        "boolean",
				"description": "True to create the app for files sent with pando_upload_source.",
			},
			"registry_credential": registryCredentialSchema(
				"Optional, with image: the credential a private image is pulled with."),
			"connection": str("Optional, with source_url: the source connection (from pando_list_sources) " +
				"to read a private repository with. Left out, the one covering the URL is used."),
		}, "name"),
		request: func(args map[string]any) (string, string, any, error) {
			name, err := stringArg(args, "name", true)
			if err != nil {
				return "", "", nil, err
			}
			source, _ := stringArg(args, "source_url", false)
			image, _ := stringArg(args, "image", false)
			upload, _ := args["upload"].(bool)
			given := 0
			for _, set := range []bool{source != "", image != "", upload} {
				if set {
					given++
				}
			}
			if given != 1 {
				return "", "", nil, fmt.Errorf("give exactly one of source_url, image, or upload: true")
			}
			switch {
			case image != "":
				src := map[string]any{"type": "image", "image": image}
				if cred, ok := args["registry_credential"].(map[string]any); ok {
					src["credential"] = cred
				}
				return "POST", "/apps", map[string]any{"name": name, "source": src}, nil
			case upload:
				return "POST", "/apps", map[string]any{"name": name, "source": map[string]string{"type": "upload"}}, nil
			}
			ref, _ := stringArg(args, "ref", false)
			src := map[string]string{"type": "git", "url": source, "ref": ref}
			if conn, _ := stringArg(args, "connection", false); conn != "" {
				src["connection"] = conn
			}
			return "POST", "/apps", map[string]any{"name": name, "source": src}, nil
		},
	},
	{
		Name: "pando_list_sources",
		Description: "The installation's source connections: how Pando reads private repositories on " +
			"GitHub, GitLab, Azure DevOps, Bitbucket, Gitea or any git host. Each has an id, the host " +
			"and scope it covers, how it signs in, whether it can list repositories, and whether it is " +
			"authorized; problem says why one cannot be used. A private repository is read with the " +
			"connection that covers its URL.",
		Schema: schema(map[string]any{}),
		request: func(map[string]any) (string, string, any, error) {
			return "GET", "/sources", nil, nil
		},
	},
	{
		Name: "pando_list_source_repositories",
		Description: "The repositories a source connection can read, to pick one for pando_create_app " +
			"rather than guess its URL. Refused for a connection with no API access, such as an SSH key.",
		Schema: schema(map[string]any{
			"source_id": str("The connection's id, from pando_list_sources."),
			"query":     str("Optional: only repositories whose name contains this."),
		}, "source_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "source_id", true)
			if err != nil {
				return "", "", nil, err
			}
			path := "/sources/" + url.PathEscape(id) + "/repositories"
			if q, _ := stringArg(args, "query", false); q != "" {
				path += "?q=" + url.QueryEscape(q)
			}
			return "GET", path, nil, nil
		},
	},
	{
		Name:        "pando_list_source_branches",
		Description: "The branches of a repository, read through a source connection.",
		Schema: schema(map[string]any{
			"source_id": str("The connection's id, from pando_list_sources."),
			"url":       str("The repository's URL."),
		}, "source_id", "url"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "source_id", true)
			if err != nil {
				return "", "", nil, err
			}
			repo, err := stringArg(args, "url", true)
			if err != nil {
				return "", "", nil, err
			}
			return "GET", "/sources/" + url.PathEscape(id) + "/branches?url=" + url.QueryEscape(repo), nil, nil
		},
	},
	{
		Name: "pando_authorize_source",
		Description: "Start signing a source connection in with OAuth by device code. Returns user_code " +
			"and verification_url: tell the person to open the URL and enter the code, then call " +
			"pando_poll_source_authorization every interval_seconds until it says authorized.",
		Schema: schema(map[string]any{"source_id": str("The connection's id, from pando_list_sources.")}, "source_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "source_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return "POST", "/sources/" + url.PathEscape(id) + "/authorize", map[string]string{"mode": "device"}, nil
		},
	},
	{
		Name: "pando_poll_source_authorization",
		Description: "Ask once whether the person approved a device authorization started with " +
			"pando_authorize_source: status pending (slow_down asks for slower polling) or authorized.",
		Schema: schema(map[string]any{"source_id": str("The connection's id.")}, "source_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "source_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return "POST", "/sources/" + url.PathEscape(id) + "/authorize/poll", nil, nil
		},
	},
	{
		// Issue #80: an agent was told to call this next and left to guess how
		// often. The description says what running means and how to wait.
		Name: "pando_get_detection",
		Description: "What Pando worked out about an app, including any questions it needs " +
			"answered before it can deploy. The questions are written to be answerable by " +
			"whatever wrote the app. `status` is `running` while Pando is still working: call " +
			"again. While running, `stage` says what it is doing — fetching (cloning the " +
			"repository), detecting (working out what the app is), trying (a trial run), " +
			"scanning (a security scan) or screening (an AI adapter checking the plan) — and " +
			"`elapsed_seconds` how long it has taken so far; a trial run can take several " +
			"minutes. Pass wait_seconds (up to 60) to have the call return as soon as the stage " +
			"changes or detection finishes, instead of calling repeatedly. When it has " +
			"finished, `status` is ready (call pando_accept_proposal), needs_answers or unknown " +
			"(answer the questions with pando_answer_detection), blocked or failed (the " +
			"`detection` field says why).",
		Schema: schema(map[string]any{
			"app_id": str("The app's ID."),
			"wait_seconds": map[string]any{
				"type": "integer",
				"description": "Optional. While detection is running, wait up to this many seconds " +
					"(at most 60) for it to move on before answering. 0 or absent answers at once.",
				"minimum": 0,
				"maximum": 60,
			},
		}, "app_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			path := appPath(id, "/detection")
			if raw, ok := args["wait_seconds"]; ok && raw != nil {
				seconds, isNumber := raw.(float64)
				if !isNumber || seconds < 0 || seconds != float64(int(seconds)) {
					return "", "", nil, fmt.Errorf("wait_seconds must be a whole number of seconds, 0 to 60")
				}
				if seconds > 0 {
					path += fmt.Sprintf("?wait=%d", int(seconds))
				}
			}
			return "GET", path, nil, nil
		},
	},
	{
		Name:        "pando_answer_detection",
		Description: "Answer one of the questions from pando_get_detection.",
		Schema: schema(map[string]any{
			"app_id": str("The app's ID."),
			"key":    str("The question's key."),
			"answer": str("The answer."),
		}, "app_id", "key", "answer"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			key, err := stringArg(args, "key", true)
			if err != nil {
				return "", "", nil, err
			}
			answer, err := stringArg(args, "answer", true)
			if err != nil {
				return "", "", nil, err
			}
			return "POST", appPath(id, "/detection/answers"),
				map[string]any{"answers": map[string]string{key: answer}}, nil
		},
	},
	{
		Name: "pando_accept_proposal",
		Description: "Accept what Pando worked out and pin it as the app's setup, optionally setting " +
			"environment variables in the same step. This does not deploy — call pando_deploy after.",
		Schema: schema(map[string]any{
			"app_id": str("The app's ID."),
			"values": map[string]any{
				"type":                 "object",
				"description":          "Environment variables to set, name to value, e.g. {\"API_URL\": \"https://api\"}. Stored as ordinary variables, not secrets.",
				"additionalProperties": map[string]any{"type": "string"},
			},
		}, "app_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			values := []map[string]any{}
			if raw, ok := args["values"].(map[string]any); ok {
				for key, v := range raw {
					value, isString := v.(string)
					if !isString {
						return "", "", nil, fmt.Errorf("values.%s must be a string", key)
					}
					values = append(values, map[string]any{"key": key, "value": value})
				}
			}
			return "POST", appPath(id, "/detection/accept"), map[string]any{"values": values}, nil
		},
	},
	{
		Name: "pando_plan",
		Description: "Show what a deploy would do, without doing it. Side-effect free, so it is " +
			"safe to call after any change to check the change is deployable.",
		Schema: schema(map[string]any{"app_id": str("The app's ID.")}, "app_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return "POST", appPath(id, "/plan"), map[string]any{}, nil
		},
	},
	{
		Name: "pando_deploy",
		Description: "Deploy an app. Returns once the deployment has been accepted, not once it is running. " +
			"When the deploy needs somebody's approval, it comes back with status `awaiting_approval` and " +
			"`approval_reasons` saying why; it runs once a person approves it.",
		Schema: schema(map[string]any{
			"app_id":          str("The app's ID."),
			"idempotency_key": str("A key you choose. Retrying with the same key will not deploy twice."),
		}, "app_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			key, _ := stringArg(args, "idempotency_key", false)
			body := map[string]any{}
			if key != "" {
				body["idempotency_key"] = key
			}
			return "POST", appPath(id, "/deployments"), body, nil
		},
	},
	{
		Name: "pando_list_approvals",
		Description: "List the deploys waiting for approval on every app you can see. Each says which app, " +
			"why it needs approval, the approvals it has so far, and `can_decide`: whether you may approve or reject it. " +
			"Oldest first, a page at a time: pass `next_cursor` back as `cursor` for the next page.",
		Schema: schema(map[string]any{
			"cursor": str("The next_cursor from a previous page."),
		}),
		request: func(args map[string]any) (string, string, any, error) {
			return pagedGet("/approvals", args, "cursor")
		},
	},
	{
		Name: "pando_approve_deploy",
		Description: "Approve a deploy that is waiting for approval. When it is the last approval the deploy " +
			"needs, the deploy starts. Approval is a person's sign-off, so by default an installation does not " +
			"let an agent's token approve; the refusal says so.",
		Schema: schema(map[string]any{
			"app_id":        str("The app's ID."),
			"deployment_id": str("The deploy's ID, as pando_list_approvals or pando_deploy returned it."),
			"comment":       str("Optional. A note recorded with the approval."),
		}, "app_id", "deployment_id"),
		request: decisionRequest("/approve"),
	},
	{
		Name: "pando_reject_deploy",
		Description: "Reject a deploy that is waiting for approval. One rejection ends the request, and the " +
			"deploy does not run. Approval is a person's sign-off, so by default an installation does not let " +
			"an agent's token reject either.",
		Schema: schema(map[string]any{
			"app_id":        str("The app's ID."),
			"deployment_id": str("The deploy's ID, as pando_list_approvals or pando_deploy returned it."),
			"comment":       str("Optional. Why, recorded with the rejection and shown to whoever asked."),
		}, "app_id", "deployment_id"),
		request: decisionRequest("/reject"),
	},
	{
		Name: "pando_get_logs",
		Description: "Read an app's recent logs. An app can be made of several parts — a web " +
			"service, a worker, a database it brought with it — and each has its own log. " +
			"Without `workload` this is the primary part, the one the app's address resolves " +
			"to; pando_get_status lists the names. Returns the most recent lines: 200 unless " +
			"`tail` says otherwise, and never more than 5000.",
		Schema: schema(map[string]any{
			"app_id":   str("The app's ID."),
			"workload": str("Which part of the app to read. Defaults to the primary one."),
			"tail": map[string]any{
				"type":        "integer",
				"description": "Optional. How many of the most recent lines to return, 1 to 5000. Defaults to 200.",
				"minimum":     1,
				"maximum":     5000,
			},
		}, "app_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			workload, err := stringArg(args, "workload", false)
			if err != nil {
				return "", "", nil, err
			}
			query := url.Values{}
			if workload != "" {
				query.Set("workload", workload)
			}
			if raw, ok := args["tail"]; ok && raw != nil {
				lines, isNumber := raw.(float64)
				if !isNumber || lines < 1 || lines != float64(int(lines)) {
					return "", "", nil, fmt.Errorf("tail must be a whole number of lines, 1 to 5000")
				}
				query.Set("tail", fmt.Sprint(int(lines)))
			}
			path := "/logs"
			if len(query) > 0 {
				path += "?" + query.Encode()
			}
			return "GET", appPath(id, path), nil, nil
		},
	},
	{
		Name: "pando_stop_app",
		Description: "Stop an app without deleting it. Its storage, configuration and address " +
			"are kept, and it stays stopped until something starts it again.",
		Schema: schema(map[string]any{"app_id": str("The app's ID.")}, "app_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return "POST", appPath(id, "/stop"), map[string]any{}, nil
		},
	},
	{
		Name:        "pando_start_app",
		Description: "Start an app that was stopped, bringing back the version that was running.",
		Schema:      schema(map[string]any{"app_id": str("The app's ID.")}, "app_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return "POST", appPath(id, "/start"), map[string]any{}, nil
		},
	},
	{
		Name: "pando_restart_app",
		Description: "Restart an app's workloads in place. Nothing is rebuilt and nothing is " +
			"re-read — the same version, started again.",
		Schema: schema(map[string]any{"app_id": str("The app's ID.")}, "app_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return "POST", appPath(id, "/restart"), map[string]any{}, nil
		},
	},
	{
		Name: "pando_set_app_icon",
		Description: "Set the image shown on an app's launcher tile. The image is a PNG, JPEG, " +
			"WebP or GIF file of at most 256 KB, base64-encoded. SVG is not accepted.",
		Schema: schema(map[string]any{
			"app_id":       str("The app's ID."),
			"image_base64": str("The image file's bytes, base64-encoded."),
		}, "app_id", "image_base64"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			encoded, err := stringArg(args, "image_base64", true)
			if err != nil {
				return "", "", nil, err
			}
			data, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return "", "", nil, fmt.Errorf("image_base64 is not valid base64: %w", err)
			}
			return "PUT", appPath(id, "/icon"), Bytes{ContentType: "application/octet-stream", Data: data}, nil
		},
	},
	{
		Name:        "pando_clear_app_icon",
		Description: "Remove the image on an app's launcher tile, so the tile shows the map generated for it.",
		Schema:      schema(map[string]any{"app_id": str("The app's ID.")}, "app_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return "DELETE", appPath(id, "/icon"), nil, nil
		},
	},
	{
		Name: "pando_upload_source",
		Description: "Send an app's files as its source: a gzipped tar of the app's directory, " +
			"base64-encoded, at most 256 MB compressed. Paths in the archive are relative to the " +
			"app's root; leave out .git, node_modules and build output. A single index.html is " +
			"enough for a static site. Replaces any files sent before. Then call " +
			"pando_rerun_detection so Pando works out how to run them.",
		Schema: schema(map[string]any{
			"app_id":         str("The app's ID."),
			"archive_base64": str("The gzipped tar's bytes, base64-encoded."),
		}, "app_id", "archive_base64"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			encoded, err := stringArg(args, "archive_base64", true)
			if err != nil {
				return "", "", nil, err
			}
			data, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return "", "", nil, fmt.Errorf("archive_base64 is not valid base64: %w", err)
			}
			return "POST", appPath(id, "/source"), Bytes{ContentType: "application/gzip", Data: data}, nil
		},
	},
	{
		Name: "pando_rerun_detection",
		Description: "Work out again how to run an app, from its source as it is now: after " +
			"pando_upload_source, after the repository changed, or after setting a registry " +
			"credential for a private image. Nothing changes until the new proposal is accepted " +
			"(R-022). Then call pando_get_detection with wait_seconds.",
		Schema: schema(map[string]any{"app_id": str("The app's ID.")}, "app_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return "POST", appPath(id, "/detection/rerun"), nil, nil
		},
	},
	{
		Name: "pando_set_registry_credential",
		Description: "Set the credential an image app's private image is pulled with, replacing " +
			"any it had. It belongs to the app and is never given to it. Pando never shows it " +
			"again; pando_get_registry_credential says only which kind is set.",
		Schema: schema(map[string]any{
			"app_id":              str("The app's ID."),
			"registry_credential": registryCredentialSchema("The credential."),
		}, "app_id", "registry_credential"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			cred, ok := args["registry_credential"].(map[string]any)
			if !ok {
				return "", "", nil, fmt.Errorf("registry_credential is required")
			}
			return "PUT", appPath(id, "/registry-credential"), cred, nil
		},
	},
	{
		Name:        "pando_get_registry_credential",
		Description: "Whether an image app has a registry credential, and of which kind, with its username or access key ID. Never the password or secret key.",
		Schema:      schema(map[string]any{"app_id": str("The app's ID.")}, "app_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return "GET", appPath(id, "/registry-credential"), nil, nil
		},
	},
	{
		Name:        "pando_remove_registry_credential",
		Description: "Remove an image app's registry credential, so its image is pulled anonymously.",
		Schema:      schema(map[string]any{"app_id": str("The app's ID.")}, "app_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return "DELETE", appPath(id, "/registry-credential"), nil, nil
		},
	},
	{
		Name: "pando_get_auto_deploy",
		Description: "Whether an app deploys automatically when its repository changes, and how: " +
			"`settings` on the newest configuration and `deployed` on the running one, with `pending` " +
			"when they differ; `paused` when approval now stops it; `last_check`, what Pando last found " +
			"and the last commit it tried (each commit is tried once) and why it went nowhere; and the " +
			"webhook URL, with whether a webhook secret is set.",
		Schema: schema(map[string]any{"app_id": str("The app's ID.")}, "app_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return "GET", appPath(id, "/auto-deploy"), nil, nil
		},
	},
	{
		Name: "pando_set_auto_deploy",
		Description: "Turn automatic deploys on or off for an app, and choose what deploys: each new " +
			"commit on a branch (trigger branch_updated, the default; branch empty follows the branch " +
			"the app was deployed from), or each new release tag (trigger release_tagged; a release is " +
			"a tag such as v1.2.3, or one matching tag_pattern, such as release-*). Saved as a new " +
			"configuration that takes effect at the app's next deploy. Refused while the app's deploys " +
			"need approval.",
		Schema: schema(map[string]any{
			"app_id":      str("The app's ID."),
			"enabled":     map[string]any{"type": "boolean", "description": "Whether the app deploys automatically."},
			"trigger":     map[string]any{"type": "string", "enum": []string{"branch_updated", "release_tagged"}},
			"branch":      str("With branch_updated: the branch to follow."),
			"tag_pattern": str("With release_tagged: which tags are releases, such as release-*."),
		}, "app_id", "enabled"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			enabled, ok := args["enabled"].(bool)
			if !ok {
				return "", "", nil, fmt.Errorf("enabled is required, true or false")
			}
			body := map[string]any{"enabled": enabled}
			for _, key := range []string{"trigger", "branch", "tag_pattern"} {
				v, err := stringArg(args, key, false)
				if err != nil {
					return "", "", nil, err
				}
				if v != "" {
					body[key] = v
				}
			}
			return "PUT", appPath(id, "/auto-deploy"), body, nil
		},
	},
	{
		Name: "pando_favorite_app",
		Description: "Pin an app to the top of your own launcher. It grants nothing and only you " +
			"see it; you must be able to open the app.",
		Schema: schema(map[string]any{"app_id": str("The app's ID.")}, "app_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return "PUT", "/me/favorites/" + url.PathEscape(id), nil, nil
		},
	},
	{
		Name:        "pando_unfavorite_app",
		Description: "Unpin an app from your launcher.",
		Schema:      schema(map[string]any{"app_id": str("The app's ID.")}, "app_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return "DELETE", "/me/favorites/" + url.PathEscape(id), nil, nil
		},
	},
	{
		Name:        "pando_rename_app",
		Description: "Change an app's display name. Its ID and address do not change.",
		Schema: schema(map[string]any{
			"app_id": str("The app's ID."),
			"name":   str("The new name."),
		}, "app_id", "name"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			name, err := stringArg(args, "name", true)
			if err != nil {
				return "", "", nil, err
			}
			return "PATCH", appPath(id, ""), map[string]any{"name": name}, nil
		},
	},
	{
		Name: "pando_list_my_apps",
		Description: "The apps you can open — your launcher — with which are favorites, which of " +
			"your sections each is filed under, and whether you can also manage it (can_manage), and your " +
			"sections. Favorites first, then filed apps, then the rest, a page at a time: pass " +
			"`next_cursor` back as `cursor` for the next page. A different list from " +
			"pando_list_apps, which is the apps you can administer.",
		Schema: schema(map[string]any{
			"q":      str("Only apps whose name or slug contains this."),
			"cursor": str("The next_cursor from a previous page."),
		}),
		request: func(args map[string]any) (string, string, any, error) {
			return pagedGet("/me/apps", args, "q", "cursor")
		},
	},
	{
		Name:        "pando_create_section",
		Description: "Make a section in your own launcher: a named grouping of apps. Only you see it.",
		Schema:      schema(map[string]any{"name": str("The section's name.")}, "name"),
		request: func(args map[string]any) (string, string, any, error) {
			name, err := stringArg(args, "name", true)
			if err != nil {
				return "", "", nil, err
			}
			return "POST", "/me/sections", map[string]any{"name": name}, nil
		},
	},
	{
		Name:        "pando_list_user_apps",
		Description: "The apps an account has access to: its role for managing each, directly or through a group, whether it can use each, and whether you can change that (can_manage). By app name, a page at a time: pass `next_cursor` back as `cursor` for the next page.",
		Schema: schema(map[string]any{
			"user_id": str("The account's ID."),
			"cursor":  str("The next_cursor from a previous page."),
		}, "user_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "user_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return pagedGet("/users/"+url.PathEscape(id)+"/apps", args, "cursor")
		},
	},
	{
		Name:        "pando_rename_section",
		Description: "Rename one of your launcher sections.",
		Schema: schema(map[string]any{
			"section_id": str("The section's ID."),
			"name":       str("The new name."),
		}, "section_id", "name"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "section_id", true)
			if err != nil {
				return "", "", nil, err
			}
			name, err := stringArg(args, "name", true)
			if err != nil {
				return "", "", nil, err
			}
			return "PATCH", "/me/sections/" + url.PathEscape(id), map[string]any{"name": name}, nil
		},
	},
	{
		Name:        "pando_delete_section",
		Description: "Delete one of your launcher sections. Its apps go back to Your apps; nothing else changes.",
		Schema:      schema(map[string]any{"section_id": str("The section's ID.")}, "section_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "section_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return "DELETE", "/me/sections/" + url.PathEscape(id), nil, nil
		},
	},
	{
		Name:        "pando_add_app_to_section",
		Description: "Move an app you can open into one of your launcher sections, out of any other.",
		Schema: schema(map[string]any{
			"section_id": str("The section's ID."),
			"app_id":     str("The app's ID."),
		}, "section_id", "app_id"),
		request: func(args map[string]any) (string, string, any, error) {
			sid, err := stringArg(args, "section_id", true)
			if err != nil {
				return "", "", nil, err
			}
			aid, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return "PUT", "/me/sections/" + url.PathEscape(sid) + "/apps/" + url.PathEscape(aid), nil, nil
		},
	},
	{
		Name:        "pando_remove_app_from_section",
		Description: "Move an app out of one of your launcher sections, back to Your apps.",
		Schema: schema(map[string]any{
			"section_id": str("The section's ID."),
			"app_id":     str("The app's ID."),
		}, "section_id", "app_id"),
		request: func(args map[string]any) (string, string, any, error) {
			sid, err := stringArg(args, "section_id", true)
			if err != nil {
				return "", "", nil, err
			}
			aid, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return "DELETE", "/me/sections/" + url.PathEscape(sid) + "/apps/" + url.PathEscape(aid), nil, nil
		},
	},
	{
		Name: "pando_list_audit",
		Description: "Read the audit log, newest first. Every filter is optional and they combine: " +
			"what was done (an action prefix such as app. or grant.delete), who did it, which app, " +
			"what it was done to, and when (RFC 3339 times; since inclusive, until exclusive).",
		Schema: schema(map[string]any{
			"action":         str("Actions starting with this, e.g. app."),
			"principal_id":   str("Who did it: a user or token ID, or system, reconciler or detection."),
			"principal_kind": str("What kind of actor: user, token, system or anonymous."),
			"app_id":         str("Events on this app."),
			"target_kind":    str("What kind of thing it was done to, e.g. user, role, app."),
			"target_id":      str("The ID of the thing it was done to."),
			"involving":      str("Events where this ID is the actor or the target: everything to do with one account."),
			"since":          str("From this time, RFC 3339."),
			"until":          str("Up to this time, RFC 3339."),
			"before":         str("The next_before from a previous page, to read further back."),
		}),
		request: func(args map[string]any) (string, string, any, error) {
			q := url.Values{}
			for _, key := range []string{"action", "principal_id", "principal_kind", "app_id", "target_kind", "target_id", "involving", "since", "until", "before"} {
				v, err := stringArg(args, key, false)
				if err != nil {
					return "", "", nil, err
				}
				if v != "" {
					q.Set(key, v)
				}
			}
			path := "/audit"
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			return "GET", path, nil, nil
		},
	},
	{
		Name: "pando_list_audit_archives",
		Description: "The months of the audit log past retention, archived and removed from the live " +
			"log: each month's row count, first and last event, time range, size, SHA-256 digest, " +
			"and where it is kept. Events in these months are not in pando_list_audit; the archive " +
			"itself is a gzip download from GET /api/v1/audit/archives/{id}, or `pando audit " +
			"archives download`.",
		Schema: schema(map[string]any{}),
		request: func(map[string]any) (string, string, any, error) {
			return "GET", "/audit/archives", nil, nil
		},
	},
	// The stream a page at a time (R-381, design 12 §4). There is no export
	// tool, for the reason there is no archive download (design 04, 12 §7):
	// a gzip of the log is nothing an agent's context can use, and this
	// serves the same events.
	{
		Name: "pando_read_audit_stream",
		Description: "Read the audit log in commit order, oldest first, one page at a time. Returns events, " +
			"cursor and caught_up. Pass the cursor back as after to read on from where this page ended; " +
			"keep it to ask later what has happened since. Without after it starts at the oldest event in " +
			"the live log; after: \"now\" starts after the newest. caught_up says there is nothing past the " +
			"cursor yet. Delivery is at least once: an event can come back again, and its id is the key to " +
			"drop it by. Needs install.audit.read.",
		Schema: schema(map[string]any{
			"after": str("The cursor a previous page returned, or now. Optional."),
			"limit": map[string]any{
				"type":        "integer",
				"description": "Optional. How many events, 1 to 1000. Defaults to 500.",
				"minimum":     1,
				"maximum":     1000,
			},
			"action":  strList("Optional. Only actions starting with one of these prefixes, such as grant."),
			"exclude": strList("Optional. Leave out actions starting with one of these prefixes."),
			"format":  str("Optional. native (the archive's line format, the default) or ocsf."),
		}),
		request: func(args map[string]any) (string, string, any, error) {
			q := url.Values{}
			for _, key := range []string{"after", "format"} {
				v, err := stringArg(args, key, false)
				if err != nil {
					return "", "", nil, err
				}
				if v != "" {
					q.Set(key, v)
				}
			}
			if raw, ok := args["limit"]; ok && raw != nil {
				n, isNumber := raw.(float64)
				if !isNumber || n < 1 || n > 1000 || n != float64(int(n)) {
					return "", "", nil, fmt.Errorf("limit must be a whole number of events, 1 to 1000")
				}
				q.Set("limit", fmt.Sprint(int(n)))
			}
			for _, key := range []string{"action", "exclude"} {
				list, err := stringListArg(args, key)
				if err != nil {
					return "", "", nil, err
				}
				for _, v := range list {
					q.Add(key, v)
				}
			}
			path := "/audit/stream"
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			return "GET", path, nil, nil
		},
	},
	{
		Name: "pando_list_audit_sinks",
		Description: "The audit sinks: destinations, such as a SIEM's syslog or HTTPS collector, that the " +
			"audit log is pushed to as it is written. Each with what it sends where (transport, endpoint, " +
			"format, actions, exclude, and disclosure, the sentence saying what leaves the installation), " +
			"its last delivery, backlog, last_error and failing_since, why Pando turned it off if it did, " +
			"and gap_from and gap_to for a range of events it missed. Needs install.audit.read.",
		Schema: schema(map[string]any{}),
		request: func(map[string]any) (string, string, any, error) {
			return "GET", "/audit/sinks", nil, nil
		},
	},
	{
		Name: "pando_get_updates",
		Description: "Whether a newer Pando is released: the version the server runs, the latest on " +
			"the update channel, and each version in between with its changelog, security fixes " +
			"and breaking changes marked, plus the command that upgrades the server. The check is " +
			"host policy (disable_update_check, update_channel).",
		Schema: schema(map[string]any{}),
		request: func(map[string]any) (string, string, any, error) {
			return "GET", "/updates", nil, nil
		},
	},
	{
		Name: "pando_plan_upgrade",
		Description: "Whether Pando can upgrade itself in place to a version, and if not, every reason " +
			"with what to change; which versions in between may break something, with their upgrade " +
			"notes. Starting an upgrade is not offered here: it takes a backup passphrase, and agents " +
			"do not hold install.upgrade by default.",
		Schema: schema(map[string]any{"version": str("The version to upgrade to, such as 0.4.0, from pando_get_updates.")}, "version"),
		request: func(args map[string]any) (string, string, any, error) {
			v, err := stringArg(args, "version", true)
			if err != nil {
				return "", "", nil, err
			}
			return "GET", "/upgrade?version=" + url.QueryEscape(v), nil, nil
		},
	},
	{
		Name: "pando_get_last_upgrade",
		Description: "The most recent in-place upgrade of Pando: from and to which version, whether it " +
			"succeeded or was rolled back and why, with the new version's last log lines when it was.",
		Schema: schema(map[string]any{}),
		request: func(map[string]any) (string, string, any, error) {
			return "GET", "/upgrade/last", nil, nil
		},
	},
	{
		Name: "pando_list_adapters",
		Description: "The adapters this installation is configured with — runtimes, routing, builders, " +
			"image registries, secrets, backup, scanners, AI, source connections and the rest — each " +
			"with its live capabilities, whether it is reachable, which credentials are set (never " +
			"their values), and whether it waits on a restart. One declared at startup is read-only.",
		Schema: schema(map[string]any{}),
		request: func(map[string]any) (string, string, any, error) {
			return "GET", "/adapters", nil, nil
		},
	},
	{
		Name: "pando_list_adapter_kinds",
		Description: "The kinds of adapter this build of Pando can run, and the settings each takes: " +
			"which are credentials, which are required, and the default each takes when left out.",
		Schema: schema(map[string]any{}),
		request: func(map[string]any) (string, string, any, error) {
			return "GET", "/adapters/kinds", nil, nil
		},
	},
	{
		Name: "pando_configure_adapter",
		Description: "Add an adapter, or change one by its id. Settings go in config; a secret such " +
			"as an API key or a registry password goes in credentials, which is stored encrypted " +
			"and never shown again. A source connection or an image registry is used from the " +
			"moment it is saved; any other adapter after Pando restarts. enabled: false turns one off.",
		Schema: schema(map[string]any{
			"id":          str("The adapter's id, such as reg_main. An existing id changes that adapter."),
			"category":    str("The category, such as image_registry, as pando_list_adapter_kinds lists it."),
			"kind":        str("The kind within the category, such as oci or ecr."),
			"name":        str("A name for people to read."),
			"config":      map[string]any{"type": "object", "description": "The kind's settings, by key."},
			"credentials": map[string]any{"type": "object", "description": "The kind's secret settings, by key. A key sent empty removes that credential."},
			"is_default":  map[string]any{"type": "boolean", "description": "Make it the default in its category."},
			"enabled":     map[string]any{"type": "boolean", "description": "false turns it off. Left out, it is on."},
		}, "id", "category", "kind"),
		request: func(args map[string]any) (string, string, any, error) {
			body := map[string]any{}
			for _, k := range []string{"id", "category", "kind", "name", "config", "credentials", "is_default", "enabled"} {
				if v, ok := args[k]; ok {
					body[k] = v
				}
			}
			return "POST", "/adapters", body, nil
		},
	},
	{
		Name: "pando_get_config",
		Description: "The configuration the Pando server started with: every non-secret setting, " +
			"its value and where it was set (an environment variable, the config file, or the " +
			"default), and the host policy fields fixed there, which cannot be changed through " +
			"the API while they are set.",
		Schema: schema(map[string]any{}),
		request: func(map[string]any) (string, string, any, error) {
			return "GET", "/config", nil, nil
		},
	},
	{
		Name: "pando_get_status",
		Description: "What an app is doing right now: running, degraded, failed, and why — " +
			"including each part separately, so a single part that is crash-looping is " +
			"visible rather than averaged into one word for the app.",
		Schema: schema(map[string]any{"app_id": str("The app's ID.")}, "app_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return "GET", appPath(id, "/status"), nil, nil
		},
	},
	{
		Name: "pando_get_usage",
		Description: "What each part of an app is using right now: CPU (thousandths of a core), " +
			"memory and disk in bytes, and each mounted volume's size, beside its limits " +
			"(0 means none). A reading, not a history.",
		Schema: schema(map[string]any{"app_id": str("The app's ID.")}, "app_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return "GET", appPath(id, "/usage"), nil, nil
		},
	},

	// The AI functions (R-259, R-343 … R-346). Each draft proposes and
	// changes nothing, so none of them is the mutation O-12 keeps out of this
	// list; applying a draft is a separate call the agent may not have.
	{
		Name: "pando_list_ai_functions",
		Description: "Each AI function Pando has, the AI adapter that handles it and on which model, " +
			"whether it is on, and whether the startup configuration assigns it.",
		Schema: schema(map[string]any{}),
		request: func(map[string]any) (string, string, any, error) {
			return "GET", "/ai/functions", nil, nil
		},
	},
	{
		Name: "pando_assign_ai_function",
		Description: "Have an AI adapter handle one AI function, optionally on a model of its own. " +
			"Refused while another adapter handles the function; unassign it there first.",
		Schema: schema(map[string]any{
			"function":   str("The AI function, such as search_audit. pando_list_ai_functions lists them."),
			"adapter_id": str("The AI adapter's ID, such as ai_anthropic."),
			"model":      str("A model for this function only. Omit to use the adapter's own."),
		}, "function", "adapter_id"),
		request: func(args map[string]any) (string, string, any, error) {
			fn, err := stringArg(args, "function", true)
			if err != nil {
				return "", "", nil, err
			}
			adapter, err := stringArg(args, "adapter_id", true)
			if err != nil {
				return "", "", nil, err
			}
			model, err := stringArg(args, "model", false)
			if err != nil {
				return "", "", nil, err
			}
			return "PUT", "/ai/functions/" + url.PathEscape(fn), map[string]string{"adapter_id": adapter, "model": model}, nil
		},
	},
	{
		Name:        "pando_unassign_ai_function",
		Description: "Turn an AI function off by removing the adapter that handles it.",
		Schema:      schema(map[string]any{"function": str("The AI function, such as search_audit.")}, "function"),
		request: func(args map[string]any) (string, string, any, error) {
			fn, err := stringArg(args, "function", true)
			if err != nil {
				return "", "", nil, err
			}
			return "DELETE", "/ai/functions/" + url.PathEscape(fn), nil, nil
		},
	},
	{
		Name: "pando_ai_draft_access",
		Description: "Draft a custom role and a group from a description of who should be able to do " +
			"what. A draft only: nothing is created. Verbs come from Pando's catalog; anything else " +
			"is listed as refused.",
		Schema: schema(map[string]any{"description": str("Who should be able to do what, in a sentence or two.")}, "description"),
		request: func(args map[string]any) (string, string, any, error) {
			d, err := stringArg(args, "description", true)
			if err != nil {
				return "", "", nil, err
			}
			return "POST", "/ai/access/draft", map[string]string{"description": d}, nil
		},
	},
	{
		Name: "pando_ai_draft_host_rules",
		Description: "Propose changes to the installation's host policy from a description: the " +
			"document as it would be saved, and each change. Nothing is saved. Settings fixed in " +
			"the startup configuration are declined, naming where they are set.",
		Schema: schema(map[string]any{"description": str("What the rules should be, in a sentence or two.")}, "description"),
		request: func(args map[string]any) (string, string, any, error) {
			d, err := stringArg(args, "description", true)
			if err != nil {
				return "", "", nil, err
			}
			return "POST", "/ai/policy/draft", map[string]string{"description": d}, nil
		},
	},
	{
		Name: "pando_ai_search_audit",
		Description: "Ask a question of the audit log, such as \"which apps did Ben Meeker create last " +
			"month?\". Returns a short summary and the filters used, which pando_list_audit takes as-is.",
		Schema: schema(map[string]any{"question": str("The question.")}, "question"),
		request: func(args map[string]any) (string, string, any, error) {
			q, err := stringArg(args, "question", true)
			if err != nil {
				return "", "", nil, err
			}
			return "POST", "/ai/audit/search", map[string]string{"question": q}, nil
		},
	},
	{
		Name: "pando_ai_ask_reference",
		Description: "Ask how to do something with Pando. Answered from Pando's API, CLI and MCP " +
			"reference, citing the endpoints, commands and tools it relies on. Describes; does nothing.",
		Schema: schema(map[string]any{"question": str("What you want to do.")}, "question"),
		request: func(args map[string]any) (string, string, any, error) {
			q, err := stringArg(args, "question", true)
			if err != nil {
				return "", "", nil, err
			}
			return "POST", "/ai/reference/answer", map[string]string{"question": q}, nil
		},
	},

	// Event subscriptions (issue #50). Rotating a webhook's signing key is
	// not here: it returns a key, and a tool that hands out a credential is
	// one O-12 keeps from agents. The console and CLI rotate one.
	{
		Name:        "pando_list_events",
		Description: "List the events a subscription can name: each event's name, whether it is about one app or the whole installation, and its data fields.",
		Schema:      schema(map[string]any{}),
		request: func(map[string]any) (string, string, any, error) {
			return "GET", "/events", nil, nil
		},
	},
	{
		Name:        "pando_list_subscriptions",
		Description: "List your event subscriptions: what each listens for, where it sends, and whether it is on. app_id narrows to one app. Newest first, a page at a time: pass `next_cursor` back as `cursor` for the next page.",
		Schema: schema(map[string]any{
			"app_id": str("Only subscriptions about this app. Optional."),
			"cursor": str("The next_cursor from a previous page."),
		}),
		request: func(args map[string]any) (string, string, any, error) {
			return pagedGet("/subscriptions", args, "app_id", "cursor")
		},
	},
	{
		Name: "pando_create_subscription",
		Description: "Subscribe to events and send them to a webhook (url) or through a notification adapter such as Slack or email (adapter_id). " +
			"events is a comma-separated list of event names, prefixes such as deploy.*, or *; pando_list_events lists them. " +
			"With app_id it is about one app; without, it is install-wide and needs install.events.manage. " +
			"A webhook's signing key is in the result once; tell the person to keep it.",
		Schema: schema(map[string]any{
			"events":           str("Comma-separated event names or patterns, such as deploy.failed,app.state_changed."),
			"app_id":           str("The app. Omit for install-wide."),
			"url":              str("The webhook URL to post to. Give this or adapter_id."),
			"adapter_id":       str("A notification adapter's ID. Give this or url."),
			"description":      str("What the subscription is for. Optional."),
			"method":           str("A webhook's HTTP method: POST, PUT or PATCH. Optional."),
			"content_type":     str("A webhook's content type. Optional; application/json by default."),
			"headers":          str("A webhook's own headers, one \"Name: value\" per line, such as an Authorization its receiver needs. Optional."),
			"payload_template": str("A webhook's body as a Go template, such as {\"text\": {{json .Subject}}}. Optional; Pando's envelope by default."),
		}, "events"),
		request: func(args map[string]any) (string, string, any, error) {
			names, err := stringArg(args, "events", true)
			if err != nil {
				return "", "", nil, err
			}
			app, _ := stringArg(args, "app_id", false)
			hook, _ := stringArg(args, "url", false)
			adapter, _ := stringArg(args, "adapter_id", false)
			desc, _ := stringArg(args, "description", false)
			if (hook == "") == (adapter == "") {
				return "", "", nil, fmt.Errorf("give exactly one of url (a webhook) or adapter_id (a notification adapter)")
			}
			body := map[string]any{"events": splitList(names), "app_id": app, "description": desc}
			if hook != "" {
				body["destination"], body["url"] = "webhook", hook
			} else {
				body["destination"], body["adapter_id"] = "notify", adapter
			}
			if err := requestOptions(args, body); err != nil {
				return "", "", nil, err
			}
			return "POST", "/subscriptions", body, nil
		},
	},
	{
		Name:        "pando_update_subscription",
		Description: "Change a subscription's events, url, adapter_id or description, or turn it on or off with enabled (true or false).",
		Schema: schema(map[string]any{
			"subscription_id":  str("The subscription's ID, sub_…."),
			"events":           str("Comma-separated event names or patterns. Optional."),
			"url":              str("A new webhook URL. Optional."),
			"adapter_id":       str("A new notification adapter. Optional."),
			"description":      str("A new description. Optional."),
			"enabled":          str("true or false. Optional."),
			"method":           str("A webhook's HTTP method: POST, PUT or PATCH. Optional."),
			"content_type":     str("A webhook's content type. Optional."),
			"headers":          str("Replace a webhook's headers: one \"Name: value\" per line. Optional."),
			"payload_template": str("A webhook's body as a Go template. Optional."),
		}, "subscription_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "subscription_id", true)
			if err != nil {
				return "", "", nil, err
			}
			body := map[string]any{}
			for _, k := range []string{"url", "adapter_id", "description"} {
				if v, _ := stringArg(args, k, false); v != "" {
					body[k] = v
				}
			}
			if v, _ := stringArg(args, "events", false); v != "" {
				body["events"] = splitList(v)
			}
			switch v, _ := stringArg(args, "enabled", false); v {
			case "true":
				body["enabled"] = true
			case "false":
				body["enabled"] = false
			case "":
			default:
				return "", "", nil, fmt.Errorf("enabled is true or false, not %q", v)
			}
			if err := requestOptions(args, body); err != nil {
				return "", "", nil, err
			}
			return "PATCH", "/subscriptions/" + url.PathEscape(id), body, nil
		},
	},
	{
		Name:        "pando_delete_subscription",
		Description: "Delete a subscription, its signing key and its delivery log.",
		Schema:      schema(map[string]any{"subscription_id": str("The subscription's ID.")}, "subscription_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "subscription_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return "DELETE", "/subscriptions/" + url.PathEscape(id), nil, nil
		},
	},
	{
		Name:        "pando_test_subscription",
		Description: "Send a test event to one subscription, whatever its filter says, to check its endpoint. Returns the delivery; read it with pando_get_delivery.",
		Schema:      schema(map[string]any{"subscription_id": str("The subscription's ID.")}, "subscription_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "subscription_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return "POST", "/subscriptions/" + url.PathEscape(id) + "/test", map[string]any{}, nil
		},
	},
	{
		Name:        "pando_list_deliveries",
		Description: "A subscription's recent deliveries, newest first: each event, whether it arrived, how many attempts it took, and the last error.",
		Schema:      schema(map[string]any{"subscription_id": str("The subscription's ID.")}, "subscription_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "subscription_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return "GET", "/subscriptions/" + url.PathEscape(id) + "/deliveries", nil, nil
		},
	},
	{
		Name:        "pando_get_delivery",
		Description: "One delivery: every attempt at it, with the status code and error each got, and the payload sent.",
		Schema: schema(map[string]any{
			"subscription_id": str("The subscription's ID."),
			"delivery_id":     str("The delivery's ID, dlv_…."),
		}, "subscription_id", "delivery_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "subscription_id", true)
			if err != nil {
				return "", "", nil, err
			}
			dlv, err := stringArg(args, "delivery_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return "GET", "/subscriptions/" + url.PathEscape(id) + "/deliveries/" + url.PathEscape(dlv), nil, nil
		},
	},
	{
		Name:        "pando_redeliver",
		Description: "Send a delivery again now, with the whole retry schedule ahead of it.",
		Schema: schema(map[string]any{
			"subscription_id": str("The subscription's ID."),
			"delivery_id":     str("The delivery's ID."),
		}, "subscription_id", "delivery_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "subscription_id", true)
			if err != nil {
				return "", "", nil, err
			}
			dlv, err := stringArg(args, "delivery_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return "POST", "/subscriptions/" + url.PathEscape(id) + "/deliveries/" + url.PathEscape(dlv) + "/redeliver", map[string]any{}, nil
		},
	},
	{
		Name:        "pando_list_app_events",
		Description: "An app's recent events, newest first: deploys, state and health changes, scans, shares, backups. Each says what happened in a sentence.",
		Schema:      schema(map[string]any{"app_id": str("The app's ID.")}, "app_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return "GET", appPath(id, "/events"), nil, nil
		},
	},
	{
		Name:        "pando_list_notifications",
		Description: "Your notifications inbox, newest first, and how many are unread. unread=true lists only those.",
		Schema:      schema(map[string]any{"unread": str("true to list only unread notifications. Optional.")}),
		request: func(args map[string]any) (string, string, any, error) {
			if v, _ := stringArg(args, "unread", false); v == "true" {
				return "GET", "/me/notifications?unread=true", nil, nil
			}
			return "GET", "/me/notifications", nil, nil
		},
	},
	{
		Name:        "pando_count_unread_notifications",
		Description: "How many of your notifications are unread, without listing them.",
		Schema:      schema(map[string]any{}),
		request: func(map[string]any) (string, string, any, error) {
			return "GET", "/me/notifications/unread", nil, nil
		},
	},
	{
		Name:        "pando_mark_notifications_read",
		Description: "Mark one of your notifications read, by notification_id, or every one when it is omitted.",
		Schema:      schema(map[string]any{"notification_id": str("The notification's ID, ntf_…. Omit to mark every one read.")}),
		request: func(args map[string]any) (string, string, any, error) {
			id, _ := stringArg(args, "notification_id", false)
			if id == "" {
				return "POST", "/me/notifications/read", map[string]any{}, nil
			}
			return "POST", "/me/notifications/" + url.PathEscape(id) + "/read", map[string]any{}, nil
		},
	},
	{
		Name:        "pando_get_notification_preferences",
		Description: "Which of Pando's own notifications reach you, on each channel that reaches people, such as the console and email.",
		Schema:      schema(map[string]any{}),
		request: func(map[string]any) (string, string, any, error) {
			return "GET", "/notification-preferences", nil, nil
		},
	},
	{
		Name:        "pando_set_notification_preference",
		Description: "Turn one of Pando's notifications on or off for you on one channel. kind and channel are as pando_get_notification_preferences lists them.",
		Schema: schema(map[string]any{
			"kind":    str("The notification, such as app_shared."),
			"channel": str("The channel's adapter ID, such as ntf_console."),
			"enabled": str("true or false."),
		}, "kind", "channel", "enabled"),
		request: func(args map[string]any) (string, string, any, error) {
			kind, err := stringArg(args, "kind", true)
			if err != nil {
				return "", "", nil, err
			}
			channel, err := stringArg(args, "channel", true)
			if err != nil {
				return "", "", nil, err
			}
			enabled, err := stringArg(args, "enabled", true)
			if err != nil {
				return "", "", nil, err
			}
			if enabled != "true" && enabled != "false" {
				return "", "", nil, fmt.Errorf("enabled is true or false, not %q", enabled)
			}
			return "PUT", "/notification-preferences", map[string]any{"choices": []map[string]any{
				{"kind": kind, "channel": channel, "enabled": enabled == "true"},
			}}, nil
		},
	},
}

// requestOptions adds a webhook's request options to a body (R-375).
func requestOptions(args map[string]any, body map[string]any) error {
	for arg, field := range map[string]string{"method": "method", "content_type": "content_type", "payload_template": "payload_template"} {
		if v, _ := stringArg(args, arg, false); v != "" {
			body[field] = v
		}
	}
	raw, _ := stringArg(args, "headers", false)
	if raw == "" {
		return nil
	}
	headers := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) == "" {
			return fmt.Errorf("%q is not a header; give one \"Name: value\" per line", line)
		}
		headers[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}
	body["headers"] = headers
	return nil
}

// splitList reads a comma-separated argument.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

var toolsByName = func() map[string]tool {
	m := make(map[string]tool, len(toolList))
	for _, t := range toolList {
		m[t.Name] = t
	}
	return m
}()
