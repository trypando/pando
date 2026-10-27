package state

import (
	"context"

	"github.com/trypando/pando/internal/errs"
)

// The outcomes of R-421 on the cluster's hosts: a host taken for lost, and the
// fences hosts report (migration 000088). What each host reached, and when,
// is not stored: it stays in the leader's memory (liveness.Ledger).

// MarkHostLost records that the host is lost, by the majority rule or a person
// (by). It reports whether the host was not lost before; marking a host lost
// twice keeps the first time and who said so.
func (c *Cluster) MarkHostLost(ctx context.Context, hostID, by string) (bool, error) {
	// newly is whether this statement marked it, not whether it was live
	// when read: two marking at once, only one of them did.
	var exists, newly bool
	err := c.db.QueryRow(ctx, `
		WITH marked AS (
		         UPDATE cluster_hosts SET lost_at = now(), lost_by = $2, updated_at = now()
		         WHERE id = $1 AND lost_at IS NULL
		         RETURNING id)
		SELECT EXISTS (SELECT 1 FROM cluster_hosts WHERE id = $1), EXISTS (SELECT 1 FROM marked)`,
		hostID, by).Scan(&exists, &newly)
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not record that the host is lost.", err)
	}
	if !exists {
		return false, errs.Newf(errs.NotFound, "There is no host %s in the cluster.", hostID)
	}
	return newly, nil
}

// LostHosts is the set of hosts marked lost.
func (c *Cluster) LostHosts(ctx context.Context) (map[string]bool, error) {
	rows, err := c.db.Query(ctx, `SELECT id FROM cluster_hosts WHERE lost_at IS NOT NULL`)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read which hosts are lost.", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read which hosts are lost.", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ClearHostLost records that a lost host is back in the cluster. Its copies of
// the apps that failed over are removed by whoever calls this, never
// restarted (R-419).
func (c *Cluster) ClearHostLost(ctx context.Context, hostID string) error {
	if _, err := c.db.Exec(ctx, `
		UPDATE cluster_hosts SET lost_at = NULL, lost_by = NULL, updated_at = now()
		WHERE id = $1`, hostID); err != nil {
		return errs.Wrap(errs.Internal, "Could not record that the host is back.", err)
	}
	return nil
}

// RecordHostFence records fenceID as the host's latest fence, and reports
// whether it was new: the leader audits each fence once, however many times it
// hears of it.
func (c *Cluster) RecordHostFence(ctx context.Context, hostID, fenceID string) (bool, error) {
	tag, err := c.db.Exec(ctx, `
		UPDATE cluster_hosts SET last_fence_id = $2, updated_at = now()
		WHERE id = $1 AND last_fence_id IS DISTINCT FROM $2`, hostID, fenceID)
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not record the host's fence.", err)
	}
	return tag.RowsAffected() == 1, nil
}

// SetHostFenced records whether the host says it is fenced: cut off from most
// of the cluster, with its apps stopped (R-421). It reports whether that
// changed. The leader calls it with what each host answers; every replica's
// reconciler reads it (Standbys.Held) and leaves a fenced host's apps
// stopped, so the control plane never undoes a fence.
func (c *Cluster) SetHostFenced(ctx context.Context, hostID string, fenced bool) (bool, error) {
	tag, err := c.db.Exec(ctx, `
		UPDATE cluster_hosts
		SET fenced_at = CASE WHEN $2 THEN now() ELSE NULL END, updated_at = now()
		WHERE id = $1 AND (fenced_at IS NOT NULL) IS DISTINCT FROM $2`, hostID, fenced)
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not record whether the host is fenced.", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RotateHostCert records next as the certificate the host's agent holds
// now, the current one kept as the one before (prev_cert_sha256), so a
// connection to the agent accepts either until it serves next (R-416,
// R-421). A control host renews its own certificate (hosts.ControlPeers).
// next, when it is revoked, is refused.
func (c *Cluster) RotateHostCert(ctx context.Context, hostID, next string) error {
	tag, err := c.db.Exec(ctx, `
		UPDATE cluster_hosts SET prev_cert_sha256 = cert_sha256, cert_sha256 = $2, updated_at = now()
		WHERE id = $1 AND cert_sha256 <> $2
		  AND NOT EXISTS (SELECT 1 FROM cluster_revoked_certs WHERE cert_sha256 = $2)`, hostID, next)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record the host's renewed certificate.", err)
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := c.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM cluster_hosts WHERE id = $1)`, hostID).Scan(&exists); err != nil {
			return errs.Wrap(errs.Internal, "Could not record the host's renewed certificate.", err)
		}
		if !exists {
			return errs.Newf(errs.NotFound, "There is no host %s in the cluster.", hostID)
		}
	}
	return nil
}

// SetHostPing records where a control host answers the cluster's liveness
// checks, its cluster listener, and the fingerprint of the certificate it
// answers with (R-421): a control host keeps its own row current with it
// (hosts.ControlPeers). The agent's address and pin, which the runtime
// reaches the host through (R-416), stay as the host joined with them.
func (c *Cluster) SetHostPing(ctx context.Context, hostID, address, certSHA256 string) error {
	tag, err := c.db.Exec(ctx, `
		UPDATE cluster_hosts SET ping_address = $2, ping_cert_sha256 = $3, updated_at = now()
		WHERE id = $1`, hostID, address, certSHA256)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record where the host answers the cluster's liveness checks.", err)
	}
	if tag.RowsAffected() == 0 {
		return errs.Newf(errs.NotFound, "There is no host %s in the cluster.", hostID)
	}
	return nil
}
