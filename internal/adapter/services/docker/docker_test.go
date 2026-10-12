package docker_test

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	servicesdocker "github.com/trypando/pando/internal/adapter/services/docker"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/secret"
)

func configured(t *testing.T) *servicesdocker.Adapter {
	t.Helper()
	a := servicesdocker.New()
	require.NoError(t, a.Configure(context.Background(), nil))
	return a
}

func provision(t *testing.T, a *servicesdocker.Adapter, typ spec.SlotType, prior secret.Value) api.ProvisionResult {
	t.Helper()
	res, err := a.Provision(context.Background(), api.ProvisionRequest{
		AppID: "app_01HQ8", BundleID: "app_01HQ8", SlotKey: "DATABASE_URL",
		Type: typ, ServiceID: "svc_01HQ9", ExistingSecret: prior,
	})
	require.NoError(t, err)
	return res
}

// TestR131_ProvisionFillsASlotWithSomethingRunnable asserts R-131.
func TestR131_ProvisionFillsASlotWithSomethingRunnable(t *testing.T) {
	for _, typ := range []spec.SlotType{spec.SlotPostgres, spec.SlotMySQL, spec.SlotRedis} {
		t.Run(string(typ), func(t *testing.T) {
			res := provision(t, configured(t), typ, secret.Value{})

			require.Len(t, res.Workloads, 1)
			require.Len(t, res.Volumes, 1, "R-135: the data has somewhere to live")
			require.NotEmpty(t, res.Workloads[0].Image)
			require.NotNil(t, res.Workloads[0].Health, "the app starts alongside it and connects immediately")

			// The workload mounts the volume it was given, and only that one. A
			// service whose data directory is not on a volume loses everything
			// on the next deploy and looks fine until then.
			require.Len(t, res.Workloads[0].Mounts, 1)
			require.Equal(t, res.Volumes[0].VolumeID, res.Workloads[0].Mounts[0].VolumeID)

			dsn := res.ConnectionSecret.Reveal()
			u, err := url.Parse(dsn)
			require.NoError(t, err, "the connection string is a URL an app can parse")
			require.Equal(t, res.Workloads[0].Name, u.Hostname(),
				"the DSN names the workload, so it resolves on the bundle's private network")
			password, ok := u.User.Password()
			require.True(t, ok)
			require.NotEmpty(t, password)
		})
	}
}

// The environment handed to a service is only what its image expects.
//
// Specifically: no MYSQL_PWD. It looks like the tidy way to keep a password off
// the health check's command line, and it breaks the image's entrypoint
// outright — initialization connects as root with no password, MYSQL_PWD
// overrides that, and the container exits 1 on "Access denied" before the
// database is created. Every first deploy, silently, until someone reads the
// container log.
func TestNoEnvironmentThatBreaksTheImagesOwnStartup(t *testing.T) {
	res := provision(t, configured(t), spec.SlotMySQL, secret.Value{})
	_, set := res.Workloads[0].Env["MYSQL_PWD"]
	require.False(t, set, "MYSQL_PWD breaks the mysql image's initialization")

	// And the health check carries no credential, because it does not need one:
	// mysqladmin ping exits 0 when the server answers at all, which is what
	// ping means.
	for _, arg := range res.Workloads[0].Health.Command {
		require.NotContains(t, arg, "-p", "a password on argv is in the host's process list")
	}
}

// TestR134_AProvisionedServiceIsNotAddressableFromOutside asserts R-134.
func TestR134_AProvisionedServiceIsNotAddressableFromOutside(t *testing.T) {
	res := provision(t, configured(t), spec.SlotPostgres, secret.Value{})

	w := res.Workloads[0]
	require.False(t, w.Exposed, "a provisioned service is never the app's entrypoint")
	require.Empty(t, w.Ports, "nothing is published; it is reachable on the private network only")
}

// TestR131_RedeployingReusesTheCredentialsTheDataWasCreatedWith asserts that a
// redeploy does not lock the app out of its own database.
//
// A database sets its password when its data directory is created and ignores
// the variable forever after. An adapter that generated a fresh one on every
// Provision would produce a deploy that succeeds and an app that cannot log in.
func TestR131_RedeployingReusesTheCredentialsTheDataWasCreatedWith(t *testing.T) {
	a := configured(t)

	first := provision(t, a, spec.SlotPostgres, secret.Value{})
	second := provision(t, a, spec.SlotPostgres, first.ConnectionSecret)

	require.Equal(t, first.ConnectionSecret.Reveal(), second.ConnectionSecret.Reveal())
	require.Equal(t, first.Handle, second.Handle)
	require.Equal(t, first.Volumes[0].VolumeID, second.Volumes[0].VolumeID,
		"the same volume, or the redeploy connects the app to an empty database")
}

