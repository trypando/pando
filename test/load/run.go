package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RunOptions are the run subcommand's inputs.
type RunOptions struct {
	Tier         Tier
	Steps        []Step
	BaseURL      string
	DatabaseURL  string
	UserPassword string
	TokenSecret  string
	Replicas     int
	Commit       string
	Out          string
	Timeout      time.Duration
	SignInRate   float64
	Navigate     time.Duration
	Workers      int
	Thresholds   Thresholds
	KeepGoing    bool
}

// The console's polling intervals, from console/src. If the console changes
// one, change it here, or the run stops resembling the console.
const (
	pollStatus    = 5 * time.Second  // admin/Parts.tsx: GET /apps/{id}/status
	pollUsage     = 10 * time.Second // admin/Usage.tsx: GET /apps/{id}/usage
	pollApprovals = 30 * time.Second // admin/Approvals.tsx: GET /approvals, in the admin console
	pollInbox     = 60 * time.Second // ui/Inbox.tsx: GET /me/notifications, on every screen
)

// Persona is what one simulated console user is doing.
type Persona string

const (
	// PersonaLauncher has the launcher open: the apps they can use.
	PersonaLauncher Persona = "launcher"
	// PersonaOwner has one of their apps open in the admin console, on its
	// overview, which polls the app's status and usage.
	PersonaOwner Persona = "owner"
	// PersonaAdmin administers the install: the app list, accounts and
	// approvals, with one app open.
	PersonaAdmin Persona = "admin"
)

type wsUser struct {
	ID    string
	Name  string
	Admin bool
	Owns  []string
}

type wsToken struct {
	Bearer string
	Owns   []string
}

type wsApp struct {
	ID, Slug, Hostname, Path string
}

type wsReal struct {
	ID, Slug  string
	Port      int
	Anonymous bool
	Members   map[string]bool
}

// workingSet is the seeded install as the run sees it, read back from the
// database so the run never has to agree with the seed about anything but
// the name prefixes.
type workingSet struct {
	Users  []wsUser
	Tokens []wsToken
	Seeded []wsApp
	Real   []wsReal
}

func loadWorkingSet(ctx context.Context, pool *pgxpool.Pool, tokenSecret string) (*workingSet, error) {
	ws := &workingSet{}
	owns := map[string][]string{}
	rows, err := pool.Query(ctx, `
		SELECT id, slug, owner_user_id, coalesce(address_hostname, ''), coalesce(address_path, '')
		FROM apps WHERE slug LIKE $1 AND deleted_at IS NULL ORDER BY slug`, appPrefix+"%")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a wsApp
		var owner *string
		if err := rows.Scan(&a.ID, &a.Slug, &owner, &a.Hostname, &a.Path); err != nil {
			rows.Close()
			return nil, err
		}
		ws.Seeded = append(ws.Seeded, a)
		if owner != nil {
			owns[*owner] = append(owns[*owner], a.ID)
		}
	}
	rows.Close()

	admins := map[string]bool{}
	rows, err = pool.Query(ctx, `
		SELECT principal_id FROM grants
		WHERE role_scope = 'install' AND role_id = 'role_administrator' AND principal_kind = 'user'`)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		admins[id] = true
	}

	rows, err = pool.Query(ctx, `
		SELECT id, external_id FROM users
		WHERE adapter_id = $1 AND external_id LIKE $2 AND deleted_at IS NULL AND status = 'active'
		ORDER BY external_id`, localAdapterID, userPrefix+"%")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var u wsUser
		if err := rows.Scan(&u.ID, &u.Name); err != nil {
			rows.Close()
			return nil, err
		}
		u.Admin = admins[u.ID]
		u.Owns = owns[u.ID]
		ws.Users = append(ws.Users, u)
	}
	rows.Close()

	rows, err = pool.Query(ctx, `
		SELECT id, owner_user_id FROM tokens WHERE name LIKE $1 AND revoked_at IS NULL ORDER BY name`, tokenPrefix+"%")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, owner string
		if err := rows.Scan(&id, &owner); err != nil {
			rows.Close()
			return nil, err
		}
		ws.Tokens = append(ws.Tokens, wsToken{Bearer: id + "." + tokenSecret, Owns: owns[owner]})
	}
	rows.Close()

	rows, err = pool.Query(ctx, `
		SELECT a.id, a.slug, coalesce((r.body->'routing'->>'port')::int, 0),
		       EXISTS (SELECT 1 FROM grants g WHERE g.app_id = a.id AND g.plane = 'data' AND g.principal_kind = 'anonymous')
		FROM apps a JOIN spec_revisions r ON r.id = a.pinned_spec_id
		WHERE a.name LIKE $1 AND a.deleted_at IS NULL AND a.state IN ('running', 'degraded')
		ORDER BY a.name`, realPrefix+"%")
	if err != nil {
		return nil, err
	}
	byID := map[string]int{}
	for rows.Next() {
		var a wsReal
		if err := rows.Scan(&a.ID, &a.Slug, &a.Port, &a.Anonymous); err != nil {
			rows.Close()
			return nil, err
		}
		a.Members = map[string]bool{}
		byID[a.ID] = len(ws.Real)
		ws.Real = append(ws.Real, a)
	}
	rows.Close()

	// Who may use each real app: its group's members, directly granted
	// users, and its owner. Read from the grants rather than assumed, so a
	// request's expected answer is the one Pando should give.
	rows, err = pool.Query(ctx, `
		SELECT g.app_id, coalesce(gm.user_id, g.principal_id)
		FROM grants g
		LEFT JOIN group_members gm ON g.principal_kind = 'group' AND gm.group_id = g.principal_id
		JOIN apps a ON a.id = g.app_id
		WHERE g.plane = 'data' AND g.principal_kind IN ('user', 'group')
		  AND a.name LIKE $1 AND a.deleted_at IS NULL`, realPrefix+"%")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var app string
		var user *string
		if err := rows.Scan(&app, &user); err != nil {
			rows.Close()
			return nil, err
		}
		if i, ok := byID[app]; ok && user != nil {
			ws.Real[i].Members[*user] = true
		}
	}
	rows.Close()
	return ws, rows.Err()
}

