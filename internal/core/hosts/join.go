package hosts

import (
	"context"
	"fmt"
	"net"
	"os"
	"regexp"
	"slices"
	"strconv"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hostagent"
	"github.com/trypando/pando/internal/id"
	"github.com/trypando/pando/internal/secret"
)

// DefaultClusterPort is where a control host's cluster listener answers
// unless server.cluster_addr says otherwise (R-415): the port after the host
// agent's (hostagent.DefaultPort), which a host holding both roles also
// listens on.
const DefaultClusterPort = 7444

// JoinReadiness says whether the cluster can take a host with roles now, such
// as a second host needing a backup destination off the host first (R-417).
// Optional; a join asks it before anything else, and a refusal spends no use
// of the token.
type JoinReadiness interface {
	ReadyToJoin(ctx context.Context, roles []string) error
}

// JoinRequest is what a joining host sends on the cluster listener (R-415).
type JoinRequest struct {
	Secret   secret.Value
	Name     string
	CSR      []byte
	Platform string
	Version  string
	// Address is where the joining host's agent listens, host:port (R-416).
	Address string
}

// ControlKeys are what a control host is handed at join and an app host never
// is (R-415, R-417): the database's address and the keys that protect what
// is in it against a dump (R-190, R-063).
type ControlKeys struct {
	DatabaseURL secret.Value
	// SecretsKey is the local secrets adapter's key, empty when the install
	// uses another secrets adapter. SecretsKeyPath is where that adapter's
	// configuration, which the joining host reads from the shared database,
	// says it lives.
	SecretsKey     secret.Value
	SecretsKeyPath string
	TokenKey       secret.Value
	// Authority is the cluster authority's certificate and key, PEM.
	Authority secret.Value
}

// JoinResult is what a host that joined is given.
type JoinResult struct {
	Host state.Host
	// Certificate is the host's own, issued from the authority; Authority is
	// the authority's certificate. Both PEM.
	Certificate []byte
	Authority   []byte
	// HostClientCert and HostClientKey are the certificate, and its key, the
	// host checks the others' agents with (R-421): subject pando-host, client
	// authentication only, which an agent answers with a ping and nothing
	// else. Issued here because only a holder of the authority's key can
	// issue one. PEM.
	HostClientCert []byte
	HostClientKey  secret.Value
	// NetworkPool is the runtime's app network range, which the host's agent
	// forwards into and nowhere else (R-416). Empty is the runtime's default.
	NetworkPool string
	// Keys is set for a host with the control role, and only then.
	Keys *ControlKeys
}

// KeySource reads the keys a control host is handed (R-415). Read at each
// join rather than held, so what is handed over is what is on disk now.
type KeySource struct {
	DatabaseURL  secret.Value
	TokenKeyPath string
	// SecretsKeyPath is where the local secrets adapter keeps its key, or ""
	// when the install uses another.
	SecretsKeyPath func(context.Context) string
}

func readKey(path, what string) (secret.Value, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- a configured key path
	if err != nil {
		return secret.Value{}, errs.Wrap(errs.Internal, fmt.Sprintf("Pando could not read the %s at %s to hand to the joining host.", what, path), err)
	}
	return secret.New(string(b)), nil
}

func (s *Service) controlKeys(ctx context.Context) (*ControlKeys, error) {
	k := &ControlKeys{DatabaseURL: s.Keys.DatabaseURL}
	if k.DatabaseURL.IsZero() {
		return nil, errs.New(errs.Internal, "This control host does not know its database address, so it cannot hand it to a joining control host.")
	}
	var err error
	if k.TokenKey, err = readKey(s.Keys.TokenKeyPath, "API token key"); err != nil {
		return nil, err
	}
	if s.Keys.SecretsKeyPath != nil {
		if k.SecretsKeyPath = s.Keys.SecretsKeyPath(ctx); k.SecretsKeyPath != "" {
			if k.SecretsKey, err = readKey(k.SecretsKeyPath, "secrets key"); err != nil {
				return nil, err
			}
		}
	}
	authority, err := s.Authority.PEM()
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Pando could not read the cluster authority to hand to the joining host.", err)
	}
	k.Authority = secret.New(string(authority))
	return k, nil
}

// hostName is what a host may be called: a DNS label, since it is the name
// in the host's certificate.
var hostName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// CheckHostName refuses a name a host cannot have.
func CheckHostName(name string) error {
	if !hostName.MatchString(name) {
		return errs.Newf(errs.ValidInvalid, "%s is not a host name Pando can use. Valid answer: lowercase letters, digits and hyphens, up to 63, such as app-host-2.", shortQuote(name))
	}
	return nil
}

// Bounds on what a joining host says about itself (R-415): what is recorded
// and audited is never longer.
const (
	maxPlatformLen = 64
	maxVersionLen  = 64
	maxAddressLen  = 260
)

// shown is what a joining host may say its platform and version are:
// printable ASCII without spaces, such as linux/amd64 and 0.16.0.
var shown = regexp.MustCompile(`^[\x21-\x7e]*$`)

// addressHost is a DNS name an agent may be reached by.
var addressHost = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)

// shortQuote quotes s for a message, cut short: a name a joining host sent
// may be any length.
func shortQuote(s string) string {
	const most = 64
	if len(s) > most {
		return strconv.Quote(s[:most]) + "…"
	}
	return strconv.Quote(s)
}

