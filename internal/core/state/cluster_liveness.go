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
