package docker

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"math/big"
	"net/url"
	"slices"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Kind is the adapter kind.
const Kind = "docker"

// Adapter provisions in-bundle services.
//
// It talks to no daemon. That is the whole design: a provisioned service is a
// workload like any other, so this adapter *plans* one and the runtime adapter
// runs it. Two consequences follow, and both are the point.
//
// The service is reachable only from inside the app's private network, because
// that is the only network its workloads are on and nothing publishes a port
// (R-134). There is no code here that could accidentally expose it.
//
// Its data is in a Pando volume, so it is recorded, backed up with the app,
// carried in the DR bundle, offered at delete and reclaimed afterwards — all by
// machinery that already exists and knows nothing about services (R-135).
type Adapter struct {
	images map[spec.SlotType]string
}

// New returns an unconfigured adapter.
func New() *Adapter { return &Adapter{} }

type config struct {
	// Images overrides the image used per slot type, keyed by type name.
	// An install that mirrors Docker Hub, or that has settled on a particular
	// Postgres major version, sets it here rather than editing this file.
	//
	// The older spelling, kept for configurations declared in the config file
	// (R-271) and stored before the per-type settings below existed. No form
	// can set it: a form sends only the settings Info declares, and saving one
	// replaces the whole configuration, so it is dropped by the first save
	// from the console.
	Images map[string]string `json:"images"`

	// One setting per type, as Info declares them: postgres_image and so on.
	// Decoded separately, by imageKey, so the list lives in one place.
	perType map[spec.SlotType]string
}

// imageKey is the setting that overrides the image for a slot type.
func imageKey(t spec.SlotType) string { return string(t) + "_image" }

// imageLabels name each type the way its setting is labeled.
var imageLabels = map[spec.SlotType]string{
	spec.SlotPostgres:  "PostgreSQL image",
	spec.SlotMySQL:     "MySQL image",
	spec.SlotRedis:     "Redis image",
	spec.SlotMongoDB:   "MongoDB image",
	spec.SlotMemcached: "Memcached image",
}

// defaultImages are the images a provisioned service runs.
//
// [P]. Pinned to a major version and not a digest, matching the runtime
// adapter's helper image, and carrying the same known weakness: the bytes
// behind a tag can change. Alpine variants because a hobbyist's host is the
// deployment target and a 400MB Postgres image on a 2-core VPS is a cost with
// nothing to show for it.
var defaultImages = map[spec.SlotType]string{
	spec.SlotPostgres: "postgres:17-alpine",
	spec.SlotMySQL:    "mysql:8.4",
	spec.SlotRedis:    "redis:7-alpine",
	// Not Alpine: MongoDB publishes no Alpine image.
	spec.SlotMongoDB:   "mongo:7",
	spec.SlotMemcached: "memcached:1.6-alpine",
}

func (a *Adapter) Kind() string           { return Kind }
func (a *Adapter) Category() api.Category { return api.CategoryServices }

func (a *Adapter) Configure(_ context.Context, raw json.RawMessage) error {
	cfg, err := parseConfig(raw)
	if err != nil {
		return err
	}

	a.images = map[spec.SlotType]string{}
	for t, img := range defaultImages {
		a.images[t] = img
	}
	for name, img := range cfg.Images {
		t := spec.SlotType(name)
		if _, ok := defaultImages[t]; !ok {
			return errs.Newf(errs.ValidInvalid,
				"Pando cannot provision a %q service, so there is no image to set for it.", name).
				WithRemedy("Valid types are postgres, mysql, redis, mongodb and memcached.")
		}
		if img == "" {
			return errs.Newf(errs.ValidInvalid, "The image for %q is empty.", name)
		}
		a.images[t] = img
	}
	// A per-type setting wins over the same type in the older images map.
	// It is the spelling a form shows and sets, so when a configuration
	// carries both, the per-type one is the more recent intent; and someone
	// reading the adapter's settings sees the image it actually runs.
	for t, img := range cfg.perType {
		a.images[t] = img
	}
	return nil
}

func parseConfig(raw json.RawMessage) (config, error) {
	cfg := config{perType: map[spec.SlotType]string{}}
	if len(raw) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, errs.Wrap(errs.ValidInvalid, "The service provisioner could not be configured.", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return cfg, errs.Wrap(errs.ValidInvalid, "The service provisioner could not be configured.", err)
	}
	for t := range defaultImages {
		v, ok := keys[imageKey(t)]
		if !ok {
			continue
		}
		var img string
		if err := json.Unmarshal(v, &img); err != nil {
			return cfg, errs.Newf(errs.ValidInvalid, "The setting %s is not text.", imageKey(t)).
				WithRemedy("Give an image reference such as " + defaultImages[t] + ", or leave it empty for that default.")
		}
		// Empty means the default, as it does for any setting a form leaves
		// blank.
		if img = strings.TrimSpace(img); img != "" {
			cfg.perType[t] = img
		}
	}
	return cfg, nil
}