// CheckAgentAddress refuses an agent address the runtime cannot dial (R-416):
// it must be host:port, a DNS name or an IP address and a port number. One
// that is not would break the runtime's host list for every host.
func CheckAgentAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	n, perr := strconv.Atoi(port)
	if len(address) > maxAddressLen || err != nil || perr != nil || n < 1 || n > 65535 ||
		(net.ParseIP(host) == nil && !addressHost.MatchString(host)) {
		return errs.Newf(errs.ValidInvalid, "%s is not an address the joining host's agent can be reached at. Valid answer: a host name or IP address and a port, such as app-host-2.example.com:%d.", shortQuote(address), hostagent.DefaultPort)
	}
	return nil
}

// CheckJoinRequest refuses what a joining host sent when Pando cannot record
// or use it (R-415): every check that needs no token, so that a refusal says
// nothing about the cluster.
func CheckJoinRequest(req JoinRequest) error {
	if err := CheckHostName(req.Name); err != nil {
		return err
	}
	if err := CheckAgentAddress(req.Address); err != nil {
		return err
	}
	if len(req.Platform) > maxPlatformLen || !shown.MatchString(req.Platform) {
		return errs.Newf(errs.ValidInvalid, "The joining host's platform must be up to %d printable characters without spaces, such as linux/amd64.", maxPlatformLen)
	}
	if len(req.Version) > maxVersionLen || !shown.MatchString(req.Version) {
		return errs.Newf(errs.ValidInvalid, "The joining host's version must be up to %d printable characters without spaces, such as 0.16.0.", maxVersionLen)
	}
	return nil
}

// InvalidField stands in for a name or version that failed CheckJoinRequest
// in the audit log, which keeps what a caller who has not joined sent only
// when it is something a host could be called (R-347: the log cannot be
// trimmed for three months).
const InvalidField = "(invalid)"

// AuditDetail is what the audit log records of a join that was refused.
func (req JoinRequest) AuditDetail() map[string]any {
	name, version := req.Name, req.Version
	if CheckHostName(name) != nil {
		name = InvalidField
	}
	if len(version) > maxVersionLen || !shown.MatchString(version) {
		version = InvalidField
	}
	return map[string]any{"name": name, "version": version}
}

// Join admits a host (R-415). It refuses, spending no use of the token, a
// request Pando cannot use, a host at another version, and whatever
// Readiness refuses; otherwise it spends one use, issues the host's
// certificates, records the host, and answers with what the token's roles
// need — the keys only for the control role. The cluster's version is said
// only to a host whose token is good: a caller without one learns nothing
// about the cluster.
func (s *Service) Join(ctx context.Context, req JoinRequest) (JoinResult, error) {
	if err := CheckJoinRequest(req); err != nil {
		return JoinResult{}, err
	}

	var res JoinResult
	_, host, err := s.Store.Join(ctx, req.Secret, func(t state.JoinToken) (state.Host, error) {
		if req.Version != s.Version {
			return state.Host{}, errs.Newf(errs.StateInvalid,
				"This host runs Pando %s and the cluster runs %s. A host joins only at the cluster's version.", orUnknown(req.Version), orUnknown(s.Version)).
				WithRemedy("Install Pando " + orUnknown(s.Version) + " on the joining host and start the join again.")
		}
		if err := s.admissible(ctx, t.Roles); err != nil {
			return state.Host{}, err
		}
		hostID := id.New(id.Host)
		cert, sha, err := s.Authority.IssueHost(req.CSR, req.Name, t.Roles, s.now())
		if err != nil {
			return state.Host{}, err
		}
		res.Certificate = cert
		if res.HostClientCert, res.HostClientKey, err = s.Authority.IssueHostClient(hostID, s.now()); err != nil {
			return state.Host{}, errs.Wrap(errs.Internal, "Pando could not issue the certificate the joining host checks other hosts with.", err)
		}
		if s.Runtime != nil && slices.Contains(t.Roles, state.HostRoleApp) {
			res.NetworkPool = s.Runtime.NetworkPool(ctx)
		}
		if slices.Contains(t.Roles, state.HostRoleControl) {
			if res.Keys, err = s.controlKeys(ctx); err != nil {
				return state.Host{}, err
			}
		}
		return state.Host{
			ID: hostID, Name: req.Name, Address: req.Address, CertSHA256: sha,
			Platform: req.Platform, Version: req.Version,
		}, nil
	})
	if err != nil {
		return JoinResult{}, err
	}
	res.Host = host
	res.Authority = s.Authority.CertPEM()
	s.Changed(ctx)
	return res, nil
}

// admissible asks whether the cluster can take a host with roles now: the
// cluster (Readiness, R-417), then the runtime for an app host (R-418).
func (s *Service) admissible(ctx context.Context, roles []string) error {
	if s.Readiness != nil {
		if err := s.Readiness.ReadyToJoin(ctx, roles); err != nil {
			return err
		}
	}
	if s.Runtime != nil {
		return s.Runtime.CheckJoin(ctx, roles)
	}
	return nil
}

func orUnknown(v string) string {
	if v == "" {
		return "of an unknown version"
	}
	return v
}
