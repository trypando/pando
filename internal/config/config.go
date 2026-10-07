package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/tokenkey"
)

// Config is Pando's configuration, from YAML, environment, and flags (R-271).
//
// Adapters usually live in the database (adapter_configs), so they can be
// changed through the API. An install kept as code may declare them in the
// file instead (Adapters, see adapters.go), and those are read-only elsewhere
// while declared.
type Config struct {
	Server     Server     `mapstructure:"server"`
	Bootstrap  Bootstrap  `mapstructure:"bootstrap"`
	Database   Database   `mapstructure:"database"`
	Log        Log        `mapstructure:"log"`
	Reconciler Reconciler `mapstructure:"reconciler"`
	Apps       Apps       `mapstructure:"apps"`
	Work       Work       `mapstructure:"work"`
	Retention  Retention  `mapstructure:"retention"`
	Registry   Registry   `mapstructure:"registry"`

	// File is the config file read at startup, or empty when there was none.
	File string `mapstructure:"-"`

	// Settings is every non-secret setting with its effective value and where
	// it came from, for GET /config. Policy is the host policy fields set at
	// startup, which override the stored policy (see sources.go).
	Settings []Setting       `mapstructure:"-"`
	Policy   []PolicySetting `mapstructure:"-"`

	// Adapters are the adapters declared in the config file, with the AI
	// functions each handles (adapters.go).
	Adapters []AdapterDecl `mapstructure:"-"`
}

// Reconciler tunes R-149's retry backoff and R-150's give-up rule.
//
// Present because the acceptance test for R-151 has to wait out the real
// schedule, and at the shipped numbers that is forty minutes. Leave these unset
// in production: the defaults are the requirement, and Pando says so at startup
// if they are set faster.
type Reconciler struct {
	// Backoff is the retry schedule, indexed by consecutive failures, as a
	// comma-separated list of durations: "0s,5s,15s,60s,5m".
	Backoff string `mapstructure:"backoff"`

	// FailureThreshold is how many failures inside FailureWindow before an app
	// is given up on and left failed (R-150).
	FailureThreshold int `mapstructure:"failure_threshold"`

	// FailureWindow is measured from the last failure, so a slow crash loop
	// still reaches the threshold rather than resetting forever.
	FailureWindow time.Duration `mapstructure:"failure_window"`

	// GCInterval is how often garbage collection runs. Zero means the shipped
	// hour. Short intervals are wasteful rather than dangerous — nothing the
	// collector does is urgent — so unlike the backoff this gets no warning.
	GCInterval time.Duration `mapstructure:"gc_interval"`
}

// Work bounds the background work one replica does at once (issue #72). Zero
// means the shipped default for each.
type Work struct {
	// Deploys is how many deploys this replica runs at once from the deploy
	// queue. Default: one per CPU, at least two.
	Deploys int `mapstructure:"deploys"`

	// Detections is how many detections this replica runs at once. Default:
	// one per CPU, at least two.
	Detections int `mapstructure:"detections"`

	// Backups is how many rolling backups the leader takes at once.
	// Default: two.
	Backups int `mapstructure:"backups"`

	// AutoDeploy is how many apps the leader checks for new commits at
	// once. Default: eight.
	AutoDeploy int `mapstructure:"auto_deploy"`
}

// Retention is how long the retention job keeps each kind of row it removes
// (issue #72, R-224). Zero means the shipped default for each, listed in
// retention.Settings: 50 deploys per app, scans 90 days, expired or revoked
// sessions 30 days, idempotency keys a day, expired sign-in flows a day, the
// event outbox 30 days, a deleted app's detection and backup record 30 days.
type Retention struct {
	DeploymentsPerApp int           `mapstructure:"deployments_per_app"`
	Scans             time.Duration `mapstructure:"scans"`
	Sessions          time.Duration `mapstructure:"sessions"`
	IdempotencyKeys   time.Duration `mapstructure:"idempotency_keys"`
	SSOFlows          time.Duration `mapstructure:"sso_flows"`
	Events            time.Duration `mapstructure:"events"`
	DeletedApps       time.Duration `mapstructure:"deleted_apps"`
}

