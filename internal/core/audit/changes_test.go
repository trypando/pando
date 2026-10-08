package audit

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/trypando/pando/internal/secret"
)

// TestR390_ChangesNameTheFieldsAndNeverASecretValue asserts R-390: a change
// records the paths that changed, before and after for scalars, and a secret
// value appears on neither side (R-194).
func TestR390_ChangesNameTheFieldsAndNeverASecretValue(t *testing.T) {
	type env struct {
		Token secret.Value `json:"token"`
		Mode  string       `json:"mode"`
	}
	type doc struct {
		Name    string   `json:"name"`
		Verbs   []string `json:"verbs"`
		Env     env      `json:"env"`
		Unmoved int      `json:"unmoved"`
	}
	before := doc{Name: "old", Verbs: []string{"app.exec"}, Env: env{Token: secret.New("hunter2"), Mode: "a"}, Unmoved: 1}
	after := doc{Name: "new", Verbs: []string{"app.exec", "app.deploy"}, Env: env{Token: secret.New("swordfish"), Mode: "b"}, Unmoved: 1}

	got := Changes(before, after)
	assert.Equal(t, []string{"env.mode", "name", "verbs"}, got["changed"],
		"a secret that changed is not visible as a change of value; a list is named, not copied")
	assert.Equal(t, map[string]any{"name": "old", "env.mode": "a"}, got["before"])
	assert.Equal(t, map[string]any{"name": "new", "env.mode": "b"}, got["after"])
	assert.NotContains(t, assertString(got), "hunter2")
	assert.NotContains(t, assertString(got), "swordfish")

	assert.Nil(t, Changes(before, before), "nothing changed, nothing recorded")
}

func assertString(v any) string {
	m, _ := flatten(v)
	s := ""
	for k, x := range m {
		s += k + "=" + toString(x) + ";"
	}
	return s
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
