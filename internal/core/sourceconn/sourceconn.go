// Package sourceconn is the install's source connections (R-091; O-3 as
// re-resolved with issue #127): which connection reads a repository, what it
// reads it with, and the OAuth authorizations that give a connection its token.
//
// A connection is an adapter_configs row in the source category, its token or
// key in adapter_credentials as ciphertext (R-190). Unlike the other adapter
// categories a connection is built from its row whenever it is used rather
// than once at startup, so connecting GitHub needs no restart: an
// administrator connects it, and the next app added from it works.
//
// The adapter knows the forge; this package decides. It checks the source
// allowlist before a credential is asked for (R-092), stores what a refreshed
// token became, records each clone in the audit log, and never lets a
// credential reach anything but the git client in this process (R-112).
package sourceconn

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// ConfigStore reads configured adapters.
type ConfigStore interface {
	List(ctx context.Context) ([]state.AdapterConfig, error)
}

// CredentialStore reads and writes sealed credentials.
type CredentialStore interface {
	Resolve(ctx context.Context, adapterID string) (map[string]secret.Value, error)
	Put(ctx context.Context, adapterID, field string, v secret.Value) error
	Delete(ctx context.Context, adapterID, field string) error
}

// SourcePolicy is the source allowlist (R-092).
type SourcePolicy interface {
	AllowsSource(ctx context.Context, src spec.Source) error
}

// AuditWriter appends to the audit log.
type AuditWriter interface {
	Write(ctx context.Context, e audit.Event) error
}

// Prober checks a repository can be read without cloning it.
type Prober interface {
	Probe(ctx context.Context, src spec.Source) error
}

// Service is the install's source connections.
type Service struct {
	Configs     ConfigStore
	Credentials CredentialStore

	// Authorizations holds OAuth authorizations in progress
	// (state.NewSourceAuthorizations).
	Authorizations CredentialStore

	// New returns an unconfigured adapter of a source kind, or nil.
	New func(kind string) api.SourceAdapter

	// Declared are source adapters the configuration file declares, already
	// configured at startup, keyed by ID. A row in adapter_configs with the
	// same ID is the declared one's record and is not built again.
	Declared map[string]api.SourceAdapter

	Policy SourcePolicy
	Audit  AuditWriter
	Clock  clock.Clock

	// Prober is what Check probes a repository with: the install's Sources,
	// which read through this service for their credential.
	Prober Prober
}