// personaOf decides what an online user does: an administrator administers,
// someone who owns an app watches it, and everyone else has the launcher.
func personaOf(u wsUser) Persona {
	switch {
	case u.Admin:
		return PersonaAdmin
	case len(u.Owns) > 0:
		return PersonaOwner
	default:
		return PersonaLauncher
	}
}

// onlineOrder is the order users come online: administrators first, since
// there are few and every install has them online, then everyone else in a
// fixed shuffle so each step adds a mix of personas.
func onlineOrder(users []wsUser) []int {
	var admins, rest []int
	for i, u := range users {
		if u.Admin {
			admins = append(admins, i)
		} else {
			rest = append(rest, i)
		}
	}
	r := rand.New(rand.NewPCG(72, 72))
	r.Shuffle(len(rest), func(i, j int) { rest[i], rest[j] = rest[j], rest[i] })
	return append(admins, rest...)
}

// runner drives one run.
type runner struct {
	o   RunOptions
	c   *Client
	ws  *workingSet
	rec *Recorder

	// Signed-in console users, by user ID, for proxy requests made as a
	// person who has a session: the proxy's signed-in path.
	mu       sync.RWMutex
	sessions map[string]string
	online   []string

	dropped        sync.Map // class -> *atomic.Uint64
	signInFailures atomic.Int64
}

// Run ramps load through the steps and writes the results.
func Run(ctx context.Context, o RunOptions) error {
	pg, err := NewPG(ctx, o.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", redactURL(o.DatabaseURL), err)
	}
	defer pg.Close()

	ws, err := loadWorkingSet(ctx, pg.pool, o.TokenSecret)
	if err != nil {
		return fmt.Errorf("reading the seeded install: %w", err)
	}
	if len(ws.Users) == 0 || len(ws.Seeded) == 0 {
		return fmt.Errorf("the database has no seeded users or apps; run `seed` first")
	}
	log.Printf("working set: %d users, %d seeded apps, %d tokens, %d real apps",
		len(ws.Users), len(ws.Seeded), len(ws.Tokens), len(ws.Real))

	r := &runner{o: o, c: NewClient(o.BaseURL, o.Timeout), ws: ws, rec: NewRecorder(), sessions: map[string]string{}}
	if err := r.c.AwaitHealthy(ctx, time.Minute); err != nil {
		return err
	}

	res := Results{
		Tier: o.Tier, Replicas: o.Replicas, BaseURL: r.c.Root, Commit: o.Commit,
		StartedAt: time.Now().UTC(), Thresholds: o.Thresholds, Statements: pg.Statements,
		Seeded: pg.Counts(ctx),
	}

	usersCtx, stopUsers := context.WithCancel(ctx)
	defer stopUsers()
	order := onlineOrder(ws.Users)
	launched := 0

	for _, step := range o.Steps {
		if ctx.Err() != nil {
			break
		}
		log.Printf("step %d: %d console users, %.1f API req/s, %.1f proxy req/s",
			step.Index+1, step.ConsoleUsers, step.APIRate, step.ProxyRate)
		stepCtx, stopStep := context.WithCancel(ctx)
		go paced(stepCtx, step.APIRate, o.Workers, r.apiRequest, func() { r.drop("api") })
		go paced(stepCtx, step.ProxyRate, o.Workers, r.proxyRequest, func() { r.drop("proxy") })

		// Bring users online, at the sign-in rate, and wait for each to have
		// signed in (or failed to) before the step holds.
		target := min(step.ConsoleUsers, len(order))
		var signedIn sync.WaitGroup
		interval := time.Duration(float64(time.Second) / max(o.SignInRate, 0.1))
		for ; launched < target && ctx.Err() == nil; launched++ {
			signedIn.Add(1)
			u := ws.Users[order[launched]]
			go r.consoleUser(usersCtx, u, signedIn.Done)
			time.Sleep(interval)
		}
		signedIn.Wait()
		r.settle(ctx, 10*time.Second)

		pg.Begin(ctx)
		watchCtx, stopWatch := context.WithCancel(ctx)
		go pg.Watch(watchCtx, 5*time.Second)
		r.dropped.Clear()
		r.rec.Hold()
		held := time.Now()
		r.settle(ctx, step.Hold)
		classes := r.rec.Take(time.Since(held))
		stopWatch()
		sample, top := pg.End(ctx, 15)
		stopStep()

		sr := StepResult{
			Step: step, Classes: classes, Dropped: r.drops(), PG: sample, TopQueries: top,
			Online: r.onlineCount(), SignInFailures: int(r.signInFailures.Load()),
		}
		sr.Breaches = Breaches(res.Steps, sr, o.Thresholds)
		res.Steps = append(res.Steps, sr)
		if err := writeResults(o.Out, &res); err != nil {
			return err
		}
		log.Printf("step %d: %s", step.Index+1, summarize(sr))
		if len(sr.Breaches) > 0 && !o.KeepGoing {
			log.Printf("step %d broke; stopping (pass -keep-going to run the remaining steps)", step.Index+1)
			break
		}
	}
	stopUsers()
	res.FinishedAt = time.Now().UTC()
	return writeResults(o.Out, &res)
}

