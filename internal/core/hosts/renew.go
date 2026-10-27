package hosts

import (
	"context"
	"slices"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hostagent"
	"github.com/trypando/pando/internal/secret"
)

// Renewing a joined host's certificates (R-421), and what removing a host
// takes back (R-415). Every certificate the cluster issues is valid for 90
// days; a host asks for new ones on the cluster listener once its own is due
// (hostagent.RenewBefore), presenting the one it holds.

// RenewResult is what a host that renewed is given: its new certificate, for
// the key its request proved, and a new probe certificate carrying its ID.
type RenewResult struct {
	Host           state.Host
	Certificate    []byte
	HostClientCert []byte
	HostClientKey  secret.Value
}

// hostByCert is the host whose verified certificate is certDER: the one it
// joined or last renewed with, or the one before it, and never a removed
// host's (R-415). It returns the certificate's fingerprint too.
func (s *Service) hostByCert(ctx context.Context, certDER []byte) (state.Host, string, error) {
	if len(certDER) == 0 {
		return state.Host{}, "", errNotAHost
	}
	pin := hostagent.Fingerprint256(certDER)
	revoked, err := s.Store.CertRevoked(ctx, pin)
	if err != nil {
		return state.Host{}, "", err
	}
	if revoked {
		return state.Host{}, "", errNotAHost
	}
	list, err := s.Store.Hosts(ctx)
	if err != nil {
		return state.Host{}, "", err
	}
	i := slices.IndexFunc(list, func(h state.Host) bool { return h.HoldsCert(pin) })
	if i < 0 {
		return state.Host{}, "", errNotAHost
	}
	return list[i], pin, nil
}

// Renew issues the host presenting certDER a new certificate for the key
// csrPEM proves, with the same name and roles, and a new probe certificate,
// and records the new certificate as the host's (R-421). The one presented
// stays the host's until it presents the new one (Peers), so a renewal whose
// answer is lost leaves the host working with what it has.
func (s *Service) Renew(ctx context.Context, certDER, csrPEM []byte) (RenewResult, error) {
	h, presented, err := s.hostByCert(ctx, certDER)
	if err != nil {
		return RenewResult{}, err
	}
	cert, sha, err := s.Authority.IssueHost(csrPEM, h.Name, h.Roles, s.now())
	if err != nil {
		return RenewResult{}, err
	}
	probe, probeKey, err := s.Authority.IssueHostClient(h.ID, s.now())
	if err != nil {
		return RenewResult{}, errs.Wrap(errs.Internal, "Pando could not issue the certificate the host checks other hosts with.", err)
	}
	if h, err = s.Store.RenewHostCert(ctx, h.ID, presented, sha); err != nil {
		return RenewResult{}, err
	}
	// The runtime's host list pins both of the host's certificates now.
	s.Changed(ctx)
	return RenewResult{Host: h, Certificate: cert, HostClientCert: probe, HostClientKey: probeKey}, nil
}

// Removed is a host removed from the cluster, and what its removal leaves to
// be done by hand.
type Removed struct {
	Host state.Host `json:"host"`
	// Rotate names the keys a removed control host was handed at its join
	// (R-415), which removing it does not take back.
	Rotate []string `json:"rotate,omitempty"`
	Notice string   `json:"notice,omitempty"`
}

// rotateAfterControl is what a control host was handed at its join (R-415).
var rotateAfterControl = []string{"secrets key", "API token key", "cluster certificate authority", "database password"}

// errSelf refuses removing the host the request is served by.
var errSelf = errs.New(errs.StateInvalid, "This is the host serving this request, which cannot remove itself from the cluster.").
	WithRemedy("Remove it from another control host of the cluster.")

// Remove removes a host from the cluster (R-414). The install's own first
// host and the host serving the request are refused: the first would be
// recorded again under a new ID at its next start, leaving its apps' homes
// naming a host that is gone, and the second would cut off the request.
// Every certificate the host held is refused from then on (R-415). A control
// host knows the installation's keys, which removing it does not take back;
// the result names them.
func (s *Service) Remove(ctx context.Context, hostID string) (Removed, error) {
	if s.IsSelf != nil {
		list, err := s.Store.Hosts(ctx)
		if err != nil {
			return Removed{}, err
		}
		if i := slices.IndexFunc(list, func(h state.Host) bool { return h.ID == hostID }); i >= 0 && s.IsSelf(list[i]) {
			return Removed{}, errSelf
		}
	}
	h, err := s.Store.RemoveHost(ctx, hostID)
	if err != nil {
		return Removed{}, err
	}
	s.Changed(ctx)
	out := Removed{Host: h}
	if slices.Contains(h.Roles, state.HostRoleControl) {
		out.Rotate = rotateAfterControl
		out.Notice = "Host " + h.Name + " held the control role, so it was handed the installation's secrets key, API token key, cluster certificate authority and database password when it joined. Removing it does not take them back: replace each of them."
	}
	return out, nil
}
