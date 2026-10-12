package docker

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"math/big"
	"net/url"
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
	Images map[string]string `json:"images"`
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
}

func (a *Adapter) Kind() string           { return Kind }
func (a *Adapter) Category() api.Category { return api.CategoryServices }

func (a *Adapter) Configure(_ context.Context, raw json.RawMessage) error {
	cfg := config{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return errs.Wrap(errs.ValidInvalid, "The service provisioner could not be configured.", err)
		}
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
				WithRemedy("Valid types are postgres, mysql and redis.")
		}
		if img == "" {
			return errs.Newf(errs.ValidInvalid, "The image for %q is empty.", name)
		}
		a.images[t] = img
	}
	return nil
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
// Deliberately three. S3 and SMTP are slot types Pando recognizes (R-130) and
// deliberately does not provision: standing up MinIO or an SMTP server is
// running infrastructure, which is what R-010 says Pando is not. Those slots
// are bound or literal, and the planner says so by name.
func (a *Adapter) Supports() []spec.SlotType {
	return []spec.SlotType{spec.SlotPostgres, spec.SlotMySQL, spec.SlotRedis}
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

	// The workload and volume names carry the service ID, not the slot key. A
	// slot key can be renamed in the spec; renaming it must not orphan a
	// database the app is still using.
	name := "svc-" + strings.ToLower(strings.TrimPrefix(req.ServiceID, "svc_"))
	volumeID := "vol_" + strings.TrimPrefix(req.ServiceID, "svc_")

	// Reuse the credentials this service was created with. See
	// ProvisionRequest.ExistingSecret: a database sets its password at initdb
	// and never reads the variable again.
	password, err := reuseOrGenerate(req.ExistingSecret)
	if err != nil {
		return api.ProvisionResult{}, err
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
		// postgresql://, the scheme libpq documents and every client reads.
		// SQLAlchemy 2 refuses the short postgres:// outright ("Can't load
		// plugin: sqlalchemy.dialects:postgres"), and an app whose compose
		// file spelled the long one stopped at its first migration (issue #90).
		dsn = (&url.URL{
			Scheme: "postgresql", User: url.UserPassword("app", password),
			Host: name + ":5432", Path: "/app", RawQuery: "sslmode=disable",
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
	}

	w := api.WorkloadPlan{
		Name:  name,
		Image: image,
		Env:   env,
		// Not exposed and no ports (R-134). A provisioned service is reachable
		// on the bundle's private network by name and nowhere else.
		Exposed: false,
		Mounts:  []api.MountPlan{{VolumeID: volumeID, Path: path}},
		Health:  healthFor(req.Type),
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
		Volumes:          []api.VolumePlan{{VolumeID: volumeID, Name: name + "-data"}},
	}, nil
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
		Fields:      []api.Field{},
	}
}
