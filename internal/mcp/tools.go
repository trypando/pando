package mcp

import (
	"encoding/base64"
	"fmt"
	"maps"
	"net/url"
	"slices"
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

// twoArgs reads two required string arguments, for a tool that names a thing
// and something under it.
func twoArgs(args map[string]any, first, second string) (string, string, error) {
	a, err := stringArg(args, first, true)
	if err != nil {
		return "", "", err
	}
	b, err := stringArg(args, second, true)
	return a, b, err
}

// appPath builds a path under an app, escaping the ID.
//
// Escaped even though app IDs are Pando's own prefixed ULIDs: the ID here comes
// from an agent, which means it comes from a model, which means it can be
// anything at all.
func appPath(id, suffix string) string {
	return "/apps/" + url.PathEscape(id) + suffix
}

// specPath builds a path under one of an app's spec revisions, escaping both.
func specPath(id, rev, suffix string) string {
	return appPath(id, "/specs/"+url.PathEscape(rev)+suffix)
}

// appIDDescription describes the app_id every app tool takes.
const appIDDescription = "The app's ID."

// accountIDArg describes a user_id argument, and usersPrefix is the path the
// account tools address one under.
const (
	accountIDArg = "The account's ID."
	usersPrefix  = "/users/"
)

// newNameArg describes the name argument of a tool that renames something.
const newNameArg = "The new name."

// appIDSchema is the schema of a tool that takes only an app.
func appIDSchema() map[string]any {
	return schema(map[string]any{"app_id": str(appIDDescription)}, "app_id")
}

// noticeSchema names one of an app's notices (R-412).
func noticeSchema() map[string]any {
	return schema(map[string]any{
		"app_id": str(appIDDescription),
		"key":    str("The notice's key, as pando_list_notices lists it, such as variables.unset."),
	}, "app_id", "key")
}

// appRequest is the request of a tool that takes only an app and calls one
// endpoint under it.
func appRequest(method, suffix string) func(args map[string]any) (string, string, any, error) {
	return func(args map[string]any) (string, string, any, error) {
		id, err := stringArg(args, "app_id", true)
		if err != nil {
			return "", "", nil, err
		}
		return method, appPath(id, suffix), nil, nil
	}
}

// decisionRequest is approving or rejecting a deploy waiting for approval
// (R-154): POST .../deployments/{id}/<action>, with an optional comment.
func decisionRequest(action string) func(args map[string]any) (string, string, any, error) {
	return func(args map[string]any) (string, string, any, error) {
		appID, depID, err := twoArgs(args, "app_id", "deployment_id")
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

// getByID reads one object, at prefix followed by the ID the key names.
func getByID(args map[string]any, key, prefix string) (string, string, any, error) {
	id, err := stringArg(args, key, true)
	if err != nil {
		return "", "", nil, err
	}
	return "GET", prefix + url.PathEscape(id), nil, nil
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

// acceptValues turns a name-to-value object into the variables POST
// /detection/accept takes, marked secret or not.
func acceptValues(args map[string]any, key string, secret bool) ([]map[string]any, error) {
	values := []map[string]any{}
	v, ok := args[key]
	if !ok || v == nil {
		return values, nil
	}
	raw, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object of names to values", key)
	}
	// In name order, so the same call sends the same body.
	for _, name := range slices.Sorted(maps.Keys(raw)) {
		value, isString := raw[name].(string)
		if !isString {
			return nil, fmt.Errorf("%s.%s must be a string", key, name)
		}
		entry := map[string]any{"key": name, "value": value}
		if secret {
			entry["secret"] = true
		}
		values = append(values, entry)
	}
	return values, nil
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
			"Each app has its `address`, its owner's name (`owner_name`), and `access`, who can open it: " +
			"`private` (its owner alone), `shared` (its owner and the people or groups it is shared " +
			"with), `public` (anyone on the internet, without signing in) or `passcode` (anyone on " +
			"the internet who has the passcode). " +
			"`notice_count` is how many of its notices you have not ignored (pando_list_notices " +
			"lists them), left out when none. " +
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
		Schema:  appIDSchema(),
		request: appRequest("GET", ""),
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
			id, repo, err := twoArgs(args, "source_id", "url")
			if err != nil {
				return "", "", nil, err
			}
			return "GET", "/sources/" + url.PathEscape(id) + "/branches?url=" + url.QueryEscape(repo), nil, nil
		},
	},
	{
		Name: "pando_authorize_source",
		Description: "Start signing a source connection in. By device code (the default) it returns user_code " +
			"and verification_url: tell the person to open the URL and enter the code, then call " +
			"pando_poll_source_authorization every interval_seconds until it says authorized. A connection " +
			"whose capabilities have web_authorization and not device_authorization — a GitHub App, which " +
			"Pando creates on GitHub and then installs — takes mode web: give the person authorize_url to open " +
			"in a browser signed in to Pando, and check pando_list_sources afterwards.",
		Schema: schema(map[string]any{
			"source_id": str("The connection's id, from pando_list_sources."),
			"mode":      str("device (the default) or web."),
		}, "source_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "source_id", true)
			if err != nil {
				return "", "", nil, err
			}
			mode, err := stringArg(args, "mode", false)
			if err != nil {
				return "", "", nil, err
			}
			if mode == "" {
				mode = "device"
			}
			return "POST", "/sources/" + url.PathEscape(id) + "/authorize", map[string]string{"mode": mode}, nil
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
			"repository), detecting (working out what the app is), building (building the image " +
			"the app would run, so the plan can show its security score), trying (a trial run), " +
			"scanning (a security scan) or screening (an AI adapter checking the plan); `running` " +
			"lists every step under way, since the scan runs beside the build and the trial; and " +
			"`elapsed_seconds` says how long it has taken so far; a build or a trial run can take " +
			"several minutes. Pass wait_seconds (up to 60) to have the call return as soon as the stage " +
			"changes or detection finishes, instead of calling repeatedly. When it has " +
			"finished, `status` is ready (call pando_accept_proposal), needs_answers or unknown " +
			"(answer the questions with pando_answer_detection), blocked or failed (the " +
			"`detection` field says why).",
		Schema: schema(map[string]any{
			"app_id": str(appIDDescription),
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
		Name: "pando_answer_detection",
		Description: "Answer one of the questions from pando_get_detection. An answer that changes what is " +
			"built (a build argument) has Pando build and scan the image again, so the plan shows the score " +
			"of what will run: detection reads as `running` until it has, and accepting waits for it.",
		Schema: schema(map[string]any{
			"app_id": str(appIDDescription),
			"key":    str("The question's key."),
			"answer": str("The answer."),
		}, "app_id", "key", "answer"),
		request: func(args map[string]any) (string, string, any, error) {
			id, key, err := twoArgs(args, "app_id", "key")
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
			"environment variables and secrets in the same step. This does not deploy — call pando_deploy after. " +
			"On an app that is already configured it is refused unless `confirm` is true: accepting again " +
			"replaces how the app is built and what it runs with what detection found, and carries over " +
			"variables, how each dependency is filled, and storage. pando_get_detection_diff shows what would change. " +
			"To change only a variable on a configured app, write a spec revision with pando_create_spec instead.",
		Schema: schema(map[string]any{
			"app_id": str(appIDDescription),
			"values": map[string]any{
				"type":                 "object",
				"description":          "Environment variables to set, name to value, e.g. {\"API_URL\": \"https://api\"}. Stored as ordinary variables, not secrets.",
				"additionalProperties": map[string]any{"type": "string"},
			},
			"secrets": map[string]any{
				"type":                 "object",
				"description":          "Secret variables to set, name to value. Stored by the secrets adapter; needs app.secrets.write.",
				"additionalProperties": map[string]any{"type": "string"},
			},
			"optional": strList("Dependencies the app runs without, by their keys."),
			"hostname": str("The address to serve the app at, where its routing gives it a hostname of its own."),
			"confirm":  map[string]any{"type": "boolean", "description": "true to accept over an app that is already configured."},
		}, "app_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			values, err := acceptValues(args, "values", false)
			if err != nil {
				return "", "", nil, err
			}
			secrets, err := acceptValues(args, "secrets", true)
			if err != nil {
				return "", "", nil, err
			}
			body := map[string]any{"values": append(values, secrets...)}
			optional, err := stringListArg(args, "optional")
			if err != nil {
				return "", "", nil, err
			}
			if len(optional) > 0 {
				body["optional"] = optional
			}
			hostname, err := stringArg(args, "hostname", false)
			if err != nil {
				return "", "", nil, err
			}
			if hostname != "" {
				body["hostname"] = hostname
			}
			if confirm, _ := args["confirm"].(bool); confirm {
				body["confirm"] = true
			}
			return "POST", appPath(id, "/detection/accept"), body, nil
		},
	},
	{
		Name: "pando_list_specs",
		Description: "List an app's spec revisions, and which one is pinned. The pinned revision is what " +
			"runs and what the next deploy ships.",
		Schema:  appIDSchema(),
		request: appRequest("GET", "/specs"),
	},
	{
		Name:        "pando_get_spec",
		Description: "Read one spec revision in full: how the app is built, what it runs, its variables, egress and routing.",
		Schema: schema(map[string]any{
			"app_id":   str(appIDDescription),
			"revision": str("The revision number, such as \"3\", or the revision's ID."),
		}, "app_id", "revision"),
		request: func(args map[string]any) (string, string, any, error) {
			id, rev, err := twoArgs(args, "app_id", "revision")
			if err != nil {
				return "", "", nil, err
			}
			return "GET", specPath(id, rev, ""), nil, nil
		},
	},
	{
		Name: "pando_create_spec",
		Description: "Write a new spec revision: the way to change a configured app's variables, egress, " +
			"workloads or anything else in its spec. Send the whole spec — read the pinned one with " +
			"pando_get_spec, change it, and send it back. Revisions are append-only, and this neither pins " +
			"nor deploys: call pando_pin_spec with the revision it returns, then pando_deploy. With " +
			"`dry_run` true nothing is written; the reply says whether the spec is accepted, the egress " +
			"rules it would run with, and whether deploying it would need approval.",
		Schema: schema(map[string]any{
			"app_id":  str(appIDDescription),
			"spec":    map[string]any{"type": "object", "description": "The whole spec, as pando_get_spec returns it under `body`."},
			"dry_run": map[string]any{"type": "boolean", "description": "true to check the spec without writing it."},
		}, "app_id", "spec"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			body, ok := args["spec"].(map[string]any)
			if !ok {
				return "", "", nil, fmt.Errorf("spec is required, as an object")
			}
			path := appPath(id, "/specs")
			if dry, _ := args["dry_run"].(bool); dry {
				path += "?dry_run=true"
			}
			return "POST", path, body, nil
		},
	},
	{
		Name: "pando_pin_spec",
		Description: "Pin a spec revision: what the app converges to, and what the next deploy ships. " +
			"This does not deploy — call pando_deploy after. On an app that has been deployed, pinning " +
			"changes what it runs, so it needs app.deploy, and a revision whose deploy needs approval " +
			"is refused: deploy it instead.",
		Schema: schema(map[string]any{
			"app_id":   str(appIDDescription),
			"revision": str("The revision number, such as \"3\", or the revision's ID."),
		}, "app_id", "revision"),
		request: func(args map[string]any) (string, string, any, error) {
			id, rev, err := twoArgs(args, "app_id", "revision")
			if err != nil {
				return "", "", nil, err
			}
			return "POST", specPath(id, rev, "/pin"), map[string]any{}, nil
		},
	},
	{
		Name: "pando_diff_specs",
		Description: "Compare two spec revisions: what changed, and whether deploying it restarts, " +
			"rebuilds or leaves the app alone.",
		Schema: schema(map[string]any{
			"app_id": str(appIDDescription),
			"from":   str("The earlier revision, by number or ID."),
			"to":     str("The later revision, by number or ID."),
		}, "app_id", "from", "to"),
		request: func(args map[string]any) (string, string, any, error) {
			id, from, err := twoArgs(args, "app_id", "from")
			if err != nil {
				return "", "", nil, err
			}
			to, err := stringArg(args, "to", true)
			if err != nil {
				return "", "", nil, err
			}
			return "GET", specPath(id, from, "/diff/"+url.PathEscape(to)), nil, nil
		},
	},
	{
		Name:        "pando_list_secrets",
		Description: "List which secrets an app has, and where each came from. Never their values.",
		Schema:      appIDSchema(),
		request:     appRequest("GET", "/secrets"),
	},
	{
		Name: "pando_set_secret",
		Description: "Set or rotate one of an app's secrets. Needs app.secrets.write. The app gets it only " +
			"through a `secret_ref` in its spec: when the reply's `read_by_spec` is false, add " +
			"{\"key\": ..., \"secret_ref\": ...} to a workload's env with pando_create_spec. Deploying " +
			"after rotating one the app reads recreates its workload with the new value.",
		Schema: schema(map[string]any{
			"app_id": str(appIDDescription),
			"key":    str("The secret's name, such as NTFY_TOPIC."),
			"value":  str("The value."),
		}, "app_id", "key", "value"),
		request: func(args map[string]any) (string, string, any, error) {
			id, key, err := twoArgs(args, "app_id", "key")
			if err != nil {
				return "", "", nil, err
			}
			value, err := stringArg(args, "value", true)
			if err != nil {
				return "", "", nil, err
			}
			return "PUT", appPath(id, "/secrets/"+url.PathEscape(key)), map[string]any{"value": value}, nil
		},
	},
	{
		Name:        "pando_delete_secret",
		Description: "Remove one of an app's secrets. Needs app.secrets.write.",
		Schema: schema(map[string]any{
			"app_id": str(appIDDescription),
			"key":    str("The secret's name."),
		}, "app_id", "key"),
		request: func(args map[string]any) (string, string, any, error) {
			id, key, err := twoArgs(args, "app_id", "key")
			if err != nil {
				return "", "", nil, err
			}
			return "DELETE", appPath(id, "/secrets/"+url.PathEscape(key)), nil, nil
		},
	},
	{
		Name: "pando_plan",
		Description: "Show what a deploy would do, without doing it. Side-effect free, so it is " +
			"safe to call after any change to check the change is deployable.",
		Schema: appIDSchema(),
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
			"Without `spec_revision` it ships the pinned revision; an edit, from the console or pando_create_spec, " +
			"writes a newer one, and the reply's `newer_revision` names it — deploy again with that revision to ship the edit. " +
			"When the deploy needs somebody's approval, it comes back with status `awaiting_approval` and " +
			"`approval_reasons` saying why; it runs once a person approves it.",
		Schema: schema(map[string]any{
			"app_id": str(appIDDescription),
			"spec_revision": map[string]any{
				"type":        "integer",
				"description": "The revision to deploy, by number, such as the newest from pando_list_specs. Left out, the pinned one.",
			},
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
			if v, ok := args["spec_revision"]; ok && v != nil {
				n, isNumber := v.(float64)
				if !isNumber || n < 1 || n != float64(int(n)) {
					return "", "", nil, fmt.Errorf("spec_revision must be a revision number, such as 3")
				}
				body["spec_revision"] = int(n)
			}
			return "POST", appPath(id, "/deployments"), body, nil
		},
	},
	{
		Name: "pando_check_app_image",
		Description: "For an app deployed from a published image: whether its tag, such as :latest, now names a " +
			"different build than the one the app runs. Writes nothing. When `update_available` is true, " +
			"pando_deploy_newer_image with `latest_digest` deploys that build.",
		Schema:  appIDSchema(),
		request: appRequest("GET", "/image-update"),
	},
	{
		Name: "pando_deploy_newer_image",
		Description: "Deploy the newer build pando_check_app_image found: writes a revision pinning `digest` and deploys it. " +
			"Refused if the tag has moved since the check; check again then. Answers as pando_deploy does, approval included.",
		Schema: schema(map[string]any{
			"app_id": str(appIDDescription),
			"digest": str("The latest_digest from pando_check_app_image."),
		}, "app_id", "digest"),
		request: func(args map[string]any) (string, string, any, error) {
			id, digest, err := twoArgs(args, "app_id", "digest")
			if err != nil {
				return "", "", nil, err
			}
			return "POST", appPath(id, "/image-update"), map[string]any{"digest": digest}, nil
		},
	},
	{
		Name: "pando_list_approvals",
		Description: "List the deploys waiting for approval on every app you can see. Each says which app, " +
			"why it needs approval, the approvals it has so far, and `can_decide`: whether you may approve or reject it. " +
			"Oldest first, a page at a time: pass `next_cursor` back as `cursor` for the next page.",
		Schema: schema(map[string]any{
			"cursor":    str("The next_cursor from a previous page."),
			"decidable": map[string]any{"type": "boolean", "description": "true to list only the deploys you may approve or reject."},
		}),
		request: func(args map[string]any) (string, string, any, error) {
			method, path, body, err := pagedGet("/approvals", args, "cursor")
			if err != nil {
				return "", "", nil, err
			}
			if decidable, _ := args["decidable"].(bool); decidable {
				if strings.Contains(path, "?") {
					path += "&decidable=true"
				} else {
					path += "?decidable=true"
				}
			}
			return method, path, body, nil
		},
	},
	{
		Name: "pando_approve_deploy",
		Description: "Approve a deploy that is waiting for approval. When it is the last approval the deploy " +
			"needs, the deploy starts. Approval is a person's sign-off, so by default an installation does not " +
			"let an agent's token approve; the refusal says so.",
		Schema: schema(map[string]any{
			"app_id":        str(appIDDescription),
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
			"app_id":        str(appIDDescription),
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
			"app_id":   str(appIDDescription),
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
		Schema: appIDSchema(),
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
		Schema:      appIDSchema(),
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
		Schema: appIDSchema(),
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
			"app_id":       str(appIDDescription),
			"image_base64": str("The image file's bytes, base64-encoded."),
		}, "app_id", "image_base64"),
		request: func(args map[string]any) (string, string, any, error) {
			id, encoded, err := twoArgs(args, "app_id", "image_base64")
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
		Schema:      appIDSchema(),
		request:     appRequest("DELETE", "/icon"),
	},
	{
		Name: "pando_upload_source",
		Description: "Send an app's files as its source: a gzipped tar of the app's directory, " +
			"base64-encoded, at most 256 MB compressed. Paths in the archive are relative to the " +
			"app's root; leave out .git, node_modules and build output. A single index.html is " +
			"enough for a static site. Replaces any files sent before. Then call " +
			"pando_rerun_detection so Pando works out how to run them.",
		Schema: schema(map[string]any{
			"app_id":         str(appIDDescription),
			"archive_base64": str("The gzipped tar's bytes, base64-encoded."),
		}, "app_id", "archive_base64"),
		request: func(args map[string]any) (string, string, any, error) {
			id, encoded, err := twoArgs(args, "app_id", "archive_base64")
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
			"credential for a private image. Nothing changes until the new proposal is accepted. " +
			"Then call pando_get_detection with wait_seconds.",
		Schema:  appIDSchema(),
		request: appRequest("POST", "/detection/rerun"),
	},
	{
		Name: "pando_revise_detection",
		Description: "Tell the install's AI adapter what is wrong with the plan from pando_get_detection, " +
			"in plain words, e.g. \"The app serves on port 8080.\" It checks the repository, changes what " +
			"it can show, and replies; the reply, what it changed and what Pando refused are the last " +
			"entry of `detection.conversation`. Needs an AI adapter.",
		Schema: schema(map[string]any{
			"app_id":  str(appIDDescription),
			"message": str("What is wrong with the plan."),
		}, "app_id", "message"),
		request: func(args map[string]any) (string, string, any, error) {
			id, message, err := twoArgs(args, "app_id", "message")
			if err != nil {
				return "", "", nil, err
			}
			return "POST", appPath(id, "/detection/revise"), map[string]any{"message": message}, nil
		},
	},
	{
		Name: "pando_get_detection_diff",
		Description: "What accepting the proposal would change about the app's pinned configuration: " +
			"each change with its class (benign, restart, rebuild or destructive) and a summary. " +
			"`is_first` is true when the app is not configured yet, so everything is new.",
		Schema:  appIDSchema(),
		request: appRequest("GET", "/detection/diff"),
	},
	{
		Name: "pando_set_registry_credential",
		Description: "Set the credential an image app's private image is pulled with, replacing " +
			"any it had. It belongs to the app and is never given to it. Pando never shows it " +
			"again; pando_get_registry_credential says only which kind is set.",
		Schema: schema(map[string]any{
			"app_id":              str(appIDDescription),
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
		Schema:      appIDSchema(),
		request:     appRequest("GET", "/registry-credential"),
	},
	{
		Name:        "pando_remove_registry_credential",
		Description: "Remove an image app's registry credential, so its image is pulled anonymously.",
		Schema:      appIDSchema(),
		request:     appRequest("DELETE", "/registry-credential"),
	},
	{
		Name: "pando_get_auto_deploy",
		Description: "Whether an app deploys automatically when its repository changes, and how: " +
			"`settings` on the newest configuration and `deployed` on the running one, with `pending` " +
			"when they differ; `paused` when approval now stops it; `last_check`, what Pando last found " +
			"and the last commit it tried (each commit is tried once) and why it went nowhere; and the " +
			"webhook URL, with whether a webhook secret is set.",
		Schema:  appIDSchema(),
		request: appRequest("GET", "/auto-deploy"),
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
			"app_id":      str(appIDDescription),
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
		Schema: appIDSchema(),
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
		Schema:      appIDSchema(),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return "DELETE", "/me/favorites/" + url.PathEscape(id), nil, nil
		},
	},
	{
		Name: "pando_list_notices",
		Description: "What an app's Overview asks of somebody: a configuration not yet deployed, " +
			"variables with no value, a backup that was not taken, a security score below this " +
			"installation's minimum, the spec's warnings. Each has a stable key, a message, a fingerprint " +
			"and whether you have ignored it as it reads now.",
		Schema:  appIDSchema(),
		request: appRequest("GET", "/notices"),
	},
	{
		Name: "pando_ignore_notice",
		Description: "Hide one of an app's notices from yourself until what it says changes. " +
			"Only you: everyone else still sees it. It hides a reminder and nothing more — a deploy " +
			"the notice warned about is still refused.",
		Schema: noticeSchema(),
		request: func(args map[string]any) (string, string, any, error) {
			id, key, err := twoArgs(args, "app_id", "key")
			if err != nil {
				return "", "", nil, err
			}
			return "PUT", appPath(id, "/notices/"+url.PathEscape(key)+"/ignore"), nil, nil
		},
	},
	{
		Name:        "pando_restore_notice",
		Description: "Show one of an app's notices you ignored again.",
		Schema:      noticeSchema(),
		request: func(args map[string]any) (string, string, any, error) {
			id, key, err := twoArgs(args, "app_id", "key")
			if err != nil {
				return "", "", nil, err
			}
			return "DELETE", appPath(id, "/notices/"+url.PathEscape(key)+"/ignore"), nil, nil
		},
	},
	{
		Name:        "pando_rename_app",
		Description: "Change an app's display name. Its ID and address do not change.",
		Schema: schema(map[string]any{
			"app_id": str(appIDDescription),
			"name":   str(newNameArg),
		}, "app_id", "name"),
		request: func(args map[string]any) (string, string, any, error) {
			id, name, err := twoArgs(args, "app_id", "name")
			if err != nil {
				return "", "", nil, err
			}
			return "PATCH", appPath(id, ""), map[string]any{"name": name}, nil
		},
	},
	{
		Name: "pando_get_address",
		Description: "Where an app is reached: its routing (adapter, mode, hostname, path or port) and " +
			"address, next_address when a change is saved and not yet deployed, and the routing adapters it " +
			"could move to — each with the modes it serves, its default mode, base_domain, and domains: every " +
			"domain a hostname can be in, such as each of a Cloudflare adapter's zones.",
		Schema:  appIDSchema(),
		request: appRequest("GET", "/routing"),
	},
	{
		Name: "pando_set_address",
		Description: "Change where a configured app is reached. Everything left out keeps what the app has, " +
			"or takes the adapter's default when moving to another adapter. To put the app in another of the " +
			"adapter's domains (from pando_get_address), give domain: alone it makes <slug>.<domain>, and with " +
			"a hostname that is a single name such as docs it makes docs.<domain>. This writes a spec revision " +
			"and does not deploy; the next deploy moves the app, and the old address stops working then. " +
			"Without confirm: true it changes nothing and the error says what would move where — tell the " +
			"person before confirming. A mode the adapter does not default to needs app.routing.override.",
		Schema: schema(map[string]any{
			"app_id":      str(appIDDescription),
			"adapter_ref": str("The routing adapter, such as rte_cloudflare. Optional; the app's current one."),
			"mode":        map[string]any{"type": "string", "enum": []string{"subdomain", "path", "port"}, "description": "How the app is addressed. Optional."},
			"hostname":    str("With subdomain mode: the hostname, such as notes.example.com, or with domain a single name such as notes. Optional."),
			"domain":      str("With subdomain mode: which of the adapter's domains the hostname is in, such as example.org. Optional; the adapter's base domain."),
			"path_prefix": str("With path mode: the path, such as /team/notes. Optional."),
			"confirm":     map[string]any{"type": "boolean", "description": "true to make the change. Left out, nothing changes and the reply says what would."},
		}, "app_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			body := map[string]any{}
			for _, key := range []string{"adapter_ref", "mode", "hostname", "domain", "path_prefix"} {
				v, err := stringArg(args, key, false)
				if err != nil {
					return "", "", nil, err
				}
				if v != "" {
					body[key] = v
				}
			}
			confirm, _ := args["confirm"].(bool)
			body["confirm"] = confirm
			return "PUT", appPath(id, "/routing"), body, nil
		},
	},
	{
		Name: "pando_get_app_idle",
		Description: "When Pando stops or deletes an app nobody uses: the app's own " +
			"stop_days and delete_days (null is the installation's, 0 is never), the installation's, the ones " +
			"in force, last_activity_at, stops_at and deletes_at if nobody uses it before then, and " +
			"stopped_for_idle when Pando stopped it.",
		Schema:  appIDSchema(),
		request: appRequest("GET", "/idle"),
	},
	{
		Name: "pando_set_app_idle",
		Description: "Set when Pando stops and deletes an app nobody uses. Both are replaced, so pass " +
			"what pando_get_app_idle returned for the one you are not changing. Each is a number of days, " +
			"\"never\", or \"default\" for the installation's setting. Both count from the last use, so " +
			"deleting has to come later than stopping. App settings, not spec: nothing is deployed.",
		Schema: schema(map[string]any{
			"app_id":      str(appIDDescription),
			"stop_days":   numberOrWord("Days without use before Pando stops the app, \"never\", or \"default\".", "never", "default"),
			"delete_days": numberOrWord("Days without use before Pando deletes the app, \"never\", or \"default\".", "never", "default"),
		}, "app_id", "stop_days", "delete_days"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "app_id", true)
			if err != nil {
				return "", "", nil, err
			}
			stop, err := idleDaysArg(args, "stop_days")
			if err != nil {
				return "", "", nil, err
			}
			del, err := idleDaysArg(args, "delete_days")
			if err != nil {
				return "", "", nil, err
			}
			return "PUT", appPath(id, "/idle"), map[string]any{"stop_days": stop, "delete_days": del}, nil
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
		Name: "pando_list_users",
		Description: "The accounts on this installation, newest first, a page at a time: pass `next_cursor` " +
			"back as `cursor` for the next page. Each carries adapter_name — the identity provider it signs in with — and install_roles — every installation role it holds, " +
			"and whether it holds it directly or through a group (with the group's name, and the identity " +
			"provider for a synced group) — and install_role_id, the role granted to the account itself. " +
			"Needs install.view.",
		Schema: schema(map[string]any{
			"q":      str("Only accounts whose username, display name or email contains this."),
			"cursor": str("The next_cursor from a previous page."),
		}),
		request: func(args map[string]any) (string, string, any, error) {
			return pagedGet("/users", args, "q", "cursor")
		},
	},
	{
		Name: "pando_get_user",
		Description: "One account, with every installation role it holds and where each comes from " +
			"(install_roles), as pando_list_users shows it. Your own needs no permission; anyone else's needs " +
			"install.view.",
		Schema: schema(map[string]any{"user_id": str(accountIDArg)}, "user_id"),
		request: func(args map[string]any) (string, string, any, error) {
			return getByID(args, "user_id", usersPrefix)
		},
	},
	{
		// R-405. Setting up, replacing or turning off one's own second factor
		// has no tool, and is not missing one: each puts an authenticator key,
		// recovery codes or the account's password into the conversation, which
		// is a transcript somebody keeps. The console and `pando two-factor`
		// do them.
		Name: "pando_get_two_factor",
		Description: "Who you are (GET /me), including, on a local account, two_factor: whether two-factor " +
			"authentication is on, whether host policy requires it, and how many recovery codes are left. Setting " +
			"it up or changing it is done in the console's settings or with `pando two-factor`, never here: each " +
			"step handles an authenticator key, recovery codes or your password.",
		Schema: schema(map[string]any{}),
		request: func(map[string]any) (string, string, any, error) {
			return "GET", "/me", nil, nil
		},
	},
	{
		Name: "pando_reset_user_two_factor",
		Description: "Remove another local account's two-factor authentication — its authenticator and recovery " +
			"codes — for someone who has lost both, and sign it out everywhere. They then sign in with their " +
			"password alone, and set it up again if host policy requires it. Needs install.users.manage and every " +
			"installation permission that account holds.",
		Schema: schema(map[string]any{"user_id": str(accountIDArg)}, "user_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "user_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return "DELETE", usersPrefix + url.PathEscape(id) + "/two-factor", nil, nil
		},
	},
	{
		Name:        "pando_list_user_apps",
		Description: "The apps an account has access to: its role for managing each, directly or through a group, whether it can use each, and whether you can change that (can_manage). By app name, a page at a time: pass `next_cursor` back as `cursor` for the next page.",
		Schema: schema(map[string]any{
			"user_id": str(accountIDArg),
			"cursor":  str("The next_cursor from a previous page."),
		}, "user_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "user_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return pagedGet(usersPrefix+url.PathEscape(id)+"/apps", args, "cursor")
		},
	},
	{
		Name: "pando_get_app_limit",
		Description: "How many apps a person may own. With user_id: the limit in force (0 is " +
			"unlimited), where it comes from (user, group or policy, with the group's name), how many apps " +
			"they own, and the value on their own account. With group_id: the group's max_apps, null when it " +
			"sets none. Your own account needs no permission; anything else needs install.users.manage.",
		Schema: schema(map[string]any{
			"user_id":  str("An account's ID. Give this or group_id."),
			"group_id": str("A group's ID. Give this or user_id."),
		}),
		request: func(args map[string]any) (string, string, any, error) {
			path, err := limitTarget(args)
			if err != nil {
				return "", "", nil, err
			}
			return "GET", path, nil, nil
		},
	},
	{
		Name: "pando_set_app_limit",
		Description: "Set how many apps an account, or each person in a group, may own. A " +
			"number, 0 for unlimited, or \"clear\" to remove it. An account's own value applies whatever its " +
			"groups say; otherwise the most generous group's, then host policy's max_apps_per_user. Lowering " +
			"it removes nothing. Needs install.users.manage, which host policy keeps from agents unless an " +
			"administrator allows it.",
		Schema: schema(map[string]any{
			"user_id":  str("An account's ID. Give this or group_id."),
			"group_id": str("A group's ID. Give this or user_id."),
			"max_apps": numberOrWord("A number of apps, 0 for unlimited, or \"clear\".", "clear"),
		}, "max_apps"),
		request: func(args map[string]any) (string, string, any, error) {
			path, err := limitTarget(args)
			if err != nil {
				return "", "", nil, err
			}
			n, err := maxAppsArg(args)
			if err != nil {
				return "", "", nil, err
			}
			return "PUT", path, map[string]any{"max_apps": n}, nil
		},
	},
	{
		Name: "pando_rename_group",
		Description: "Rename a group made in Pando. A group synced from an identity provider is renamed there. " +
			"Needs install.users.manage.",
		Schema: schema(map[string]any{
			"group_id": str("The group's ID."),
			"name":     str(newNameArg),
		}, "group_id", "name"),
		request: func(args map[string]any) (string, string, any, error) {
			id, name, err := twoArgs(args, "group_id", "name")
			if err != nil {
				return "", "", nil, err
			}
			return "PATCH", "/groups/" + url.PathEscape(id), map[string]any{"name": name}, nil
		},
	},
	{
		Name: "pando_edit_role",
		Description: "Rename a custom role, replace its permissions, or both. The verbs must be of the role's " +
			"scope; built-in roles cannot be changed. Changing an installation role needs every installation " +
			"permission it carries before and after; permissions added to an app role must be yours on every app " +
			"it is granted on. Needs install.users.manage.",
		Schema: schema(map[string]any{
			"role_id": str("The role's ID."),
			"name":    str("The new name. Leave out to keep it."),
			"verbs":   strList("Every permission the role carries, from the verb catalog. Leave out to keep them."),
		}, "role_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "role_id", true)
			if err != nil {
				return "", "", nil, err
			}
			body := map[string]any{}
			if name, err := stringArg(args, "name", false); err != nil {
				return "", "", nil, err
			} else if name != "" {
				body["name"] = name
			}
			if _, ok := args["verbs"]; ok {
				verbs, err := stringListArg(args, "verbs")
				if err != nil {
					return "", "", nil, err
				}
				body["verbs"] = verbs
			}
			if len(body) == 0 {
				return "", "", nil, fmt.Errorf("give name, verbs, or both")
			}
			return "PATCH", "/roles/" + url.PathEscape(id), body, nil
		},
	},
	{
		Name: "pando_list_role_holders",
		Description: "Who holds a role: the groups granted it, then the accounts holding it, each with via — " +
			"direct, or group for an account holding it only through a group. An app role's holders are those " +
			"holding it on any app. A page at a time: pass `next_cursor` back as `cursor`. Needs install.view.",
		Schema: schema(map[string]any{
			"role_id": str("The role's ID."),
			"cursor":  str("The next_cursor from a previous page."),
		}, "role_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "role_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return pagedGet("/roles/"+url.PathEscape(id)+"/holders", args, "cursor")
		},
	},
	{
		Name:        "pando_rename_section",
		Description: "Rename one of your launcher sections.",
		Schema: schema(map[string]any{
			"section_id": str("The section's ID."),
			"name":       str(newNameArg),
		}, "section_id", "name"),
		request: func(args map[string]any) (string, string, any, error) {
			id, name, err := twoArgs(args, "section_id", "name")
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
			"app_id":     str(appIDDescription),
		}, "section_id", "app_id"),
		request: func(args map[string]any) (string, string, any, error) {
			sid, aid, err := twoArgs(args, "section_id", "app_id")
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
			"app_id":     str(appIDDescription),
		}, "section_id", "app_id"),
		request: func(args map[string]any) (string, string, any, error) {
			sid, aid, err := twoArgs(args, "section_id", "app_id")
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
			"what it was done to, how it ended, text to search for, and when (RFC 3339 times; since " +
			"inclusive, until exclusive). Each event has a summary in plain words, such as " +
			"\"Deployed crewmate\", beside its action.",
		Schema: schema(map[string]any{
			"action":         str("Actions starting with this, e.g. app."),
			"principal_id":   str("Who did it: a user or token ID, or system, reconciler or detection."),
			"principal_kind": str("What kind of actor: user, token, system or anonymous."),
			"app_id":         str("Events on this app."),
			"target_kind":    str("What kind of thing it was done to, e.g. user, role, app."),
			"target_id":      str("The ID of the thing it was done to."),
			"involving":      str("Events where this ID is the actor or the target: everything to do with one account."),
			"outcome":        str("Only events with this outcome: success, denied or failed."),
			"q":              str("Text found in the action, the actor's name, email or ID, the target's ID, or the app's ID or name."),
			"since":          str("From this time, RFC 3339."),
			"until":          str("Up to this time, RFC 3339."),
			"before":         str("The next_before from a previous page, to read further back."),
		}),
		request: func(args map[string]any) (string, string, any, error) {
			q := url.Values{}
			for _, key := range []string{"action", "principal_id", "principal_kind", "app_id", "target_kind", "target_id", "involving", "outcome", "q", "since", "until", "before"} {
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
		Name: "pando_list_hosts",
		Description: "The hosts that have joined this installation's cluster: each one's name, roles " +
			"(control, app), address, version and when it was last heard from. Needs install.cluster.manage.",
		Schema: schema(map[string]any{}),
		request: func(map[string]any) (string, string, any, error) {
			return "GET", "/cluster/hosts", nil, nil
		},
	},
	{
		Name:        "pando_remove_host",
		Description: "Remove a host from the cluster. The installation's first host and the host serving the request cannot be removed. For a host that held the control role, the answer's `notice` and `rotate` name the keys to replace. Needs install.cluster.manage.",
		Schema:      hostIDSchema,
		request:     hostRequest("DELETE", ""),
	},
	{
		Name: "pando_mark_host_lost",
		Description: "Say a host is gone and start the standby of each app homed on it. With three or " +
			"more hosts Pando does this itself when most of them cannot reach the host; with fewer it never does. " +
			"Refused while the host answers. A failed app is not failed over. Returns the apps started and the " +
			"ones skipped, with why. Needs install.cluster.manage.",
		Schema:  hostIDSchema,
		request: hostRequest("POST", "/lost"),
	},
	{
		Name: "pando_list_join_tokens",
		Description: "Every join token, newest first: its roles, how many of its uses are spent, when it " +
			"expires and whether it was revoked. Never the token itself. Needs install.cluster.manage.",
		Schema: schema(map[string]any{}),
		request: func(map[string]any) (string, string, any, error) {
			return "GET", "/cluster/join-tokens", nil, nil
		},
	},
	{
		Name: "pando_create_join_token",
		Description: "Create a token a new Pando joins this installation with, at its setup. `join` in the " +
			"reply is the token and is shown once. A token for the control role needs every install permission.",
		Schema: schema(map[string]any{
			"roles": strList("control, app, or both. Left out: both while the cluster has fewer than three hosts, then app."),
			"max_uses": map[string]any{
				"type": "integer", "minimum": 1, "maximum": 100,
				"description": "How many hosts may join with it. Left out, 1.",
			},
			"expires_hours": map[string]any{
				"type": "integer", "minimum": 1, "maximum": 720,
				"description": "How many hours it lasts. Left out, 24.",
			},
			"address": str("The installation's address the joining host reaches, such as https://pando.example.com. Left out, this server's advertise address."),
		}),
		request: func(args map[string]any) (string, string, any, error) {
			body := map[string]any{}
			roles, err := stringListArg(args, "roles")
			if err != nil {
				return "", "", nil, err
			}
			if len(roles) > 0 {
				body["roles"] = roles
			}
			for _, key := range []string{"max_uses", "expires_hours"} {
				if v, ok := args[key]; ok && v != nil {
					n, isNumber := v.(float64)
					if !isNumber || n < 1 || n != float64(int(n)) {
						return "", "", nil, fmt.Errorf("%s must be a whole number, at least 1", key)
					}
					body[key] = int(n)
				}
			}
			if a, _ := stringArg(args, "address", false); a != "" {
				body["address"] = a
			}
			return "POST", "/cluster/join-tokens", body, nil
		},
	},
	{
		Name:        "pando_revoke_join_token",
		Description: "Revoke a join token, so no host can join with it. Needs install.cluster.manage.",
		Schema:      schema(map[string]any{"token_id": str("The join token's ID, from pando_list_join_tokens.")}, "token_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "token_id", true)
			if err != nil {
				return "", "", nil, err
			}
			return "DELETE", "/cluster/join-tokens/" + url.PathEscape(id), nil, nil
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
		Name: "pando_check_updates",
		Description: "Ask GitHub for Pando's releases now rather than waiting for the six-hourly check, " +
			"and answer as pando_get_updates does. A second request within a minute answers from the " +
			"first. Sends nothing while host policy's disable_update_check is set.",
		Schema: schema(map[string]any{}),
		request: func(map[string]any) (string, string, any, error) {
			return "POST", "/updates/check", nil, nil
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
			"is_default":  map[string]any{"type": "boolean", "description": "Make it the default in its category, in place of the current default. The result's replaced_default names that adapter and handover says what changes; tell the person both."},
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
		Schema:  appIDSchema(),
		request: appRequest("GET", "/status"),
	},
	{
		Name: "pando_get_usage",
		Description: "What each part of an app is using right now: CPU (thousandths of a core), " +
			"memory and disk in bytes, and each mounted volume's size, beside its limits " +
			"(0 means none). A reading, not a history.",
		Schema:  appIDSchema(),
		request: appRequest("GET", "/usage"),
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
			fn, adapter, err := twoArgs(args, "function", "adapter_id")
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
			"channel":          str("The channel it posts to, for an adapter whose destination says channels, such as Slack; pando_list_notify_channels lists them."),
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
				if channel, _ := stringArg(args, "channel", false); channel != "" {
					body["channel"] = channel
				}
			}
			if err := requestOptions(args, body); err != nil {
				return "", "", nil, err
			}
			return "POST", "/subscriptions", body, nil
		},
	},
	{
		Name:        "pando_update_subscription",
		Description: "Change a subscription's events, url, adapter_id, channel or description, or turn it on or off with enabled (true or false).",
		Schema: schema(map[string]any{
			"subscription_id":  str("The subscription's ID, sub_…."),
			"events":           str("Comma-separated event names or patterns. Optional."),
			"url":              str("A new webhook URL. Optional."),
			"adapter_id":       str("A new notification adapter. Optional."),
			"channel":          str("A new channel, for an adapter such as Slack. Optional."),
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
			for _, k := range []string{"url", "adapter_id", "channel", "description"} {
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
		Name: "pando_list_notify_adapters",
		Description: "List the notification adapters, each with whether it can send: capabilities.authorized, and problem saying why one cannot. " +
			"One with web_authorization or manual_authorization that is not authorized, such as Slack, needs pando_connect_notify first.",
		Schema: schema(map[string]any{}),
		request: func(map[string]any) (string, string, any, error) {
			return "GET", "/notify", nil, nil
		},
	},
	{
		Name: "pando_connect_notify",
		Description: "Connect a notification adapter such as Slack to its provider. mode web takes token, a Slack configuration token " +
			"(Slack's app settings, Your App Configuration Tokens, Generate Token) used once and never stored; Pando creates its app " +
			"and returns authorize_url for the person to open in a browser signed in to Pando and choose Allow. Slack returns only to " +
			"an https address. mode manual without token returns authorize_url, which creates the app in Slack by hand; with token, " +
			"the bot token it shows once installed, it finishes. Check pando_list_notify_adapters afterwards.",
		Schema: schema(map[string]any{
			"adapter_id": str("The adapter's ID, from pando_list_notify_adapters."),
			"mode":       str("web (the default) or manual."),
			"token":      str("For web, a Slack configuration token; for manual, the app's bot token, or omit for the link."),
		}, "adapter_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "adapter_id", true)
			if err != nil {
				return "", "", nil, err
			}
			mode, _ := stringArg(args, "mode", false)
			token, _ := stringArg(args, "token", false)
			if mode == "" {
				mode = "web"
			}
			body := map[string]string{"mode": mode}
			if token != "" {
				body["token"] = token
			}
			return "POST", "/notify/" + url.PathEscape(id) + "/authorize", body, nil
		},
	},
	{
		Name: "pando_list_notify_channels",
		Description: "List the channels a subscription to a notification adapter such as Slack can post to: id, name, private. " +
			"A private Slack channel is listed once Pando is invited to it with /invite @Pando.",
		Schema: schema(map[string]any{
			"adapter_id": str("The adapter's ID."),
			"q":          str("Only channels whose names contain this. Optional."),
		}, "adapter_id"),
		request: func(args map[string]any) (string, string, any, error) {
			id, err := stringArg(args, "adapter_id", true)
			if err != nil {
				return "", "", nil, err
			}
			path := "/notify/" + url.PathEscape(id) + "/channels"
			if q, _ := stringArg(args, "q", false); q != "" {
				path += "?q=" + url.QueryEscape(q)
			}
			return "GET", path, nil, nil
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
			id, dlv, err := twoArgs(args, "subscription_id", "delivery_id")
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
			id, dlv, err := twoArgs(args, "subscription_id", "delivery_id")
			if err != nil {
				return "", "", nil, err
			}
			return "POST", "/subscriptions/" + url.PathEscape(id) + "/deliveries/" + url.PathEscape(dlv) + "/redeliver", map[string]any{}, nil
		},
	},
	{
		Name:        "pando_list_app_events",
		Description: "An app's recent events, newest first: deploys, state and health changes, scans, shares, backups. Each says what happened in a sentence.",
		Schema:      appIDSchema(),
		request:     appRequest("GET", "/events"),
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
			kind, channel, err := twoArgs(args, "kind", "channel")
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

// hostIDSchema is the input of a tool about one host.
var hostIDSchema = schema(map[string]any{"host_id": str("The host's ID, from pando_list_hosts.")}, "host_id")

// hostRequest is a tool's request to /cluster/hosts/<host_id><suffix>.
func hostRequest(method, suffix string) func(map[string]any) (string, string, any, error) {
	return func(args map[string]any) (string, string, any, error) {
		id, err := stringArg(args, "host_id", true)
		if err != nil {
			return "", "", nil, err
		}
		return method, "/cluster/hosts/" + url.PathEscape(id) + suffix, nil, nil
	}
}
