package config

import (
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

// TestR271_EverySettingIsDescribed asserts R-271 for docs/config.json: every
// setting Pando reads at startup is in the reference with a description, the
// variable that sets it and the default Load starts from — and nothing Load
// knows of is missing from it.
func TestR271_EverySettingIsDescribed(t *testing.T) {
	docs := Reference()
	require.NotEmpty(t, docs)

	byKey := map[string]SettingDoc{}
	for _, d := range docs {
		require.NotEmpty(t, d.Description, "%s has no desc tag: say what it does, for docs/config.json", d.Key)
		require.NotEmpty(t, d.Env, d.Key)
		require.NotEmpty(t, d.Group, d.Key)
		require.NotEmpty(t, d.Type, d.Key)
		byKey[d.Key] = d
	}

	v := viper.New()
	setDefaults(v)
	for _, key := range v.AllKeys() {
		require.Contains(t, byKey, key, "%s has a default but is not a field of Config", key)
	}
	for key := range boundEnv {
		require.Contains(t, byKey, key, "%s is bound to a variable but is not a field of Config", key)
	}

	require.Equal(t, SettingDoc{
		Env: "PANDO_SERVER_ADDR", Key: "server.addr", Type: "string", Default: ":8080",
		Description: byKey["server.addr"].Description, Group: "server",
	}, byKey["server.addr"])
	require.Equal(t, "PANDO_ADMIN_PASSWORD", byKey["bootstrap.admin_password"].Env)
	require.True(t, byKey["bootstrap.admin_password"].Secret)
	require.Equal(t, "duration", byKey["server.shutdown_timeout"].Type)
	require.Equal(t, "15s", byKey["server.shutdown_timeout"].Default)
	require.Equal(t, "PANDO_WORK_DETECTIONS", byKey["work.detections"].Env)
}