func (r *runner) settle(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func (r *runner) drop(class string) {
	v, _ := r.dropped.LoadOrStore(class, new(atomic.Uint64))
	v.(*atomic.Uint64).Add(1)
}

func (r *runner) drops() map[string]uint64 {
	out := map[string]uint64{}
	r.dropped.Range(func(k, v any) bool {
		out[k.(string)] = v.(*atomic.Uint64).Load()
		return true
	})
	return out
}

func (r *runner) onlineCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.sessions)
}

// hit makes one request and records it. ok decides whether the status is the
// answer Pando should have given.
func (r *runner) hit(ctx context.Context, surface, class, method, url, host string, cred Credential, ok func(int) bool) Response {
	resp, err := r.c.Do(ctx, method, url, host, cred, "")
	if ctx.Err() != nil {
		// The step ended under the request; it measured nothing.
		return resp
	}
	r.rec.Record(Outcome{
		Surface: surface, Class: class, Status: resp.Status, Err: err,
		OK: err == nil && ok(resp.Status), Latency: resp.Latency,
	})
	return resp
}

func is2xx(s int) bool { return s >= 200 && s < 300 }

func expect(codes ...int) func(int) bool {
	return func(s int) bool {
		for _, c := range codes {
			if s == c {
				return true
			}
		}
		return false
	}
}

// consoleUser signs in and then behaves like the console's open tab until
// ctx ends. ready is called once the sign-in has answered.
func (r *runner) consoleUser(ctx context.Context, u wsUser, ready func()) {
	cookie, resp, err := r.c.SignIn(ctx, u.Name, r.o.UserPassword)
	r.rec.Record(Outcome{Surface: "sign-in", Class: "POST /sessions", Status: resp.Status, Err: err,
		OK: err == nil, Latency: resp.Latency, Always: true})
	ready()
	if err != nil {
		r.signInFailures.Add(1)
		return
	}
	r.mu.Lock()
	r.sessions[u.ID] = cookie
	r.online = append(r.online, u.ID)
	r.mu.Unlock()

	cred := Credential{Cookie: cookie}
	get := func(class, path string) func(context.Context) {
		return func(ctx context.Context) {
			r.hit(ctx, "console", class, http.MethodGet, r.c.API(path), "", cred, is2xx)
		}
	}

	// The first load: what every screen asks for (app/principal.ts, the
	// inbox), then the persona's screen.
	get("GET /me", "/me")(ctx)
	get("GET /apps", "/apps")(ctx)
	get("GET /me/notifications", "/me/notifications")(ctx)

	var wg sync.WaitGroup
	poll := func(interval time.Duration, fn func(context.Context)) {
		wg.Add(1)
		go func() { defer wg.Done(); every(ctx, interval, fn) }()
	}
	watch := func(app string) {
		get("GET /apps/{id}", "/apps/"+app)(ctx)
		get("GET /apps/{id}/deployments", "/apps/"+app+"/deployments")(ctx)
		get("GET /apps/{id}/specs", "/apps/"+app+"/specs")(ctx)
		poll(pollStatus, get("GET /apps/{id}/status", "/apps/"+app+"/status"))
		poll(pollUsage, get("GET /apps/{id}/usage", "/apps/"+app+"/usage"))
	}

	poll(pollInbox, get("GET /me/notifications", "/me/notifications"))
	switch personaOf(u) {
	case PersonaLauncher:
		get("GET /me/apps", "/me/apps")(ctx)
		// Coming back to the launcher refetches it: the query is stale
		// after ten seconds (main.tsx), and nothing polls it.
		poll(jitter(r.o.Navigate, 0.2), func(ctx context.Context) {
			get("GET /me/apps", "/me/apps")(ctx)
			get("GET /apps", "/apps")(ctx)
		})
	case PersonaOwner:
		get("GET /approvals", "/approvals")(ctx)
		poll(pollApprovals, get("GET /approvals", "/approvals"))
		watch(u.Owns[rand.IntN(len(u.Owns))])
	case PersonaAdmin:
		get("GET /users", "/users")(ctx)
		get("GET /approvals", "/approvals")(ctx)
		poll(pollApprovals, get("GET /approvals", "/approvals"))
		poll(jitter(r.o.Navigate, 0.2), func(ctx context.Context) {
			get("GET /apps", "/apps")(ctx)
			get("GET /users", "/users")(ctx)
		})
		watch(r.ws.Seeded[rand.IntN(len(r.ws.Seeded))].ID)
	}
	wg.Wait()
}