// Registry is the install's image registry, where builds go for a runtime
// that pulls rather than imports (issue #72, PR 5,
// docs/design/notes-image-registry-issue-72.md). Startup configuration only,
// like the database URL. Empty URL: no registry, which single-host Docker does
// not need (O-34).
type Registry struct {
	// URL is the registry and an optional path prefix:
	// https://registry.internal:5000, or the organization's registry such as
	// 123456789012.dkr.ecr.us-east-1.amazonaws.com/pando.
	URL string `mapstructure:"url"`

	// Username and Password are the one credential Pando pushes and pulls
	// with (O-36). The password may be given as a file instead
	// (PasswordFile), so it need not be in the environment.
	Username     string `mapstructure:"username"`
	Password     string `mapstructure:"password"`
	PasswordFile string `mapstructure:"password_file"`

	// Kind is basic (the default) or ecr, where Username and Password are an
	// AWS access key ID and its secret.
	Kind string `mapstructure:"kind"`

	// Layout is per_app (the default) or single.
	Layout string `mapstructure:"layout"`

	// Insecure permits plain HTTP (O-35). Off by default.
	Insecure bool `mapstructure:"insecure"`

	// Always sends every build through the registry, even for a runtime that
	// can import it. Off by default.
	Always bool `mapstructure:"always"`
}