// TestR131_AProvisionedPostgresURLUsesTheSchemeEveryClientReads asserts R-131:
// the connection string a provisioned Postgres fills its slot with is one the
// app can use. SQLAlchemy 2 refuses postgres:// (issue #90).
func TestR131_AProvisionedPostgresURLUsesTheSchemeEveryClientReads(t *testing.T) {
	dsn := provision(t, configured(t), spec.SlotPostgres, secret.Value{}).ConnectionSecret.Reveal()
	require.True(t, strings.HasPrefix(dsn, "postgresql://"), dsn)
}

// A stored connection string Pando cannot read is an error, not a new password.
// Regenerating would overwrite the only copy of the credential that still
// matches the data on disk.
func TestAnUnreadableStoredConnectionStringIsRefused(t *testing.T) {
	_, err := configured(t).Provision(context.Background(), api.ProvisionRequest{
		AppID: "app_01HQ8", SlotKey: "DATABASE_URL", Type: spec.SlotPostgres,
		ServiceID: "svc_01HQ9", ExistingSecret: secret.New("postgres://nopassword@host/db"),
	})
	require.Error(t, err)
}

// TestR135_TheDataIsInTheAppsOwnStorage asserts R-135.
//
// The capability is how the backup path knows the service is already covered by
// the app's volume snapshots. Getting this wrong in either direction is
// expensive: a false claim omits every provisioned database from the DR bundle,
// and a true one where it does not hold doubles the bundle's size.
func TestR135_TheDataIsInTheAppsOwnStorage(t *testing.T) {
	a := configured(t)
	require.True(t, a.Capabilities().DataInAppVolumes)

	// And it says so rather than handing back an empty archive, which is the
	// version an operator only discovers at restore time.
	err := a.Snapshot(context.Background(), api.ServiceHandle{ServiceID: "svc_01HQ9"}, nil)
	require.Error(t, err)
}

// Pando recognizes S3 and SMTP slots and deliberately does not stand them up:
// running that infrastructure is what R-010 says Pando is not.
func TestUnsupportedTypesAreRefusedRatherThanApproximated(t *testing.T) {
	supported := map[spec.SlotType]bool{}
	for _, s := range configured(t).Supports() {
		supported[s] = true
	}
	require.False(t, supported[spec.SlotS3])
	require.False(t, supported[spec.SlotSMTP])
	require.False(t, supported[spec.SlotUnknown])

	_, err := configured(t).Provision(context.Background(), api.ProvisionRequest{
		Type: spec.SlotS3, ServiceID: "svc_01HQ9", SlotKey: "S3_BUCKET",
	})
	require.Error(t, err)
}

// An install can pin its own images; it cannot name a type Pando cannot fill.
func TestImagesAreConfigurable(t *testing.T) {
	a := servicesdocker.New()
	require.NoError(t, a.Configure(context.Background(),
		json.RawMessage(`{"images":{"postgres":"mirror.internal/postgres:16"}}`)))
	require.Equal(t, "mirror.internal/postgres:16", provision(t, a, spec.SlotPostgres, secret.Value{}).Workloads[0].Image)

	require.Error(t, servicesdocker.New().Configure(context.Background(),
		json.RawMessage(`{"images":{"cassandra":"cassandra:5"}}`)))
}

// The generated password never needs escaping.
//
// A password containing a quote produces a service that starts perfectly and
// rejects every connection — a bug report nobody can read.
func TestGeneratedPasswordsNeedNoEscaping(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		res := provision(t, configured(t), spec.SlotRedis, secret.Value{})
		dsn := res.ConnectionSecret.Reveal()
		password, _ := mustParse(t, dsn).User.Password()

		require.Equal(t, password, url.QueryEscape(password), "no URL escaping needed")
		require.False(t, strings.ContainsAny(password, "'\"`$\\ \n"), "no shell or config escaping needed")
		require.False(t, seen[password], "passwords are not repeated")
		seen[password] = true
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}
