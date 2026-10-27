package hosts

import (
	"context"
	"crypto"
	"crypto/x509"
	"encoding/pem"
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
// host's (R-415). The host may be pending (state.Host.Pending). It returns
// the certificate's fingerprint too.
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
	h, found, err := s.Store.HostByCert(ctx, pin)
	if err != nil {
		return state.Host{}, "", err
	}
	if !found {
		return state.Host{}, "", errNotAHost
	}
	return h, pin, nil
}

// errRenewedAlready refuses a second renewal of one certificate for another
// key (R-421): the first is the host's next certificate.
var errRenewedAlready = errs.New(errs.StateInvalid,
	"This host's certificate was renewed already, for another key, and the host has not used the renewed one yet. A certificate is renewed once.").
	WithRemedy("Restart Pando on the host so it takes the certificate it was given. If the host lost it, remove the host from the cluster and join it again with a new join token.")

// Renew issues the host presenting certDER a new certificate for the key
// csrPEM proves, with the same name and roles, and a new probe certificate,
// and records the new certificate as the host's (R-421). The one presented
// stays the host's until it presents the new one (Peers).
//
// A certificate is renewed once (state.Cluster.RenewHostCert): a renewal
// presenting one already renewed — another made at the same time, or the
// same one tried again after its answer was lost — is answered with the
// certificate the first was, when it asks for the same key, and refused
// otherwise. So the host ends up holding the certificate it presented or the
// one it was answered with, and the cluster pins both.
func (s *Service) Renew(ctx context.Context, certDER, csrPEM []byte) (RenewResult, error) {
	h, presented, err := s.hostByCert(ctx, certDER)
	if err != nil {
		return RenewResult{}, err
	}
	if h.Pending {
		return RenewResult{}, errNotAHost
	}
	var cert []byte
	if presented == h.CertSHA256 {
		issued, sha, err := s.Authority.IssueHost(csrPEM, h.Name, h.Roles, s.now())
		if err != nil {
			return RenewResult{}, err
		}
		won := false
		if h, won, err = s.Store.RenewHostCert(ctx, h.ID, presented, sha, issued); err != nil {
			return RenewResult{}, err
		}
		if won {
			cert = issued
			// The runtime's host list pins both of the host's certificates now.
			s.Changed(ctx)
		}
	}
	if cert == nil {
		if cert, err = answeredBefore(h, presented, csrPEM); err != nil {
			return RenewResult{}, err
		}
	}
	probe, probeKey, err := s.Authority.IssueHostClient(h.ID, s.now())
	if err != nil {
		return RenewResult{}, errs.Wrap(errs.Internal, "Pando could not issue the certificate the host checks other hosts with.", err)
	}
	return RenewResult{Host: h, Certificate: cert, HostClientCert: probe, HostClientKey: probeKey}, nil
}

// answeredBefore is the certificate h's latest renewal was answered with,
// for a renewal presenting the certificate that one renewed, asking for the
// same key (R-421).
func answeredBefore(h state.Host, presented string, csrPEM []byte) ([]byte, error) {
	if presented != h.PrevCertSHA256 || len(h.NextCertPEM) == 0 {
		return nil, errRenewedAlready
	}
	pub, err := csrKey(csrPEM)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(h.NextCertPEM)
	if block == nil {
		return nil, errRenewedAlready
	}
	next, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, errRenewedAlready
	}
	if k, ok := next.PublicKey.(interface{ Equal(crypto.PublicKey) bool }); !ok || !k.Equal(pub) {
		return nil, errRenewedAlready
	}
	return h.NextCertPEM, nil
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