// Secret is the registry password: PasswordFile's contents when it is set,
// with surrounding whitespace removed, and Password otherwise.
func (r Registry) Secret() (string, error) {
	if r.PasswordFile == "" {
		return r.Password, nil
	}
	raw, err := os.ReadFile(r.PasswordFile)
	if err != nil {
		return "", fmt.Errorf("PANDO_REGISTRY_PASSWORD_FILE is %q, which Pando could not read: %w", r.PasswordFile, err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// Apps holds the resource limits every new app inherits (R-240).
//
// R-240 says these are set at the host. They were constants: one CPU, 512 MiB
// and 10 GiB for every app, with nothing that could change them, so a 12-CPU
// host refused its thirteenth app however idle the first twelve were (issue
// #55). Zero means the shipped default (spec.StandardDefaults); an app still
// overrides its own with app.resources.override (R-241).
type Apps struct {
	CPUMillis   int   `mapstructure:"cpu_millis"`
	MemoryBytes int64 `mapstructure:"memory_bytes"`
	DiskBytes   int64 `mapstructure:"disk_bytes"`

	// DockerCredentials pulls a private image with the Docker login on the
	// Pando server — `docker login`, the config.json it writes and any
	// credential helper it names, found through DOCKER_CONFIG or
	// ~/.docker — when the app has no registry credential of its own (issue
	// #41). Off by default: it lends every app on the install whatever the
	// server's login can read, so it is an operator's decision, made where
	// only an operator can make it.
	DockerCredentials bool `mapstructure:"docker_credentials"`
}

// Resources lays the configured limits over the shipped ones.
func (a Apps) Resources(shipped spec.Resources) spec.Resources {
	if a.CPUMillis > 0 {
		shipped.CPUMillis = a.CPUMillis
	}
	if a.MemoryBytes > 0 {
		shipped.MemoryBytes = a.MemoryBytes
	}
	if a.DiskBytes > 0 {
		shipped.DiskBytes = a.DiskBytes
	}
	return shipped
}

// BackoffSchedule parses Backoff, returning nil when it is unset.
func (r Reconciler) BackoffSchedule() ([]time.Duration, error) {
	if strings.TrimSpace(r.Backoff) == "" {
		return nil, nil
	}

	var out []time.Duration
	for _, part := range strings.Split(r.Backoff, ",") {
		d, err := time.ParseDuration(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("reconciler.backoff: %q is not a duration like 5s or 2m: %w", part, err)
		}
		if d < 0 {
			return nil, fmt.Errorf("reconciler.backoff: %q is negative", part)
		}
		out = append(out, d)
	}
	return out, nil
}

// Bootstrap is what first run needs, and is read on no other run.
type Bootstrap struct {
	// AdminPassword is the first administrator's initial password, empty to
	// have the first person to reach the console set the administrator up
	// there (R-046).
	//
	// The one credential in this struct, and the reason nothing logs a Config
	// wholesale. It exists because the generated password is shown once, in a
	// log line, and a server container recreated before anyone read it leaves
	// an account nobody can sign in to.
	//
	// Ignored once the install has any account. It is a bootstrap input, not a
	// way to set a password: the account must still change it at first sign-in,
	// because an environment variable is in the Compose file, in
	// `docker inspect`, and inherited by every child process.
	AdminPassword string `mapstructure:"admin_password"`
}

type Server struct {
	Addr            string        `mapstructure:"addr"`
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`

	// ProxyUpstream is the address routing adapters are told to send traffic
	// to (R-023). It must be reachable from wherever an adapter's data plane
	// runs, which is not necessarily where Pando runs.
	ProxyUpstream string `mapstructure:"proxy_upstream"`

	// Issuer is the `iss` claim in every assertion, and identifies this
	// install to the apps it fronts.
	Issuer string `mapstructure:"issuer"`

	// ExternalURL is how a browser reaches this installation, and exists
	// because Pando usually does not terminate its own TLS (O-19).
	//
	// Behind a TLS-terminating reverse proxy — the topology SECURITY.md
	// describes — every request arrives over plain HTTP, so `r.TLS` is nil and
	// Pando cannot tell an encrypted browser connection from an unencrypted
	// one. Session cookies then go out without `Secure`, and a single plaintext
	// request to the hostname puts one on the wire.
	//
	// The operator states the scheme rather than Pando guessing it from a
	// header. `X-Forwarded-Proto` is the usual answer and is rejected here for
	// the same reason R-053 strips inbound `X-Pando-*`: any client that can
	// reach Pando directly can also set it, so trusting it without a
	// trusted-proxy list trades one hole for another, and with one it becomes a
	// second thing to get wrong.
	//
	// Empty is correct for the two topologies where the request already tells
	// the truth: Pando serving TLS itself, and the plain-HTTP localhost install
	// the README documents.
	ExternalURL string `mapstructure:"external_url"`

	// RoutingMode is how apps are addressed — "subdomain" or "path" (design 03
	// §4.1). Neither is a global setting in the spec sense; this is the
	// install's default shape, and each app's spec still names its own mode.
	RoutingMode spec.RoutingMode `mapstructure:"routing_mode"`

	// BaseDomain is what a per-app subdomain is carved out of, so an app
	// called "notes" is reached at notes.example.com. Only used in subdomain
	// mode; empty is correct for an install addressing apps by path, which is
	// the default and needs no DNS at all (R-002 — setup cost is paid once).
	BaseDomain string `mapstructure:"base_domain"`

	// PortRangeStart and PortRangeEnd bound the ports handed to apps in
	// port-mode routing — the loopback adapter's only mode, and so the laptop
	// default. [P]: the requirements do not specify a range (O-15).
	PortRangeStart int `mapstructure:"port_range_start"`
	PortRangeEnd   int `mapstructure:"port_range_end"`

	// WorkDir is where a DR bundle is assembled before it is encrypted and
	// streamed out (R-212). It needs room for a database dump plus every app
	// volume, which is why it is configurable and does not default to the
	// system temporary directory — that is often a small tmpfs, and running out
	// of space partway through a backup is how an install discovers it has no
	// backups.
	WorkDir string `mapstructure:"work_dir"`

	// AuditArchiveDir is where Pando keeps audit archives itself, when host
	// policy's audit_archive is keep, the default (R-347). Beside the rest
	// of Pando's data, on the same volume, so the archives are on the disk
	// R-224 is about rather than somewhere nobody counts.
	AuditArchiveDir string `mapstructure:"audit_archive_dir"`

	// TokenKeyPath is the file holding the key API tokens are stored under,
	// as HMAC-SHA-256 (R-063). Generated on first start. Outside the database
	// so a dump alone cannot test a guess at a token; every replica must read
	// the same file, which is why it defaults to the shared /var/lib/pando
	// beside the secrets key, and a DR bundle carries it (R-212).
	TokenKeyPath string `mapstructure:"token_key_path"`

	// AdvertiseURL is where the other Pando replicas reach this one, for the
	// one request that has to go to a particular replica: a deploy's live log
	// (issue #72). Empty means http://<hostname><port of addr>, which is right
	// on a Compose network; in Kubernetes, set it to the pod's address, such
	// as http://$(POD_IP):8080. With one replica nothing reads it.
	AdvertiseURL string `mapstructure:"advertise_url"`
}

// Advertise is AdvertiseURL, or the default built from hostname.
func (s Server) Advertise(hostname string) string {
	if s.AdvertiseURL != "" {
		return strings.TrimSuffix(s.AdvertiseURL, "/")
	}
	port := s.Addr
	if i := strings.LastIndex(port, ":"); i >= 0 {
		port = port[i:]
	} else {
		port = ""
	}
	return "http://" + hostname + port
}

type Database struct {
	// URL is the connection string. The bundled Compose file supplies it; an
	// operator pointing Pando at an existing Postgres sets PANDO_DATABASE_URL.
	//
	// Whatever it names, the account must be able to manage Pando's restricted
	// application role — see design 00 §1.1. Pando verifies this at startup and
	// refuses to run if the audit log would be rewritable.
	URL            string        `mapstructure:"url"`
	ConnectTimeout time.Duration `mapstructure:"connect_timeout"`

	// MaxConns caps this replica's pool of connections as the application
	// role. pgx's own default is the CPU count, at least four, which a
	// reconciler holding a connection per app it is converging, plus the
	// leader's held lock, could use up on a small host and then wait on
	// forever (issue #72). Postgres's max_connections must allow MaxConns
	// for every replica, plus a few for each one's bootstrap.
	MaxConns int32 `mapstructure:"max_conns"`
}

type Log struct {
	Level       string `mapstructure:"level"`
	Development bool   `mapstructure:"development"`
}

// Load reads configuration. Precedence, lowest to highest: defaults, config
// file, environment, flags.
func Load(path string) (*Config, error) {
	v := viper.New()

	v.SetDefault("server.addr", ":8080")
	v.SetDefault("server.shutdown_timeout", 15*time.Second)
	v.SetDefault("database.connect_timeout", 60*time.Second)
	v.SetDefault("database.max_conns", 32)
	v.SetDefault("server.issuer", "https://pando.local")
	v.SetDefault("server.routing_mode", string(spec.RoutingPath))
	v.SetDefault("server.port_range_start", 9000)
	v.SetDefault("server.port_range_end", 9999)
	v.SetDefault("server.work_dir", "/var/lib/pando/work")
	v.SetDefault("server.audit_archive_dir", "/var/lib/pando/audit-archives")
	v.SetDefault("server.token_key_path", tokenkey.DefaultPath)
	v.SetDefault("log.level", "info")
	v.SetDefault("log.development", false)

	v.SetEnvPrefix("PANDO")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// Registering every key with a zero default, so Unmarshal can see it.
	//
	// This is the other half of the same viper trap as the binds below. BindEnv
	// makes a key readable through Get; it does not necessarily put the key in
	// the settings map that Unmarshal walks. A key with neither a default nor a
	// config-file entry can therefore be bound, be present in the environment,
	// and still arrive as the zero value — silently, which is the part that
	// costs an afternoon.
	//
	// A zero default is not a value: it is how the key gets registered.
	v.SetDefault("server.base_domain", "")
	v.SetDefault("server.external_url", "")
	v.SetDefault("server.advertise_url", "")
	v.SetDefault("reconciler.backoff", "")
	v.SetDefault("reconciler.failure_threshold", 0)
	v.SetDefault("reconciler.failure_window", time.Duration(0))
	v.SetDefault("reconciler.gc_interval", time.Duration(0))
	v.SetDefault("apps.cpu_millis", 0)
	v.SetDefault("apps.memory_bytes", int64(0))
	v.SetDefault("apps.disk_bytes", int64(0))
	v.SetDefault("apps.docker_credentials", false)
	v.SetDefault("work.deploys", 0)
	v.SetDefault("work.detections", 0)
	v.SetDefault("work.backups", 0)
	v.SetDefault("work.auto_deploy", 0)
	v.SetDefault("retention.deployments_per_app", 0)
	v.SetDefault("retention.scans", time.Duration(0))
	v.SetDefault("retention.sessions", time.Duration(0))
	v.SetDefault("retention.idempotency_keys", time.Duration(0))
	v.SetDefault("retention.sso_flows", time.Duration(0))
	v.SetDefault("retention.events", time.Duration(0))
	v.SetDefault("retention.deleted_apps", time.Duration(0))
	v.SetDefault("bootstrap.admin_password", "")
	v.SetDefault("registry.url", "")
	v.SetDefault("registry.username", "")
	v.SetDefault("registry.password", "")
	v.SetDefault("registry.password_file", "")
	v.SetDefault("registry.kind", "basic")
	v.SetDefault("registry.layout", "per_app")
	v.SetDefault("registry.insecure", false)
	v.SetDefault("registry.always", false)

	// Every key in boundEnv is bound explicitly — see there for why that is not
	// belt-and-braces.
	for key, env := range boundEnv {
		_ = v.BindEnv(key, env)
	}

	if path != "" {
		v.SetConfigFile(path)
		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("reading config file %s: %w", path, err)
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("parsing configuration: %w", err)
	}
	cfg.File = path
	cfg.Settings = settingsOf(v, path)
	cfg.Policy = policyOf(v, path)
	adapters, err := adaptersOf(v, path)
	if err != nil {
		return nil, err
	}
	cfg.Adapters = adapters
	return &cfg, cfg.validate()
}

// boundEnv is every key read from an explicitly named variable.
//
// AutomaticEnv looks up an environment variable only for keys viper already
// knows — from a default, a config file, or a bind. A key with none of those is
// invisible to Unmarshal, so `PANDO_SERVER_BASE_DOMAIN=…` sets nothing and says
// nothing: the process starts, the setting is empty, and the failure surfaces
// much later as an app with no hostname. That happened to base_domain. A key
// that is only ever set from the environment therefore has to be listed here.
//
// Also where sources.go learns which variable a key came from.
var boundEnv = map[string]string{
	"database.url":          "PANDO_DATABASE_URL",
	"database.max_conns":    "PANDO_DATABASE_MAX_CONNS",
	"server.base_domain":    "PANDO_SERVER_BASE_DOMAIN",
	"server.proxy_upstream": "PANDO_SERVER_PROXY_UPSTREAM",
	"server.issuer":         "PANDO_SERVER_ISSUER",
	"server.external_url":   "PANDO_SERVER_EXTERNAL_URL",
	"server.addr":           "PANDO_SERVER_ADDR",
	"server.routing_mode":   "PANDO_SERVER_ROUTING_MODE",
	"server.work_dir":       "PANDO_SERVER_WORK_DIR",
	"server.advertise_url":  "PANDO_SERVER_ADVERTISE_URL",

	"server.audit_archive_dir": "PANDO_SERVER_AUDIT_ARCHIVE_DIR",
	"server.token_key_path":    "PANDO_SERVER_TOKEN_KEY_PATH",
	"log.level":                "PANDO_LOG_LEVEL",

	"apps.docker_credentials": "PANDO_APPS_DOCKER_CREDENTIALS",

	"registry.url":           "PANDO_REGISTRY_URL",
	"registry.username":      "PANDO_REGISTRY_USERNAME",
	"registry.password":      "PANDO_REGISTRY_PASSWORD",
	"registry.password_file": "PANDO_REGISTRY_PASSWORD_FILE",
	"registry.kind":          "PANDO_REGISTRY_KIND",
	"registry.layout":        "PANDO_REGISTRY_LAYOUT",
	"registry.insecure":      "PANDO_REGISTRY_INSECURE",
	"registry.always":        "PANDO_REGISTRY_ALWAYS",

	// Not PANDO_BOOTSTRAP_ADMIN_PASSWORD, which is what the replacer would
	// derive — this is the one setting an operator types from memory at the
	// worst possible moment. That makes this bind load-bearing rather than
	// belt-and-braces: AutomaticEnv only finds the derived name, so without the
	// line below the variable is read by nothing at all.
	"bootstrap.admin_password": "PANDO_ADMIN_PASSWORD",
}

func (c *Config) validate() error {
	if c.Database.URL == "" {
		return fmt.Errorf("no database URL configured: set PANDO_DATABASE_URL, or database.url in the config file")
	}
	if _, err := c.Server.External(); err != nil {
		return err
	}
	return nil
}

// External parses ExternalURL, returning nil when it is unset.
//
// Checked at startup rather than at the first sign-in, because the symptom of
// getting it wrong is a cookie attribute nobody looks at until it matters.
func (s Server) External() (*url.URL, error) {
	if strings.TrimSpace(s.ExternalURL) == "" {
		return nil, nil
	}

	u, err := url.Parse(strings.TrimSpace(s.ExternalURL))
	if err != nil {
		return nil, fmt.Errorf(
			"PANDO_SERVER_EXTERNAL_URL is not a URL: %q. "+
				"Valid answer: the address browsers use to reach Pando, such as https://pando.example.com", s.ExternalURL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf(
			"PANDO_SERVER_EXTERNAL_URL must start with http:// or https://, got %q. "+
				"The scheme is the whole point of this setting: it is how Pando knows whether to mark "+
				"session cookies Secure when something else terminates TLS", s.ExternalURL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf(
			"PANDO_SERVER_EXTERNAL_URL has no host: %q. "+
				"Valid answer: the address browsers use to reach Pando, such as https://pando.example.com", s.ExternalURL)
	}
	return u, nil
}