// HealthCheck always passes.
//
// There is nothing to reach: this adapter holds no connection and provisions by
// planning. The runtime adapter's health is what decides whether a provisioned
// service can actually start, and the planner already checks that.
func (a *Adapter) HealthCheck(_ context.Context) error { return nil }

// Capabilities reports that provisioned data lives in the app's own volumes.
func (a *Adapter) Capabilities() api.ServicesCapabilities {
	return api.ServicesCapabilities{DataInAppVolumes: true}
}

// Supports lists the slot types this adapter can fill.
//
// Databases and caches, deliberately nothing else. S3 and SMTP are slot types Pando recognizes (R-130) and
// deliberately does not provision: standing up MinIO or an SMTP server is
// running infrastructure, which is what R-010 says Pando is not. Those slots
// are bound or literal, and the planner says so by name.
func (a *Adapter) Supports() []spec.SlotType {
	return []spec.SlotType{spec.SlotPostgres, spec.SlotMySQL, spec.SlotRedis, spec.SlotMongoDB, spec.SlotMemcached}
}

// Provision plans one service instance.
//
// Pure: no daemon call, no randomness beyond the password, and the same request
// twice produces the same shape. The caller stores the connection secret and
// hands the workloads and volumes to the runtime, which is what actually starts
// anything.
func (a *Adapter) Provision(_ context.Context, req api.ProvisionRequest) (api.ProvisionResult, error) {
	image, ok := a.images[req.Type]
	if !ok {
		return api.ProvisionResult{}, errs.Newf(errs.PlanAdapterNotConfigured,
			"Pando cannot provision a %s.", req.Type.DisplayName()).
			WithRemedy("Connect this slot to an existing instance, or paste a connection string.")
	}
	if req.ServiceID == "" {
		return api.ProvisionResult{}, errs.New(errs.Internal, "A service was provisioned without an identifier.")
	}

	name, volumeID := names(req.ServiceID)

	// Reuse the credentials this service was created with. See
	// ProvisionRequest.ExistingSecret: a database sets its password at initdb
	// and never reads the variable again.
	// A memcached has no password, and its stored address has none to reuse:
	// reading one out of it refused every redeploy after the first.
	var password string
	if req.Type != spec.SlotMemcached {
		var err error
		if password, err = reuseOrGenerate(req.ExistingSecret); err != nil {
			return api.ProvisionResult{}, err
		}
	}

	var (
		env  map[string]secret.Value
		path string
		dsn  string
	)
	switch req.Type {
	case spec.SlotPostgres:
		// The database and user are named for the app, not "postgres": an
		// operator reading psql output should be able to tell whose database
		// they are looking at.
		env = map[string]secret.Value{
			"POSTGRES_DB":       secret.New("app"),
			"POSTGRES_USER":     secret.New("app"),
			"POSTGRES_PASSWORD": secret.New(password),
			// Without this, Postgres puts its data in a subdirectory of the
			// mount and a lost+found in the volume root makes initdb refuse to
			// start on some hosts.
			"PGDATA": secret.New("/var/lib/postgresql/data/pgdata"),
		}
		path = "/var/lib/postgresql/data"
		cert, key, err := postgresTLS(name, password)
		if err != nil {
			return api.ProvisionResult{}, err
		}
		env["PANDO_TLS_CERT"] = cert
		env["PANDO_TLS_KEY"] = key
		// No query string. The server offers TLS (postgresTLS says why), so a
		// client's own default connects, and a URL that names an sslmode is
		// one some drivers refuse outright: asyncpg takes it as a keyword
		// argument it does not have (issue #90).
		dsn = (&url.URL{
			Scheme: postgresScheme(req.Scheme), User: url.UserPassword("app", password),
			Host: name + ":5432", Path: "/app",
		}).String()

	case spec.SlotMySQL:
		env = map[string]secret.Value{
			"MYSQL_DATABASE":      secret.New("app"),
			"MYSQL_USER":          secret.New("app"),
			"MYSQL_PASSWORD":      secret.New(password),
			"MYSQL_ROOT_PASSWORD": secret.New(password),
		}
		path = "/var/lib/mysql"
		dsn = (&url.URL{
			Scheme: "mysql", User: url.UserPassword("app", password),
			Host: name + ":3306", Path: "/app",
		}).String()

	case spec.SlotRedis:
		// Redis takes its password on the command line rather than from the
		// environment, and that argv is visible to anything that can list
		// processes on the host. The requirepass is set through a config file
		// written into the data volume instead — see Command below.
		env = map[string]secret.Value{}
		path = "/data"
		dsn = (&url.URL{
			Scheme: "redis", User: url.UserPassword("default", password),
			Host: name + ":6379", Path: "/0",
		}).String()

	case spec.SlotMongoDB:
		// The root user, because it is the only one the image's
		// MONGO_INITDB_* variables create — and with it an app that creates
		// its own databases, as most do, can. authSource=admin is where a
		// root user lives; without it the driver looks for the user in the
		// "app" database and every connection is refused.
		env = map[string]secret.Value{
			"MONGO_INITDB_ROOT_USERNAME": secret.New("app"),
			"MONGO_INITDB_ROOT_PASSWORD": secret.New(password),
			"MONGO_INITDB_DATABASE":      secret.New("app"),
		}
		path = "/data/db"
		dsn = (&url.URL{
			Scheme: "mongodb", User: url.UserPassword("app", password),
			Host: name + ":27017", Path: "/app", RawQuery: "authSource=admin",
		}).String()

	case spec.SlotMemcached:
		// "host:port", with no scheme and no credentials: memcached has no
		// URL form and no password unless it is built with SASL, and its
		// clients take a list of server addresses. Yopass reads MEMCACHED
		// this way. It holds nothing worth keeping, so it has no volume
		// (below), and it is not addressable from outside the bundle (R-134),
		// which is the only lock it needs.
		env = map[string]secret.Value{}
		dsn = name + ":11211"
	}

	w := api.WorkloadPlan{
		Name:  name,
		Image: image,
		Env:   env,
		// Not exposed and no ports (R-134). A provisioned service is reachable
		// on the bundle's private network by name and nowhere else.
		Exposed: false,
		Health:  healthFor(req.Type),
	}
	// A cache with nothing on disk gets no volume. Volumes outlive the app
	// (R-204), and one kept for a memcached would be a backup prompt for data
	// that was never written anywhere.
	var volumes []api.VolumePlan
	if path != "" {
		w.Mounts = []api.MountPlan{{VolumeID: volumeID, Path: path}}
		volumes = []api.VolumePlan{{VolumeID: volumeID, Name: name + "-data"}}
	}
	if req.Type == spec.SlotPostgres {
		// The certificate is written each start, like Redis's config below,
		// and owned by postgres because the server refuses a key anyone else
		// can read. The image's own entrypoint still runs — it is what creates
		// the database on first start — with the TLS settings as arguments,
		// and the key is out of its environment before it does.
		w.Command = []string{"sh", "-c", `umask 077 && mkdir -p ` + postgresTLSDir +
			` && printf '%s' "$PANDO_TLS_CERT" > ` + postgresTLSDir + `/server.crt` +
			` && printf '%s' "$PANDO_TLS_KEY" > ` + postgresTLSDir + `/server.key` +
			` && { [ "$(id -u)" != 0 ] || chown -R postgres:postgres ` + postgresTLSDir + `; }` +
			` && unset PANDO_TLS_CERT PANDO_TLS_KEY` +
			` && exec docker-entrypoint.sh postgres -c ssl=on` +
			` -c ssl_cert_file=` + postgresTLSDir + `/server.crt` +
			` -c ssl_key_file=` + postgresTLSDir + `/server.key`}
	}
	if req.Type == spec.SlotRedis {
		w.Command = []string{"sh", "-c",
			// Written each start rather than once, so rotating the password is
			// a redeploy rather than a manual edit inside a volume.
			`printf 'requirepass %s\n' "$REDIS_PASSWORD" > /data/pando.conf && exec redis-server /data/pando.conf --dir /data`}
		w.Env = map[string]secret.Value{"REDIS_PASSWORD": secret.New(password)}
	}

	return api.ProvisionResult{
		Handle:           api.ServiceHandle{ServiceID: req.ServiceID, Handle: name},
		ConnectionSecret: secret.New(dsn),
		Workloads:        []api.WorkloadPlan{w},
		Volumes:          volumes,
	}, nil
}

