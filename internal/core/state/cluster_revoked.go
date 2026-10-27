package state

import (
	"context"

	"github.com/trypando/pando/internal/errs"
)

// Revoked is what the cluster refuses of the hosts removed from it (R-415):
// their IDs, and the fingerprints of every certificate recorded as theirs.
//
// A probe certificate is never recorded by fingerprint: it is issued at a
// join and at each renewal, and carries the host's ID as its one URI
// (hostagent.IssueHostClient). Revoking the ID is what revokes every probe
// certificate the host was ever issued; IDs are never reused. RemoveHost
// records at least the host's current certificate, which every host has
// (cluster_hosts.cert_sha256 is NOT NULL), so every removed host's ID is
// among Hosts.
type Revoked struct {
	Hosts []string `json:"hosts"`
	Certs []string `json:"certs"`
}

// Revoked lists every removed host's ID and certificate fingerprint, for the
// hosts that answer pings to refuse them (R-415, hosts.Members).
func (c *Cluster) Revoked(ctx context.Context) (Revoked, error) {
	const msg = "Could not read the certificates of the hosts removed from the cluster."
	out := Revoked{Hosts: []string{}, Certs: []string{}}
	rows, err := c.db.Query(ctx, `SELECT host_id, cert_sha256 FROM cluster_revoked_certs ORDER BY host_id, cert_sha256`)
	if err != nil {
		return out, errs.Wrap(errs.Internal, msg, err)
	}
	defer rows.Close()
	for rows.Next() {
		var hostID, sha string
		if err := rows.Scan(&hostID, &sha); err != nil {
			return out, errs.Wrap(errs.Internal, msg, err)
		}
		if n := len(out.Hosts); n == 0 || out.Hosts[n-1] != hostID {
			out.Hosts = append(out.Hosts, hostID)
		}
		out.Certs = append(out.Certs, sha)
	}
	if err := rows.Err(); err != nil {
		return out, errs.Wrap(errs.Internal, msg, err)
	}
	return out, nil
}