// Connection is one source connection, built.
type Connection struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`

	Capabilities api.SourceCapabilities `json:"capabilities"`

	// Problem is why the connection cannot be used — its settings or
	// credential were refused — or empty.
	Problem string `json:"problem,omitempty"`

	adapter api.SourceAdapter
}

// Usable says the connection can read repositories now.
func (c Connection) Usable() bool {
	return c.adapter != nil && c.Problem == "" && c.Capabilities.Authorized
}

// Adapter is the configured adapter, for the handlers that list repositories
// and authorize. Nil when the connection could not be built.
func (c Connection) Adapter() api.SourceAdapter { return c.adapter }

// List builds every enabled source connection, ordered by name. One that
// cannot be built is listed with its Problem rather than left out, so the
// console can say what is wrong with it.
func (s *Service) List(ctx context.Context) ([]Connection, error) {
	if s == nil || s.Configs == nil {
		return nil, nil
	}
	rows, err := s.Configs.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []Connection
	seen := map[string]bool{}
	for _, row := range rows {
		if row.Category != string(api.CategorySource) || !row.Enabled {
			continue
		}
		seen[row.ID] = true
		if a, ok := s.Declared[row.ID]; ok {
			out = append(out, connection(row.ID, row.Name, row.Kind, a, ""))
			continue
		}
		out = append(out, s.build(ctx, row))
	}
	for id, a := range s.Declared {
		if !seen[id] {
			out = append(out, connection(id, id, a.Kind(), a, ""))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// Get builds one connection.
func (s *Service) Get(ctx context.Context, id string) (Connection, error) {
	all, err := s.List(ctx)
	if err != nil {
		return Connection{}, err
	}
	for _, c := range all {
		if c.ID == id {
			return c, nil
		}
	}
	return Connection{}, errs.Newf(errs.NotFound,
		"There is no source connection %q on this installation.", id).
		WithRemedy("List the connections under Sources in the console, or with pando source list.")
}

func connection(id, name, kind string, a api.SourceAdapter, problem string) Connection {
	c := Connection{ID: id, Name: name, Kind: kind, Problem: problem, adapter: a}
	if a != nil {
		c.Capabilities = a.SourceCapabilities()
	}
	if c.Name == "" {
		c.Name = id
	}
	return c
}

// build configures a connection from its row and its sealed credentials. The
// reason one cannot be built is kept, never the credential: an adapter's
// Configure error is held to R-194 like every other.
func (s *Service) build(ctx context.Context, row state.AdapterConfig) Connection {
	if s.New == nil {
		return connection(row.ID, row.Name, row.Kind, nil, "This build of Pando cannot run source connections.")
	}
	a := s.New(row.Kind)
	if a == nil {
		return connection(row.ID, row.Name, row.Kind, nil,
			"This build of Pando has no source adapter of kind "+row.Kind+".")
	}
	var creds map[string]secret.Value
	if s.Credentials != nil {
		var err error
		if creds, err = s.Credentials.Resolve(ctx, row.ID); err != nil {
			return connection(row.ID, row.Name, row.Kind, nil,
				"The connection's credentials could not be opened. Enter them again in its settings.")
		}
	}
	raw, err := withCredentials(row.Config, creds)
	if err != nil {
		return connection(row.ID, row.Name, row.Kind, nil, "The connection's settings could not be read.")
	}
	if err := a.Configure(ctx, raw); err != nil {
		msg := "The connection's settings were refused."
		if e := errs.As(err); e != nil {
			msg = e.Message
		}
		return connection(row.ID, row.Name, row.Kind, nil, msg)
	}
	return connection(row.ID, row.Name, row.Kind, a, "")
}

// stored says a connection's credentials are Pando's to write: it has a row,
// rather than being declared by the configuration file, whose credentials are
// read from where the file says and are not Pando's to replace.
func (s *Service) stored(id string) bool {
	if s.Credentials == nil {
		return false
	}
	_, declared := s.Declared[id]
	return !declared
}

// Validate configures a connection as it would be saved — config and the
// credentials given, beside the ones already stored for id that are not
// replaced — and returns the adapter's refusal, if it has one. A credential
// sent empty removes the stored one, as saving does.
func (s *Service) Validate(ctx context.Context, id, kind string, config json.RawMessage, given map[string]secret.Value) error {
	if s.New == nil {
		return nil
	}
	a := s.New(kind)
	if a == nil {
		return errs.Newf(errs.ValidInvalid, "This build of Pando has no source adapter of kind %q.", kind)
	}
	creds := map[string]secret.Value{}
	if s.Credentials != nil && id != "" {
		if stored, err := s.Credentials.Resolve(ctx, id); err == nil {
			for k, v := range stored {
				creds[k] = v
			}
		}
	}
	for k, v := range given {
		if v.IsZero() {
			delete(creds, k)
			continue
		}
		creds[k] = v
	}
	raw, err := withCredentials(config, creds)
	if err != nil {
		return errs.New(errs.ValidInvalid, "The connection's settings could not be read.")
	}
	if err := a.Configure(ctx, raw); err != nil {
		if errs.As(err) != nil {
			return err
		}
		return errs.New(errs.ValidInvalid, "The connection's settings were refused.")
	}
	return nil
}

// withCredentials puts credentials where an adapter's Configure reads them,
// as cmd/pando does for adapters built at startup (O-20).
func withCredentials(raw json.RawMessage, creds map[string]secret.Value) (json.RawMessage, error) {
	cfg := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
	}
	plain := make(map[string]string, len(creds))
	for field, v := range creds {
		plain[field] = v.Reveal()
	}
	body, err := json.Marshal(plain)
	if err != nil {
		return nil, err
	}
	cfg["credentials"] = body
	return json.Marshal(cfg)
}

// Match is the usable connection that covers repoURL most specifically, or
// false when none does. Ties go to the connection whose ID sorts first, so
// the answer does not change from one call to the next.
func (s *Service) Match(ctx context.Context, repoURL string) (Connection, bool, error) {
	all, err := s.List(ctx)
	if err != nil {
		return Connection{}, false, err
	}
	best, bestScore := Connection{}, 0
	for _, c := range all {
		if !c.Usable() {
			continue
		}
		score := c.adapter.Covers(repoURL)
		if score > bestScore || (score == bestScore && score > 0 && c.ID < best.ID) {
			best, bestScore = c, score
		}
	}
	return best, bestScore > 0, nil
}

// Access is what source.Sources reads a repository with: the connection the
// app's spec names (Source.CredentialRef), or nothing — anonymous — when it
// names none.
//
// The allowlist is checked here as well as by every caller before a fetch
// (R-092): a blocked source must not cause a token to be minted or used, and
// this is the one place every use passes through.
func (s *Service) Access(ctx context.Context, src spec.Source, purpose source.Purpose) (*source.Access, error) {
	if src.Type != spec.SourceGit || src.CredentialRef == "" {
		return nil, nil
	}
	if s.Policy != nil {
		if err := s.Policy.AllowsSource(ctx, src); err != nil {
			return nil, err
		}
	}
	c, err := s.Get(ctx, src.CredentialRef)
	if err != nil {
		if e := errs.As(err); e != nil && e.Code == errs.NotFound {
			return nil, errs.Newf(errs.SourceUnreadable,
				"This app's repository %s is read with the source connection %q, which no longer exists.",
				src.URL, src.CredentialRef).
				WithRemedy("Connect the host again under Sources in the console, then choose the connection in the app's source settings.")
		}
		return nil, err
	}
	if !c.Usable() {
		reason := c.Problem
		if reason == "" {
			reason = "It has not been authorized yet."
		}
		return nil, errs.Newf(errs.SourceUnreadable,
			"This app's repository %s is read with the source connection %q, which cannot be used. %s",
			src.URL, c.Name, reason).
			WithRemedy("Fix the connection under Sources in the console, then try again.")
	}

	cred, err := c.adapter.GitCredential(ctx, src.URL)
	if err != nil {
		return nil, err
	}
	if len(cred.Rotated) > 0 && s.stored(c.ID) {
		// A refreshed token replaces the old one before it is used, so a
		// refresh token the provider rotated is never lost to a crash
		// between the two.
		for field, v := range cred.Rotated {
			if err := s.Credentials.Put(ctx, c.ID, field, v); err != nil {
				return nil, err
			}
		}
	}

	if purpose == source.PurposeClone && s.Audit != nil {
		// Every clone with a connection is recorded: who connected it is in
		// the audit log already, as the adapter's creation. The credential
		// never is (R-194).
		_ = s.Audit.Write(ctx, audit.Event{
			PrincipalKind: audit.KindSystem,
			PrincipalID:   "pando",
			Action:        "source.connection.use",
			AppID:         source.AppFrom(ctx),
			TargetKind:    "source_connection",
			TargetID:      c.ID,
			Detail: map[string]any{
				"repository": src.URL,
				"method":     c.Capabilities.Method,
			},
		})
	}
	return &source.Access{Credential: cred, Connection: c.Name}, nil
}

// Check decides which connection a new app's repository is read with, and
// that it can be read with it, before the app exists. It returns the
// connection's ID for the spec's credential_ref — empty for a repository read
// anonymously — or an error saying why the repository cannot be read (R-105).
//
// The caller has checked the source allowlist (R-092).
func (s *Service) Check(ctx context.Context, src spec.Source) (string, error) {
	if src.Type != spec.SourceGit || src.URL == "" {
		return src.CredentialRef, nil
	}
	if err := refuseInlineCredential(src.URL); err != nil {
		return "", err
	}
	ref := src.CredentialRef
	if ref == "" {
		c, ok, err := s.Match(ctx, src.URL)
		if err != nil {
			return "", err
		}
		if ok {
			ref = c.ID
		}
	} else if _, err := s.Get(ctx, ref); err != nil {
		return "", err
	}
	if s.Prober != nil {
		src.CredentialRef = ref
		if err := s.Prober.Probe(ctx, src); err != nil {
			return "", err
		}
	}
	return ref, nil
}
