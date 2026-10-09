package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func settingNamed(t *testing.T, cfg *Config, key string) Setting {
	t.Helper()
	for _, s := range cfg.Settings {
		if s.Key == key {
			return s
		}
	}
	t.Fatalf("no setting %q in %v", key, cfg.Settings)
	return Setting{}
}

// R-271: configuration comes from a file, the environment or a default, and
// Pando says which — so the console can tell an operator where to change it.
func TestR271_EverySettingSaysWhereItCameFrom(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pando.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
server:
  routing_mode: port
log:
  level: debug
policy:
  min_security_score: 70
  disabled_verbs: [app.exec]
  allow_anonymous_grants: false
`), 0o600))

	t.Setenv("PANDO_DATABASE_URL", "postgres://pando:secret@db/pando")
	t.Setenv("PANDO_SERVER_BASE_DOMAIN", "example.test")
	t.Setenv("PANDO_LOG_LEVEL", "warn") // the environment beats the file
	t.Setenv("PANDO_POLICY_MIN_SECURITY_SCORE", "80")
	t.Setenv("PANDO_POLICY_MAX_TOKEN_LIFETIME_DAYS", "") // empty is unset
	t.Setenv("PANDO_ADMIN_PASSWORD", "hunter2hunter2")

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, path, cfg.File)

	require.Equal(t, Source{Kind: "file", Name: path, Key: "server.routing_mode"}, settingNamed(t, cfg, "server.routing_mode").Source)
	require.Equal(t, Source{Kind: "env", Name: "PANDO_SERVER_BASE_DOMAIN"}, settingNamed(t, cfg, "server.base_domain").Source)
	require.Equal(t, "warn", settingNamed(t, cfg, "log.level").Value)
	require.Equal(t, Source{Kind: "env", Name: "PANDO_LOG_LEVEL"}, settingNamed(t, cfg, "log.level").Source)
	require.Equal(t, Source{Kind: "default"}, settingNamed(t, cfg, "server.work_dir").Source)
	require.Equal(t, "PANDO_SERVER_WORK_DIR", settingNamed(t, cfg, "server.work_dir").Env, "a default still says how to set it")
	require.Equal(t, "15s", settingNamed(t, cfg, "server.shutdown_timeout").Value, "durations read as written")

	// Secrets are never listed (R-194).
	for _, s := range cfg.Settings {
		require.NotEqual(t, "database.url", s.Key)
		require.NotEqual(t, "bootstrap.admin_password", s.Key)
	}

	policy := map[string]PolicySetting{}
	for _, p := range cfg.Policy {
		policy[p.Key] = p
	}
	require.Len(t, policy, 3, "an empty variable sets nothing")
	require.Equal(t, "80", policy["min_security_score"].Value, "the environment beats the file")
	require.Equal(t, Source{Kind: "env", Name: "PANDO_POLICY_MIN_SECURITY_SCORE"}, policy["min_security_score"].Source)
	require.Equal(t, Source{Kind: "file", Name: path, Key: "policy.disabled_verbs"}, policy["disabled_verbs"].Source)
	require.Equal(t, false, policy["allow_anonymous_grants"].Value)
}

func policyNamed(cfg *Config, key string) (PolicySetting, bool) {
	for _, p := range cfg.Policy {
		if p.Key == key {
			return p, true
		}
	}
	return PolicySetting{}, false
}

// TestR271_WorkDeploysIsTheOlderNameOfAPolicySetting asserts R-271 for a
// setting that moved: work.deploys was a startup setting and is now host
// policy's max_concurrent_deploys (issue #93). An install that set it keeps
// it, fixed and saying where it was set; the policy key wins when both are.
func TestR271_WorkDeploysIsTheOlderNameOfAPolicySetting(t *testing.T) {
	t.Setenv("PANDO_DATABASE_URL", "postgres://pando@db/pando")
	t.Setenv("PANDO_WORK_DEPLOYS", "6")
	cfg, err := Load("")
	require.NoError(t, err)
	got, ok := policyNamed(cfg, "max_concurrent_deploys")
	require.True(t, ok, "the older setting fixes the policy field")
	require.Equal(t, 6, got.Value)
	require.Equal(t, Source{Kind: "env", Name: "PANDO_WORK_DEPLOYS"}, got.Source)

	t.Setenv("PANDO_POLICY_MAX_CONCURRENT_DEPLOYS", "3")
	cfg, err = Load("")
	require.NoError(t, err)
	got, _ = policyNamed(cfg, "max_concurrent_deploys")
	require.Equal(t, "3", got.Value, "the policy key wins over the older name")

	t.Setenv("PANDO_POLICY_MAX_CONCURRENT_DEPLOYS", "")
	t.Setenv("PANDO_WORK_DEPLOYS", "")
	cfg, err = Load("")
	require.NoError(t, err)
	_, ok = policyNamed(cfg, "max_concurrent_deploys")
	require.False(t, ok, "left alone, it is the console's to set")
}
