package hosts

import (
	"cmp"
	"context"
	"fmt"
	"net"
	"os"
	"regexp"
	"slices"
	"strconv"
	"time"

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

// CheckHostName refuses a name a host cannot have (R-415): one the Docker
// hosts runtime would not take (runtimeHostName, its own rule), since a
// joined app host is written into its host list under it and one bad name
// stops every later sync of that list (R-418), or installHostName, which the
// runtime's own entry for the install's machine may already have. Which
// names the runtime's host list holds is checked once the token is
// (Runtime.CheckJoin).
func CheckHostName(name string) error {
	if !runtimeHostName.MatchString(name) {
		return errs.Newf(errs.ValidInvalid, "%s is not a host name Pando can use. Valid answer: lowercase letters, digits and hyphens, up to 40, starting and ending with a letter or digit, such as app-host-2.", shortQuote(name))
	}
	if name == installHostName {
		return errs.Newf(errs.ValidInvalid, "%q is the name Pando keeps for the installation's first host. Valid answer: another name, such as app-host-2.", installHostName)
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
	if !runtimeHostName.MatchString(name) {
		name = InvalidField
	}
	if len(version) > maxVersionLen || !shown.MatchString(version) {
		version = InvalidField
	}
	return map[string]any{"name": name, "version": version}
}

// DefaultJoinCheckTimeout is how long a join's checks may take before the
// join is refused (R-415 [P]).
const DefaultJoinCheckTimeout = 30 * time.Second

// Join admits a host (R-415). It refuses, spending no use of the token, a
// request Pando cannot use, a host at another version, and whatever
// Readiness or the runtime refuses; otherwise it issues the host's
// certificates, then spends one use and records the host, pending, and
// answers with what the token's roles need — the keys only for the control
// role. The cluster's version is said only to a host whose token is good: a
// caller without one learns nothing about the cluster.
//
// Every slow step runs before the token is spent, with no database
// connection held and within JoinCheckTimeout, so a check that hangs keeps no
// other join waiting on the token's row and holds none of the pool; the
// spend and the record are one short transaction (state.Cluster.Join). The
// host is listed once it presents the certificate it was issued
// (Service.Peers, which the joining host asks right after writing its files);
// until then a join of the same name and key replaces it, and one of the
// same name and another key, another machine's, is refused.
func (s *Service) Join(ctx context.Context, req JoinRequest) (JoinResult, error) {
	if err := CheckJoinRequest(req); err != nil {
		return JoinResult{}, err
	}
	t, err := s.Store.CheckJoinToken(ctx, req.Secret)
	if err != nil {
		return JoinResult{}, err
	}
	if req.Version != s.Version {
		return JoinResult{}, errs.Newf(errs.StateInvalid,
			"This host runs Pando %s and the cluster runs %s. A host joins only at the cluster's version.", orUnknown(req.Version), orUnknown(s.Version)).
			WithRemedy("Install Pando " + orUnknown(s.Version) + " on the joining host and start the join again.")
	}
	res, host, err := s.prepareJoin(ctx, req, t)
	if err != nil {
		return JoinResult{}, err
	}
	if _, res.Host, err = s.Store.Join(ctx, req.Secret, host); err != nil {
		return JoinResult{}, err
	}
	res.Authority = s.Authority.CertPEM()
	return res, nil
}

// prepareJoin makes every check and issues everything a join needs before
// the token is spent, within JoinCheckTimeout.
func (s *Service) prepareJoin(ctx context.Context, req JoinRequest, t state.JoinToken) (JoinResult, state.Host, error) {
	timeout := cmp.Or(s.JoinCheckTimeout, DefaultJoinCheckTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, host, err := s.issueJoin(ctx, req, t)
	if err != nil && ctx.Err() != nil {
		return res, host, errs.Wrap(errs.AdapterUnavailable,
			fmt.Sprintf("The cluster's checks of the joining host did not finish within %s, so the join was refused. Its join token was not spent.", timeout), err).
			WithRemedy("Check that the cluster's control hosts and its runtime are reachable, then start the join again.")
	}
	return res, host, err
}

func (s *Service) issueJoin(ctx context.Context, req JoinRequest, t state.JoinToken) (JoinResult, state.Host, error) {
	var res JoinResult
	if err := s.admissible(ctx, req.Name, t.Roles); err != nil {
		return res, state.Host{}, err
	}
	hostID := id.New(id.Host)
	cert, sha, err := s.Authority.IssueHost(req.CSR, req.Name, t.Roles, s.now())
	if err != nil {
		return res, state.Host{}, err
	}
	res.Certificate = cert
	keySHA, err := csrKeySHA256(req.CSR)
	if err != nil {
		return res, state.Host{}, err
	}
	if res.HostClientCert, res.HostClientKey, err = s.Authority.IssueHostClient(hostID, s.now()); err != nil {
		return res, state.Host{}, errs.Wrap(errs.Internal, "Pando could not issue the certificate the joining host checks other hosts with.", err)
	}
	if s.Runtime != nil && slices.Contains(t.Roles, state.HostRoleApp) {
		res.NetworkPool = s.Runtime.NetworkPool(ctx)
	}
	if slices.Contains(t.Roles, state.HostRoleControl) {
		if res.Keys, err = s.controlKeys(ctx); err != nil {
			return res, state.Host{}, err
		}
	}
	return res, state.Host{
		ID: hostID, Name: req.Name, Address: req.Address, CertSHA256: sha, KeySHA256: keySHA,
		Platform: req.Platform, Version: req.Version,
	}, ctx.Err()
}

// admissible asks whether the cluster can take a host called name with roles
// now: the cluster (Readiness, R-417), then the runtime for an app host or a
// second control host (R-418). Asked once the token is good, so that only a
// holder of one learns what the runtime's host list holds.
func (s *Service) admissible(ctx context.Context, name string, roles []string) error {
	if s.Readiness != nil {
		if err := s.Readiness.ReadyToJoin(ctx, roles); err != nil {
			return err
		}
	}
	if s.Runtime != nil {
		return s.Runtime.CheckJoin(ctx, name, roles)
	}
	return nil
}

func orUnknown(v string) string {
	if v == "" {
		return "of an unknown version"
	}
	return v
}
