package docker_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
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
	for _, typ := range []spec.SlotType{spec.SlotPostgres, spec.SlotMySQL, spec.SlotRedis, spec.SlotMongoDB} {
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

// TestR131_AProvisionedPostgresURLUsesTheSchemeTheAppSpells asserts R-131: the
// connection string a provisioned Postgres fills its slot with is one the app
// can use. SQLAlchemy 2 refuses postgres://, readflow refuses postgresql://,
// and asyncpg refuses an sslmode in the query (issue #90).
func TestR131_AProvisionedPostgresURLUsesTheSchemeTheAppSpells(t *testing.T) {
	for spelled, want := range map[string]string{
		"":           "postgresql",
		"postgresql": "postgresql",
		"postgres":   "postgres",
		"mysql":      "postgresql",
	} {
		res, err := configured(t).Provision(context.Background(), api.ProvisionRequest{
			AppID: "app_01HQ8", SlotKey: "DATABASE_URL", Type: spec.SlotPostgres,
			ServiceID: "svc_01HQ9", Scheme: spelled,
		})
		require.NoError(t, err)
		u := mustParse(t, res.ConnectionSecret.Reveal())
		require.Equal(t, want, u.Scheme, "spelled %q", spelled)
		require.Empty(t, u.RawQuery, "no sslmode: the server offers TLS, and asyncpg refuses the parameter")
	}
}

// TestR131_AProvisionedPostgresServesTheSameCertificateEveryDeploy asserts
// R-131: the provisioned Postgres serves TLS, so a driver that requires it
// (lib/pq) connects with a URL that does not say sslmode. The certificate is the
// same bytes on every deploy of one service, because a changed environment
// recreates the container and Provision runs on every deploy and reconciler
// tick.
func TestR131_AProvisionedPostgresServesTheSameCertificateEveryDeploy(t *testing.T) {
	a := configured(t)
	first := provision(t, a, spec.SlotPostgres, secret.Value{})
	second := provision(t, a, spec.SlotPostgres, first.ConnectionSecret)
	other := provision(t, a, spec.SlotPostgres, secret.Value{})

	env := first.Workloads[0].Env
	certPEM, keyPEM := env["PANDO_TLS_CERT"].Reveal(), env["PANDO_TLS_KEY"].Reveal()
	pair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	require.NoError(t, err, "a certificate and the key that matches it")
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	require.NoError(t, err)
	require.Equal(t, []string{first.Workloads[0].Name}, leaf.DNSNames)
	require.True(t, leaf.NotAfter.Year() == 9999, "nothing to expire for: nobody verifies it")

	require.Equal(t, certPEM, second.Workloads[0].Env["PANDO_TLS_CERT"].Reveal())
	require.Equal(t, keyPEM, second.Workloads[0].Env["PANDO_TLS_KEY"].Reveal())
	require.Equal(t, first.Workloads[0].Command, second.Workloads[0].Command)
	require.NotEqual(t, keyPEM, other.Workloads[0].Env["PANDO_TLS_KEY"].Reveal(),
		"another service, another password, another key")

	cmd := strings.Join(first.Workloads[0].Command, " ")
	require.Contains(t, cmd, "ssl=on")
	require.Contains(t, cmd, "exec docker-entrypoint.sh postgres", "the image still creates the database")
	require.NotContains(t, cmd, "BEGIN", "the key reaches the container through its environment, not its command")
}

// TestR194_TheProvisionedPostgresKeyNeverRenders asserts R-194: the private key
// is a secret.Value, so a plan that is logged or marshaled shows [redacted].
func TestR194_TheProvisionedPostgresKeyNeverRenders(t *testing.T) {
	res := provision(t, configured(t), spec.SlotPostgres, secret.Value{})
	b, err := json.Marshal(res.Workloads[0])
	require.NoError(t, err)
	require.NotContains(t, string(b), "PRIVATE KEY")
	require.NotContains(t, fmt.Sprintf("%v %+v", res.Workloads[0], res.Workloads[0].Env), "PRIVATE KEY")
}

// TestR131_AProvisionedMongoDBSignsInWhereItsUserLives asserts R-131. The
// image creates one user, a root user, and a root user lives in the admin
// database: without authSource=admin the driver looks for it in the app's
// database and every connection is refused.
func TestR131_AProvisionedMongoDBSignsInWhereItsUserLives(t *testing.T) {
	res := provision(t, configured(t), spec.SlotMongoDB, secret.Value{})
	u, err := url.Parse(res.ConnectionSecret.Reveal())
	require.NoError(t, err)
	require.Equal(t, "mongodb", u.Scheme)
	require.Equal(t, "admin", u.Query().Get("authSource"))
	password, _ := u.User.Password()
	require.Equal(t, password, res.Workloads[0].Env["MONGO_INITDB_ROOT_PASSWORD"].Reveal())
	require.Equal(t, "/data/db", res.Workloads[0].Mounts[0].Path)

	again := provision(t, configured(t), spec.SlotMongoDB, res.ConnectionSecret)
	require.Equal(t, res.ConnectionSecret.Reveal(), again.ConnectionSecret.Reveal(), "and keeps its password")
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

// TestR131_AProvisionedMemcachedIsAnAddressWithNothingToKeep asserts R-131 for
// a cache. Yopass reads MEMCACHED as "host:port", which is how memcached's
// clients take it: no scheme and no credentials. It holds nothing to back up,
// so it has no volume to outlive the app (R-204), and a redeploy hands back the
// same address rather than refusing one with no password in it.
func TestR131_AProvisionedMemcachedIsAnAddressWithNothingToKeep(t *testing.T) {
	a := configured(t)
	require.Contains(t, a.Supports(), spec.SlotMemcached)

	res := provision(t, a, spec.SlotMemcached, secret.Value{})
	require.Len(t, res.Workloads, 1)
	w := res.Workloads[0]
	require.Equal(t, w.Name+":11211", res.ConnectionSecret.Reveal())
	require.Empty(t, res.Volumes)
	require.Empty(t, w.Mounts)
	require.NotNil(t, w.Health)
	require.Equal(t, 11211, w.Health.Port)
	require.False(t, w.Exposed, "R-134")

	again := provision(t, a, spec.SlotMemcached, res.ConnectionSecret)
	require.Equal(t, res.ConnectionSecret.Reveal(), again.ConnectionSecret.Reveal())
}

// Each per-type setting a form can show replaces that type's image, and only
// that type's.
func TestEachImageSettingOverridesItsImage(t *testing.T) {
	defaults := map[spec.SlotType]string{}
	base := configured(t)
	for _, typ := range base.Supports() {
		defaults[typ] = provision(t, base, typ, secret.Value{}).Workloads[0].Image
	}
	for _, typ := range base.Supports() {
		t.Run(string(typ), func(t *testing.T) {
			a := servicesdocker.New()
			raw := fmt.Sprintf(`{%q: "mirror.internal/%s:pinned"}`, string(typ)+"_image", typ)
			require.NoError(t, a.Configure(context.Background(), json.RawMessage(raw)))
			for _, other := range a.Supports() {
				want := defaults[other]
				if other == typ {
					want = "mirror.internal/" + string(typ) + ":pinned"
				}
				require.Equal(t, want, provision(t, a, other, secret.Value{}).Workloads[0].Image, other)
			}
		})
	}
}

// Configuration declared in the config file, or stored, before the per-type
// settings existed still uses the images map (R-271).
func TestTheImagesMapStillWorks(t *testing.T) {
	a := servicesdocker.New()
	require.NoError(t, a.Configure(context.Background(),
		json.RawMessage(`{"images":{"redis":"mirror.internal/redis:7","mongodb":"mirror.internal/mongo:7"}}`)))
	require.Equal(t, "mirror.internal/redis:7", provision(t, a, spec.SlotRedis, secret.Value{}).Workloads[0].Image)
	require.Equal(t, "mirror.internal/mongo:7", provision(t, a, spec.SlotMongoDB, secret.Value{}).Workloads[0].Image)
}

// Given both for one type, the per-type setting wins; the map still sets the
// others.
func TestAnImageSettingWinsOverTheImagesMap(t *testing.T) {
	a := servicesdocker.New()
	require.NoError(t, a.Configure(context.Background(), json.RawMessage(
		`{"images":{"postgres":"old/postgres:16","mysql":"old/mysql:8"},"postgres_image":"new/postgres:17"}`)))
	require.Equal(t, "new/postgres:17", provision(t, a, spec.SlotPostgres, secret.Value{}).Workloads[0].Image)
	require.Equal(t, "old/mysql:8", provision(t, a, spec.SlotMySQL, secret.Value{}).Workloads[0].Image)
}

// Nothing set, or a setting left blank, runs the defaults, and each setting
// states that default for a form to show.
func TestDefaultImagesWhenNothingIsSet(t *testing.T) {
	want := map[spec.SlotType]string{
		spec.SlotPostgres:  "postgres:17-alpine",
		spec.SlotMySQL:     "mysql:8.4",
		spec.SlotRedis:     "redis:7-alpine",
		spec.SlotMongoDB:   "mongo:7",
		spec.SlotMemcached: "memcached:1.6-alpine",
	}
	for _, raw := range []string{``, `{}`, `{"postgres_image":"  ","redis_image":""}`} {
		a := servicesdocker.New()
		require.NoError(t, a.Configure(context.Background(), json.RawMessage(raw)))
		for typ, img := range want {
			require.Equal(t, img, provision(t, a, typ, secret.Value{}).Workloads[0].Image, raw)
		}
	}

	info := servicesdocker.Info()
	require.NoError(t, info.Validate())
	require.Len(t, info.Fields, len(want))
	for _, f := range info.Fields {
		typ := spec.SlotType(strings.TrimSuffix(f.Key, "_image"))
		require.Equal(t, want[typ], f.Default, f.Key)
		require.True(t, f.Advanced, f.Key)
		require.Equal(t, "string", f.Type, f.Key)
	}
}

func TestAnImageSettingThatIsNotTextIsRefused(t *testing.T) {
	err := servicesdocker.New().Configure(context.Background(), json.RawMessage(`{"mysql_image":8}`))
	require.ErrorContains(t, err, "mysql_image")
}

// TestR423_DatabasesAreDumpedInTheirOwnWorkload asserts the plans R-423 runs:
// Postgres and MySQL dumped and loaded in the workload Provision created,
// standing for its volume, with no password in argv; nothing else dumped.
func TestR423_DatabasesAreDumpedInTheirOwnWorkload(t *testing.T) {
	a := configured(t)
	for _, slot := range []spec.SlotType{spec.SlotPostgres, spec.SlotMySQL} {
		res, err := a.Provision(context.Background(), api.ProvisionRequest{
			AppID: "app_1", SlotKey: "DATABASE_URL", Type: slot, ServiceID: "svc_01ABC",
		})
		require.NoError(t, err)
		plan, ok := a.Dump(res.Handle, slot)
		require.True(t, ok, slot)
		require.Equal(t, res.Workloads[0].Name, plan.Workload, slot)
		require.Equal(t, []string{res.Volumes[0].VolumeID}, plan.Volumes, slot)
		require.NotEmpty(t, plan.Dump, slot)
		require.NotEmpty(t, plan.Load, slot)
		password, _ := url.Parse(res.ConnectionSecret.Reveal())
		pw, _ := password.User.Password()
		require.NotContains(t, strings.Join(append(plan.Dump, plan.Load...), " "), pw, slot)
	}
	for _, slot := range []spec.SlotType{spec.SlotRedis, spec.SlotMongoDB, spec.SlotMemcached} {
		_, ok := a.Dump(api.ServiceHandle{ServiceID: "svc_01ABC"}, slot)
		require.False(t, ok, slot)
	}
}
