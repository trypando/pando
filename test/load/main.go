// Command load seeds a Pando install to one of the issue #72 scale tiers,
// drives console, API and proxy traffic through its load balancer in steps,
// and reports where it stopped keeping up.
//
//	go run ./test/load seed    -tier vm -db postgres://… -url http://localhost:28080
//	go run ./test/load run     -tier vm -db postgres://… -url http://localhost:28080 -out results.json
//	go run ./test/load report  -in results.json -out report.md
//	go run ./test/load cleanup -db postgres://… -url http://localhost:28080
//
// `make load-test TIER=vm` does all four against a stack of its own. See
// test/load/README.md.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "seed":
		err = seedCmd(ctx, os.Args[2:])
	case "run":
		err = runCmd(ctx, os.Args[2:])
	case "report":
		err = reportCmd(os.Args[2:])
	case "cleanup":
		err = cleanupCmd(ctx, os.Args[2:])
	case "-h", "-help", "--help", "help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: go run ./test/load <seed|run|report|cleanup> [flags]

  seed     write a tier's users, groups, apps, grants and tokens into Postgres,
           and deploy its real apps through the API
  run      ramp console, API and proxy load through the balancer; write results JSON
  report   render one or more results files as Markdown
  cleanup  delete the real apps through the API and wait for their containers to go

