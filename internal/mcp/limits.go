package mcp

import (
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
)

// Idle apps and app limits (issue #131), as tools. A model sends a number as
// a JSON number or a string, and "never", "default" and "clear" as words, so
// each argument reads all of them and says which it wanted when it gets
// something else.

// idleDaysArg reads stop_days or delete_days: a number of days, "never"
// (0), or "default" (null, the installation's setting).
func idleDaysArg(args map[string]any, key string) (any, error) {
	v, ok := args[key]
	if !ok {
		return nil, fmt.Errorf("%s is required: a number of days, \"never\", or \"default\" for the installation's setting", key)
	}
	switch s := v.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "default":
			return nil, nil
		case "never":
			return 0, nil
		}
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n >= 0 {
			return n, nil
		}
	case float64:
		if s >= 0 && s == math.Trunc(s) {
			return int(s), nil
		}
	}
	return nil, fmt.Errorf("%s must be a number of days, \"never\", or \"default\"; got %v", key, v)
}

// limitTarget is the path of a user's or a group's app limit: exactly one of
// user_id and group_id.
func limitTarget(args map[string]any) (string, error) {
	user, err := stringArg(args, "user_id", false)
	if err != nil {
		return "", err
	}
	group, err := stringArg(args, "group_id", false)
	if err != nil {
		return "", err
	}
	switch {
	case user != "" && group != "":
		return "", fmt.Errorf("give user_id or group_id, not both")
	case user != "":
		return "/users/" + url.PathEscape(user) + "/app-limit", nil
	case group != "":
		return "/groups/" + url.PathEscape(group) + "/app-limit", nil
	}
	return "", fmt.Errorf("user_id or group_id is required")
}

// maxAppsArg reads max_apps: a number, 0 for unlimited, or "clear" (null).
func maxAppsArg(args map[string]any) (any, error) {
	v, ok := args["max_apps"]
	if !ok {
		return nil, fmt.Errorf("max_apps is required: a number of apps, 0 for unlimited, or \"clear\"")
	}
	switch s := v.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(s), "clear") {
			return nil, nil
		}
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n >= 0 {
			return n, nil
		}
	case float64:
		if s >= 0 && s == math.Trunc(s) {
			return int(s), nil
		}
	}
	return nil, fmt.Errorf("max_apps must be a number of apps, 0 for unlimited, or \"clear\"; got %v", v)
}

func daysOrWord(description string) map[string]any {
	return map[string]any{
		"description": description,
		"anyOf":       []any{map[string]any{"type": "integer", "minimum": 0}, map[string]any{"type": "string"}},
	}
}