// apiRequest is one request from an API client holding a delegated token,
// against its owner's own app: the CLI or a script.
func (r *runner) apiRequest(ctx context.Context) {
	if len(r.ws.Tokens) == 0 {
		return
	}
	tok := r.ws.Tokens[rand.IntN(len(r.ws.Tokens))]
	cred := Credential{Bearer: tok.Bearer}
	app := ""
	if len(tok.Owns) > 0 {
		app = tok.Owns[rand.IntN(len(tok.Owns))]
	}
	n := rand.IntN(100)
	switch {
	case n < 40 || app == "":
		r.hit(ctx, "api", "GET /apps", http.MethodGet, r.c.API("/apps"), "", cred, is2xx)
	case n < 70:
		r.hit(ctx, "api", "GET /apps/{id}", http.MethodGet, r.c.API("/apps/"+app), "", cred, is2xx)
	case n < 90:
		r.hit(ctx, "api", "GET /apps/{id}/deployments", http.MethodGet, r.c.API("/apps/"+app+"/deployments"), "", cred, is2xx)
	default:
		r.hit(ctx, "api", "GET /me/apps", http.MethodGet, r.c.API("/me/apps"), "", cred, is2xx)
	}
}

// proxyRequest is one request to an app through Pando's proxy. Seeded apps
// have no containers, so the proxy resolves them and answers 503 (not
// running); that exercises the lookup by hostname and path. Real apps answer
// through the whole path: authentication, CheckData, the assertion, and the
// forward.
func (r *runner) proxyRequest(ctx context.Context) {
	n := rand.IntN(100)
	if len(r.ws.Real) == 0 || n < 50 {
		a := r.ws.Seeded[rand.IntN(len(r.ws.Seeded))]
		if a.Hostname != "" {
			r.hit(ctx, "proxy", "seeded app by hostname", http.MethodGet, r.c.Root+"/", a.Hostname, Credential{}, expect(http.StatusServiceUnavailable))
		} else {
			r.hit(ctx, "proxy", "seeded app by path", http.MethodGet, r.c.Root+a.Path+"/", "", Credential{}, expect(http.StatusServiceUnavailable))
		}
		return
	}

	a := r.ws.Real[rand.IntN(len(r.ws.Real))]
	byPort := n%2 == 0 && a.Port > 0
	url := r.c.Root + "/" + a.Slug + "/"
	how := "slug"
	if byPort {
		url = fmt.Sprintf("http://%s:%d/", r.c.hostOf(), a.Port)
		how = "port"
	}

	user, cookie := r.someSession()
	if cookie == "" || rand.IntN(2) == 0 {
		want := http.StatusFound // sent to sign in
		if a.Anonymous {
			want = http.StatusOK
		}
		r.hit(ctx, "proxy", "real app by "+how+", anonymous", http.MethodGet, url, "", Credential{}, expect(want))
		return
	}
	want := http.StatusForbidden
	if a.Members[user] {
		want = http.StatusOK
	}
	r.hit(ctx, "proxy", "real app by "+how+", signed in", http.MethodGet, url, "", Credential{Cookie: cookie}, expect(want))
}

func (r *runner) someSession() (string, string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.online) == 0 {
		return "", ""
	}
	u := r.online[rand.IntN(len(r.online))]
	return u, r.sessions[u]
}

func writeResults(path string, res *Results) error {
	if path == "" {
		return nil
	}
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
