package aikit

import (
	"github.com/trypando/pando/internal/adapter/api"
)

// itemSchema is what one submitted amendment may look like for fn.
//
// A repair gets the whole closed set. Answering questions gets a schema of its
// own: one kind, and a key that must be one of the questions actually asked,
// with every field required. Core refuses anything else from that call
// regardless (R-336), but the answer schema used to be the general one, where
// "key" is optional because most kinds have none — and a model answered the
// build_strategy question correctly with no key at all, so the answer was
// refused and the person was asked anyway.
//
// Answering also fills the values the deploy waits on (api.ScreenRequest.
// Values): a second shape, set_env, whose key must be one of those. Each shape
// is offered only when it has keys, so an empty enum never reaches the API.
func itemSchema(fn api.AIFunction, questions []api.Question, values []string) map[string]any {
	if fn != api.AIFunctionAnswerQuestions {
		return amendmentSchema([]string{
			"set_command", "set_env", "set_port", "set_health", "add_slot",
			"set_build_context", "set_dockerfile", "set_static_dir",
			"add_volume", "answer_question", "add_warning",
		})
	}

	keys := make([]string, 0, len(questions))
	for _, q := range questions {
		keys = append(keys, q.Key)
	}
	var shapes []any
	if len(keys) > 0 {
		shapes = append(shapes, keyedItem(api.AmendAnswerQuestion, keys,
			"The key of the question this answers, exactly as listed.",
			"The answer, as a person would type it into the question's field: one of its valid answers when it lists them.",
			"Why the repository answers the question this way"))
	}
	if len(values) > 0 {
		shapes = append(shapes, keyedItem(api.AmendSetEnv, values,
			"The variable this fills, exactly as listed.",
			"The value, exactly as the app should read it. Never a secret, key, token or password you made up.",
			"Why the repository, or the address the plan gives the app, settles this value"))
	}
	if len(shapes) == 1 {
		return shapes[0].(map[string]any)
	}
	return map[string]any{"anyOf": shapes}
}

// keyedItem is one strict amendment shape: a fixed kind, a key from a list,
// and every field required.
func keyedItem(kind api.AmendmentKind, keys []string, keyHelp, valueHelp, reasonHelp string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"kind", "key", "value", "reason", "evidence"},
		"properties": map[string]any{
			"kind": map[string]any{
				"type": "string",
				"enum": []string{string(kind)},
			},
			"key": map[string]any{
				"type":        "string",
				"enum":        keys,
				"description": keyHelp,
			},
			"value": map[string]any{
				"type":        "string",
				"description": valueHelp,
			},
			"reason": map[string]any{
				"type": "string",
				"description": reasonHelp + ", written so that someone who cannot see the repository " +
					"understands it. One or two sentences.",
			},
			"evidence": map[string]any{
				"type":     "array",
				"items":    map[string]any{"type": "string"},
				"minItems": 1,
				"description": "The repository paths you read that settle this, such as " +
					"\"docker-compose.yml\". Each is checked to exist; an empty path is not evidence.",
			},
		},
	}
}

// amendmentSchema is the closed set, as JSON Schema.
//
// It mirrors api.Amendment, and the mirroring is the point: a kind that is not
// in this enum cannot be asked for, which is R-332 arriving one layer earlier
// than core's validation rather than instead of it.
func amendmentSchema(kinds []string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"kind", "reason", "evidence"},
		"properties": map[string]any{
			"kind": map[string]any{
				"type":        "string",
				"enum":        kinds,
				"description": "Which change this is.",
			},
			"workload": map[string]any{
				"type": "string",
				"description": "Which workload this applies to. Leave empty for the primary one, " +
					"which is almost always correct.",
			},
			"key": map[string]any{
				"type": "string",
				"description": "An environment variable name for set_env, a dependency key for " +
					"add_slot, or the question key for answer_question.",
			},
			"value": map[string]any{
				"type": "string",
				"description": "The value: the variable's value for set_env, the answer for " +
					"answer_question, the message for add_warning, or a shell command line for " +
					"set_command when command is not given.",
			},
			"command": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "The command as an argv, for set_command.",
			},
			"path": map[string]any{
				"type": "string",
				"description": "A path in the repository for set_build_context, set_dockerfile " +
					"and set_static_dir; the health check path for set_health; the absolute " +
					"container path for add_volume.",
			},
			"port": map[string]any{
				"type":        "integer",
				"description": "A port number, for set_port and set_health.",
			},
			"slot_type": map[string]any{
				"type":        "string",
				"enum":        []string{"postgres", "mysql", "redis", "mongodb", "s3", "smtp", "unknown"},
				"description": "What kind of dependency, for add_slot.",
			},
			"required": map[string]any{
				"type": "boolean",
				"description": "Whether the app cannot start without this dependency. Only " +
					"honored when the trial run actually crashed; otherwise the dependency is " +
					"recorded as optional so it cannot block a deploy.",
			},
			"reason": map[string]any{
				"type": "string",
				"description": "Why this change is needed, written so that someone who cannot see " +
					"the repository can understand it. No apology, no error prefix. One or two " +
					"sentences.",
			},
			"evidence": map[string]any{
				"type":     "array",
				"items":    map[string]any{"type": "string"},
				"minItems": 1,
				"description": "The repository paths this rests on. These are checked to exist; " +
					"an amendment citing a file that is not in the repository is refused. Cite " +
					"only files you actually read.",
			},
		},
	}
}
