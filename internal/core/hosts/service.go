package hosts

import (
	"context"
	"encoding/base64"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Store is the slice of state.Cluster the service uses.
type Store interface {
	CreateJoinToken(ctx context.Context, roles []string, maxUses int, createdBy string, expiresAt time.Time) (state.IssuedJoinToken, error)
	JoinTokens(ctx context.Context) ([]state.JoinToken, error)
	RevokeJoinToken(ctx context.Context, tokenID string) (state.JoinToken, error)
	Hosts(ctx context.Context) ([]state.Host, error)
	RemoveHost(ctx context.Context, hostID string) (state.Host, error)
}

// Join token limits (R-415).
const (
	DefaultTokenLifetime = 24 * time.Hour
	MaxTokenLifetime     = 30 * 24 * time.Hour
	MaxTokenUses         = 100

	// bothRolesUpTo is how many hosts take both roles by default [P]. Three
	// control hosts are enough for the majority rule (R-421); past them a
	// control host costs a database pool and a leader candidate and adds
	// nothing, so later tokens default to the app role.
	bothRolesUpTo = 3
)

// tokenPrefix marks a join token, and its version.
const tokenPrefix = "pj1"

// Service mints join tokens and lists and removes hosts. Authorization is the
// caller's: httpapi checks install.cluster.manage, and every install verb for
// a token carrying the control role (R-080a). ControlRoleNeedsEveryVerb says
// which tokens those are, so the rule is written once.
type Service struct {
	Store     Store
	Authority *Authority

	// Address is where a joining host reaches this install, used when a token
	// is minted without one: the server's advertise address.
	Address string

	// Runtime keeps the runtime adapter's host list in line with the
	// cluster's app hosts (runtime.go). A join checks Runtime.CheckJoin
	// before it lets an app host in. Nil in tests that do not need it.
	Runtime *Runtime

	// OnChange runs after a host joins or is removed: the server syncs the
	// runtime's host list there (Runtime.Sync) and logs what fails, which
	// does not undo the change. Nil does nothing.
	OnChange func(ctx context.Context)

	Now func() time.Time
}

// Changed reports a change to the host list (OnChange).
func (s *Service) Changed(ctx context.Context) {
	if s.OnChange != nil {
		s.OnChange(ctx)
	}
}

// MintRequest is what a join token is minted from. Zero values take the
// defaults.
type MintRequest struct {
	Roles     []string
	MaxUses   int
	ExpiresIn time.Duration
	Address   string
	CreatedBy string
}

// Minted is a join token and the string a joining host is given. Join is
// present exactly once, here.
type Minted struct {
	Token state.JoinToken `json:"token"`
	Join  secret.Value    `json:"join"`
}

// ControlRoleNeedsEveryVerb reports whether minting a token for roles hands
// over every key the install has: a control host receives the secrets key and
// the token key at join (R-415, R-417).
func ControlRoleNeedsEveryVerb(roles []string) bool {
	return slices.Contains(roles, state.HostRoleControl)
}

// DefaultRoles is the roles a token minted without any gets [P]: both while
// the cluster has fewer than three hosts, the app role after.
func (s *Service) DefaultRoles(ctx context.Context) ([]string, error) {
	hosts, err := s.Store.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	// A cluster of one has no rows until hosts join: the install itself is
	// the first host.
	if len(hosts)+1 < bothRolesUpTo {
		return []string{state.HostRoleControl, state.HostRoleApp}, nil
	}
	return []string{state.HostRoleApp}, nil
}

// NormalizeRoles checks roles and returns them sorted and without repeats.
func NormalizeRoles(roles []string) ([]string, error) {
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		r = strings.TrimSpace(r)
		if r != state.HostRoleControl && r != state.HostRoleApp {
			return nil, errs.Newf(errs.ValidInvalid, "%q is not a host role. Valid answers: control, app, or both.", r)
		}
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	slices.Sort(out)
	return out, nil
}

// Mint creates a join token.
func (s *Service) Mint(ctx context.Context, req MintRequest) (Minted, error) {
	roles := req.Roles
	if len(roles) == 0 {
		var err error
		if roles, err = s.DefaultRoles(ctx); err != nil {
			return Minted{}, err
		}
	}
	roles, err := NormalizeRoles(roles)
	if err != nil {
		return Minted{}, err
	}

	uses := req.MaxUses
	if uses == 0 {
		uses = 1
	}
	if uses < 1 || uses > MaxTokenUses {
		return Minted{}, errs.Newf(errs.ValidInvalid, "A join token can be used from 1 to %d times.", MaxTokenUses)
	}

	lifetime := req.ExpiresIn
	if lifetime == 0 {
		lifetime = DefaultTokenLifetime
	}
	if lifetime < time.Minute || lifetime > MaxTokenLifetime {
		return Minted{}, errs.New(errs.ValidInvalid, "A join token lasts from a minute to 30 days.")
	}

	address := req.Address
	if address == "" {
		address = s.Address
	}
	if err := checkAddress(address); err != nil {
		return Minted{}, err
	}

	issued, err := s.Store.CreateJoinToken(ctx, roles, uses, req.CreatedBy, s.now().Add(lifetime))
	if err != nil {
		return Minted{}, err
	}
	return Minted{
		Token: issued.Token,
		Join:  secret.New(Encode(address, s.Authority.Hash(), issued.Secret)),
	}, nil
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func checkAddress(address string) error {
	u, err := url.Parse(address)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errs.Newf(errs.ValidInvalid, "%q is not an address a joining host can reach. Give the install's address, for example https://pando.example.com.", address)
	}
	return nil
}

// Encode is the string a joining host is given:
// pj1.<base64url address>.<authority hash>.<token id>.<secret>.
func Encode(address, authorityHash string, tokenSecret secret.Value) string {
	return strings.Join([]string{
		tokenPrefix,
		base64.RawURLEncoding.EncodeToString([]byte(address)),
		authorityHash,
		tokenSecret.Reveal(),
	}, ".")
}

// Decoded is a join token taken apart.
type Decoded struct {
	Address       string
	AuthorityHash string
	Secret        secret.Value
}

// Decode takes a join token apart, without checking it with anyone.
func Decode(token string) (Decoded, error) {
	bad := errs.New(errs.ValidInvalid, "This is not a Pando join token. A join token starts with pj1.").
		WithRemedy("Copy the whole token from the host that created it.")
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 5 || parts[0] != tokenPrefix {
		return Decoded{}, bad
	}
	addr, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(parts[2]) != 64 || parts[3] == "" || parts[4] == "" {
		return Decoded{}, bad
	}
	return Decoded{
		Address:       string(addr),
		AuthorityHash: parts[2],
		Secret:        secret.New(parts[3] + "." + parts[4]),
	}, nil
}

// Tokens lists every join token, newest first.
func (s *Service) Tokens(ctx context.Context) ([]state.JoinToken, error) {
	return s.Store.JoinTokens(ctx)
}

// Revoke revokes a join token.
func (s *Service) Revoke(ctx context.Context, tokenID string) (state.JoinToken, error) {
	return s.Store.RevokeJoinToken(ctx, tokenID)
}

// Hosts lists the cluster's hosts.
func (s *Service) Hosts(ctx context.Context) ([]state.Host, error) {
	return s.Store.Hosts(ctx)
}

// Remove removes a host from the cluster.
func (s *Service) Remove(ctx context.Context, hostID string) (state.Host, error) {
	h, err := s.Store.RemoveHost(ctx, hostID)
	if err == nil {
		s.Changed(ctx)
	}
	return h, err
}