// postgresScheme is the scheme a provisioned Postgres URL is written with: the
// app's own spelling when it is one Postgres answers to (spec.Slot.Scheme),
// postgresql:// otherwise.
//
// Drivers disagree and refuse each other's spelling. SQLAlchemy 2 will not load
// postgres:// ("Can't load plugin: sqlalchemy.dialects:postgres"); readflow
// will not accept postgresql:// ("unsupported database: postgresql", issue
// #90). No single spelling works for both, but the app's configuration already
// says which one it reads.
func postgresScheme(spelled string) string {
	if slices.Contains(spec.PostgresSchemes, spelled) {
		return spelled
	}
	return spec.PostgresSchemes[0]
}

// healthFor is how the runtime knows the service is ready.
//
// It matters more here than for an app workload: the app's own container will
// start at the same moment and connect immediately, and a database that is
// still running initdb refuses the connection. DependsOn plus a health check is
// what turns that race into an ordering.
func healthFor(t spec.SlotType) *api.HealthPlan {
	switch t {
	case spec.SlotPostgres:
		return &api.HealthPlan{
			Command: []string{"pg_isready", "-U", "app", "-d", "app"},
			// Short and frequent: this is a local socket, and the cost of
			// checking is nothing against an app waiting to start.
			IntervalSeconds: 5, TimeoutSeconds: 5, Retries: 12,
		}
	case spec.SlotMySQL:
		// No credentials, deliberately. `mysqladmin ping` exits 0 when the
		// server answers at all, including with "access denied" — which is what
		// ping means and is the whole liveness signal. Measured, because the
		// obvious-looking alternative is worse in two ways: a password on argv
		// is visible in the host's process list and re-run every few seconds
		// forever, and putting it in MYSQL_PWD instead breaks the image's
		// entrypoint outright — it connects as root with no password during
		// initialization, MYSQL_PWD overrides that, and the container exits 1
		// on "Access denied" before the database is ever created.
		return &api.HealthPlan{
			Command: []string{"mysqladmin", "ping", "-h", "127.0.0.1"},
			// Longer than the others: MySQL's first start initializes the data
			// directory and can take a minute on a small host.
			IntervalSeconds: 5, TimeoutSeconds: 5, Retries: 24,
		}
	case spec.SlotRedis:
		// A port check rather than redis-cli ping, for the same reason: with a
		// requirepass set, an unauthenticated PING answers NOAUTH and redis-cli
		// exits non-zero. Redis binds the port after it finishes loading, so
		// the port answering is a true readiness signal.
		return &api.HealthPlan{
			Port:            6379,
			IntervalSeconds: 5, TimeoutSeconds: 3, Retries: 12,
		}
	case spec.SlotMemcached:
		// memcached binds its port once it is ready to serve.
		return &api.HealthPlan{
			Port:            11211,
			IntervalSeconds: 5, TimeoutSeconds: 3, Retries: 12,
		}
	case spec.SlotMongoDB:
		// ping needs no credentials, so none go on argv.
		return &api.HealthPlan{
			Command:         []string{"mongosh", "--quiet", "--eval", "db.adminCommand('ping')"},
			IntervalSeconds: 5, TimeoutSeconds: 5, Retries: 24,
		}
	}
	return nil
}

