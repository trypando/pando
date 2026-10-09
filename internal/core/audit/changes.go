package audit

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
)

// Changes describes what changed between two versions of a document, for an
// audit event's detail (R-390): "changed" lists the paths of the fields that
// differ, and "before" and "after" hold their values where a value is a
// scalar. A list or an object that changed is named and not copied: the path
// is what an investigation searches for, and copying whole structures would
// make one policy edit the largest row in the log.
//
// Both sides are read through their JSON encoding, so a secret.Value — which
// encodes as "[redacted]" — is the same on both sides and its change is never
// recorded as a value (R-194). A field holding a secret's value changes as a
// path alone at most.
//
// Nil when nothing changed or either side cannot be encoded.
func Changes(before, after any) map[string]any {
	b, okB := flatten(before)
	a, okA := flatten(after)
	if !okB || !okA {
		return nil
	}
	paths := map[string]bool{}
	for k := range b {
		paths[k] = true
	}
	for k := range a {
		paths[k] = true
	}
	var changed []string
	was, now := map[string]any{}, map[string]any{}
	for p := range paths {
		bv, inB := b[p]
		av, inA := a[p]
		if inB && inA && reflect.DeepEqual(bv, av) {
			continue
		}
		changed = append(changed, p)
		if inB && scalar(bv) {
			was[p] = bv
		}
		if inA && scalar(av) {
			now[p] = av
		}
	}
	if len(changed) == 0 {
		return nil
	}
	sort.Strings(changed)
	out := map[string]any{"changed": changed}
	if len(was) > 0 {
		out["before"] = was
	}
	if len(now) > 0 {
		out["after"] = now
	}
	return out
}

// maxDepth bounds how far a document is flattened: deeper than this, a
// changed object is named by its path and not walked.
const maxDepth = 4

func flatten(v any) (map[string]any, bool) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, false
	}
	out := map[string]any{}
	walk("", root, 0, out)
	return out, true
}

func walk(prefix string, v any, depth int, out map[string]any) {
	m, ok := v.(map[string]any)
	if !ok || depth >= maxDepth {
		if prefix != "" {
			out[prefix] = v
		}
		return
	}
	if len(m) == 0 && prefix != "" {
		out[prefix] = v
		return
	}
	for k, child := range m {
		p := k
		if prefix != "" {
			p = fmt.Sprintf("%s.%s", prefix, k)
		}
		walk(p, child, depth+1, out)
	}
}

func scalar(v any) bool {
	switch v.(type) {
	case nil, bool, float64, string:
		return true
	}
	return false
}