Run any subcommand with -h for its flags.`)
}

// Flags shared by seed, run and cleanup.
type common struct {
	tier          string
	db            string
	url           string
	adminUser     string
	adminPassword string
	setupToken    string
	userPassword  string
	tokenSecret   string
	// Overrides of the tier's sizes; zero keeps the tier's.
	users, apps, groups, admins, tokens, realApps, consoleUsers int
	apiRate, proxyRate                                          float64
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.tier, "tier", "vm", "scale tier: vm or cluster")
	fs.StringVar(&c.db, "db", os.Getenv("LOAD_DATABASE_URL"), "the install's database, as its owner (default $LOAD_DATABASE_URL)")
	fs.StringVar(&c.url, "url", "http://localhost:28080", "the load balancer in front of Pando")
	fs.StringVar(&c.adminUser, "admin-user", "admin", "the first administrator's username")
	fs.StringVar(&c.adminPassword, "admin-password", os.Getenv("LOAD_ADMIN_PASSWORD"), "the first administrator's password; claims setup with it on a fresh install (default $LOAD_ADMIN_PASSWORD)")
	fs.StringVar(&c.setupToken, "setup-token", os.Getenv("LOAD_SETUP_TOKEN"), "the setup token from `pando admin setup-token`, for a fresh install (default $LOAD_SETUP_TOKEN)")
	fs.StringVar(&c.userPassword, "user-password", "pando-load-user-password", "the password every seeded user signs in with")
	fs.StringVar(&c.tokenSecret, "token-secret", "pando-load-token-secret", "the secret half of every seeded API token")
	fs.IntVar(&c.users, "users", 0, "override the tier's user count")
	fs.IntVar(&c.apps, "apps", 0, "override the tier's app count")
	fs.IntVar(&c.groups, "groups", 0, "override the tier's group count")
	fs.IntVar(&c.admins, "admins", 0, "override the tier's administrator count")
	fs.IntVar(&c.tokens, "tokens", 0, "override the tier's API token count")
	fs.IntVar(&c.realApps, "real-apps", -1, "override how many real apps (containers) to deploy; 0 deploys none")
	fs.IntVar(&c.consoleUsers, "console-users", 0, "override the peak number of console users")
	fs.Float64Var(&c.apiRate, "api-rate", 0, "override the peak API request rate (req/s)")
	fs.Float64Var(&c.proxyRate, "proxy-rate", 0, "override the peak proxy request rate (req/s)")
}

func (c *common) resolve() (Tier, error) {
	t, err := TierByName(c.tier)
	if err != nil {
		return Tier{}, err
	}
	setInt := func(dst *int, v int) {
		if v > 0 {
			*dst = v
		}
	}
	setInt(&t.Users, c.users)
	setInt(&t.Apps, c.apps)
	setInt(&t.Groups, c.groups)
	setInt(&t.Admins, c.admins)
	setInt(&t.Tokens, c.tokens)
	setInt(&t.ConsoleUsers, c.consoleUsers)
	if c.realApps >= 0 {
		t.RealApps = c.realApps
	}
	if c.apiRate > 0 {
		t.APIRate = c.apiRate
	}
	if c.proxyRate > 0 {
		t.ProxyRate = c.proxyRate
	}
	if c.db == "" {
		return Tier{}, fmt.Errorf("-db is required: the install's database URL, as its owner")
	}
	return t, t.Validate()
}

func seedCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	var c common
	c.register(fs)
	baseDomain := fs.String("base-domain", "localtest.me", "the install's PANDO_SERVER_BASE_DOMAIN; seeded hostnames are <slug>.<base-domain>")
	portStart := fs.Int("real-port-start", 29000, "the first port real apps are given; must be in the install's port range")
	batch := fs.Int("batch", 5000, "rows per transaction")
	_ = fs.Parse(args)
	t, err := c.resolve()
	if err != nil {
		return err
	}
	if c.adminPassword == "" {
		return fmt.Errorf("-admin-password is required")
	}
	return Seed(ctx, SeedOptions{
		Tier: t, DatabaseURL: c.db, BaseURL: c.url, BaseDomain: *baseDomain,
		AdminUser: c.adminUser, AdminPassword: c.adminPassword, SetupToken: c.setupToken,
		UserPassword: c.userPassword, TokenSecret: c.tokenSecret,
		RealPortStart: *portStart, Batch: *batch,
	})
}

func runCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var c common
	c.register(fs)
	hold := fs.Duration("hold", 3*time.Minute, "how long each step holds once its users are online")
	steps := fs.String("steps", "", "comma-separated shares of the peak, one per step (default 0.1,0.25,0.5,0.75,1)")
	out := fs.String("out", "load-results.json", "where to write the results JSON")
	replicas := fs.Int("replicas", 0, "how many Pando replicas the stack runs, for the report")
	commit := fs.String("commit", "", "the commit under test, for the report")
	timeout := fs.Duration("timeout", 30*time.Second, "how long one request may take before it counts as an error")
	signIn := fs.Float64("sign-in-rate", 20, "sign-ins per second while users come online")
	navigate := fs.Duration("navigate", 3*time.Minute, "how often a launcher user or administrator moves between screens, refetching lists")
	workers := fs.Int("workers", 512, "concurrent requests per open-loop surface (API, proxy)")
	maxErr := fs.Float64("max-error-rate", DefaultThresholds.MaxErrorRate, "error rate above the first step's at which a class has broken")
	p95 := fs.Duration("p95", DefaultThresholds.P95, "p95 latency at which a class has broken")
	keepGoing := fs.Bool("keep-going", false, "run every step even after one breaks")
	_ = fs.Parse(args)
	t, err := c.resolve()
	if err != nil {
		return err
	}
	fractions, err := parseFractions(*steps)
	if err != nil {
		return err
	}
	return Run(ctx, RunOptions{
		Tier: t, Steps: Ramp(t, fractions, *hold), BaseURL: c.url, DatabaseURL: c.db,
		UserPassword: c.userPassword, TokenSecret: c.tokenSecret, Replicas: *replicas, Commit: *commit,
		Out: *out, Timeout: *timeout, SignInRate: *signIn, Navigate: *navigate, Workers: *workers,
		Thresholds: Thresholds{MaxErrorRate: *maxErr, P95: *p95, MinRequests: DefaultThresholds.MinRequests},
		KeepGoing:  *keepGoing,
	})
}

func parseFractions(s string) ([]float64, error) {
	if strings.TrimSpace(s) == "" {
		return DefaultFractions, nil
	}
	var out []float64
	for _, part := range strings.Split(s, ",") {
		f, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil || f <= 0 || f > 10 {
			return nil, fmt.Errorf("-steps: %q is not a share of the peak such as 0.5", part)
		}
		out = append(out, f)
	}
	return out, nil
}

func reportCmd(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	in := fs.String("in", "load-results.json", "comma-separated results files, one per run")
	out := fs.String("out", "", "where to write the Markdown (default stdout)")
	_ = fs.Parse(args)
	var runs []Results
	for _, path := range strings.Split(*in, ",") {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var r Results
		if err := json.Unmarshal(b, &r); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		runs = append(runs, r)
	}
	w := os.Stdout
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		w = f
	}
	return WriteReport(w, runs)
}

func cleanupCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("cleanup", flag.ExitOnError)
	var c common
	c.register(fs)
	within := fs.Duration("within", 3*time.Minute, "how long to wait for Pando to remove the real apps' containers")
	idsFile := fs.String("real-apps-file", "", "write every real app's ID here, one per line, before deleting them")
	_ = fs.Parse(args)
	if c.db == "" {
		return fmt.Errorf("-db is required")
	}
	return Cleanup(ctx, c.url, c.db, c.adminUser, c.adminPassword, *idsFile, *within)
}