// Destroy releases a provisioned service.
//
// A no-op, and not a stub. The service is workloads and a volume inside the
// app's bundle; the runtime destroys the workloads with the bundle and the
// volume follows R-204 and R-135, which means it outlives the app on purpose
// and is reclaimed once it is backed up. Destroying it here would delete an
// app's database out from under the backup prompt that is about to offer it.
func (a *Adapter) Destroy(_ context.Context, _ api.ServiceHandle) error { return nil }

// Snapshot is not this adapter's job, and says so rather than returning empty.
//
// Capabilities().DataInAppVolumes is true: the data is in a Pando volume that
// the backup path already snapshots. A caller reaching here has ignored the
// capability, and an empty tar would make that look like a service with no
// data — which is exactly the failure an operator discovers at restore time.
func (a *Adapter) Snapshot(_ context.Context, h api.ServiceHandle, _ io.Writer) error {
	return errs.Newf(errs.Internal,
		"The data for %s is in this app's storage and is backed up with it, not separately.", h.ServiceID)
}

// Restore is refused for the same reason as Snapshot.
func (a *Adapter) Restore(_ context.Context, h api.ServiceHandle, _ io.Reader) error {
	return errs.Newf(errs.Internal,
		"The data for %s is restored with this app's storage, not separately.", h.ServiceID)
}

// names are a service's workload and volume. They carry the service ID, not
// the slot key: a slot key can be renamed in the spec, and renaming it must not
// orphan a database the app is still using.
func names(serviceID string) (workload, volumeID string) {
	return "svc-" + strings.ToLower(strings.TrimPrefix(serviceID, "svc_")),
		"vol_" + strings.TrimPrefix(serviceID, "svc_")
}

// Dump is the engine's own dump and load for a database (R-423), run in the
// service's workload while it serves: pg_dump and mysqldump each read one
// transaction's view of the data, which a copy of the files under a running
// server is not (#344).
//
// Load replaces what the dump holds, in one transaction where the engine has
// one, so an app connected during a restore reads the old data or the new and
// never half of each. Tables created since the dump are left alone.
//
// Redis and MongoDB are copied with their volumes. Redis writes its dump file
// whole and renames it into place, so a copy holds its last save; MongoDB's
// tools are not in every image this adapter may be configured to run.
//
// The password stays out of argv, which anything on the host can list:
// Postgres trusts its own socket in the official image, and MySQL's client
// reads MYSQL_PWD.
func (a *Adapter) Dump(h api.ServiceHandle, t spec.SlotType) (api.DumpPlan, bool) {
	workload, volumeID := names(h.ServiceID)
	plan := api.DumpPlan{Workload: workload, Volumes: []string{volumeID}}
	switch t {
	case spec.SlotPostgres:
		plan.Dump = []string{"pg_dump", "-U", "app", "-d", "app", "--format=custom"}
		plan.Load = []string{"pg_restore", "-U", "app", "-d", "app",
			"--clean", "--if-exists", "--no-owner", "--single-transaction", "--exit-on-error"}
	case spec.SlotMySQL:
		// mysqldump, or mariadb-dump in an image that has only that.
		plan.Dump = []string{"sh", "-c", `d=$(command -v mysqldump || command -v mariadb-dump) && ` +
			`MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec "$d" -uroot --single-transaction --routines --triggers --events app`}
		plan.Load = []string{"sh", "-c", `c=$(command -v mysql || command -v mariadb) && ` +
			`MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec "$c" -uroot app`}
	default:
		return api.DumpPlan{}, false
	}
	return plan, true
}

// passwordAlphabet excludes characters that need escaping in a URL, a shell
// word or a Redis config line.
//
// A generated password that happens to contain a quote produces a service that
// starts perfectly and rejects every connection, which is a bug report nobody
// can read. Sixty-two characters at 32 positions is ~190 bits; losing the
// punctuation costs nothing that matters here.
const passwordAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// reuseOrGenerate returns the password inside an existing connection string, or
// a new one when there is no existing string.
//
// A stored value that does not parse is an error rather than a silent
// regeneration. Regenerating would produce a deploy that succeeds and an app
// that cannot log in, and would overwrite the only copy of the password that
// still matches the data on disk.
func reuseOrGenerate(existing secret.Value) (string, error) {
	raw := existing.Reveal()
	if raw == "" {
		return randomPassword()
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return "", errs.New(errs.StateInvalid,
			"The stored connection string for this service could not be read, so Pando will not redeploy it.").
			WithRemedy("Restore this app from a backup, or change the slot to connect to an instance you can reach.")
	}
	password, ok := u.User.Password()
	if !ok {
		return "", errs.New(errs.StateInvalid,
			"The stored connection string for this service has no password in it.").
			WithRemedy("Restore this app from a backup, or change the slot to connect to an instance you can reach.")
	}
	return password, nil
}

func randomPassword() (string, error) {
	b := make([]byte, 32)
	max := big.NewInt(int64(len(passwordAlphabet)))
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", errs.Wrap(errs.Internal, "Pando could not generate a password for the service.", err)
		}
		b[i] = passwordAlphabet[n.Int64()]
	}
	return string(b), nil
}

// Info describes this kind of adapter for the forms that configure one
// (api.KindInfo, R-261).
func Info() api.KindInfo {
	return api.KindInfo{
		Category:    api.CategoryServices,
		Kind:        Kind,
		Name:        "Docker",
		Description: "Provisions the databases and caches apps declare, as containers.",
		IDPrefix:    "svcs_",
		Fields:      imageFields(),
	}
}

// imageFields are the per-type image overrides, in the order Supports lists
// the types, each stating the image it replaces as its default.
func imageFields() []api.Field {
	types := New().Supports()
	fields := make([]api.Field, 0, len(types))
	for _, t := range types {
		fields = append(fields, api.Field{
			Key:      imageKey(t),
			Label:    imageLabels[t],
			Help:     "The container image a provisioned " + t.DisplayName() + " runs.",
			Type:     "string",
			Advanced: true,
			Default:  defaultImages[t],
		})
	}
	return fields
}
