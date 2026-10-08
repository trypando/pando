package state

import "context"

// BootstrapLock is the advisory lock a starting replica holds while it
// bootstraps, for tests that hold it the way another replica would.
const BootstrapLock = bootstrapLock

// SearchUsers and SearchApps are the expressions the account and app searches
// match, which the trigram indexes must index exactly.
const (
	SearchUsers = searchUsers
	SearchApps  = searchApps
)

// SetPruneBatches sets how many apps a PruneSpecRevisions statement reads and
// how many revisions it removes from each, and returns a function restoring
// the defaults.
func SetPruneBatches(apps, perApp int) (restore func()) {
	oldApps, oldPerApp := pruneAppsPerBatch, prunePerApp
	pruneAppsPerBatch, prunePerApp = apps, perApp
	return func() { pruneAppsPerBatch, prunePerApp = oldApps, oldPerApp }
}

// ManagerCountNeeded reports whether a change to these people's memberships
// (users) or these groups' (groups) would have the R-088 count of managers
// taken before and after it.
func ManagerCountNeeded(ctx context.Context, db *DB, users, groups []string) (bool, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var l lockout
	if users != nil {
		l, err = lockoutForUsers(ctx, tx, users...)
	} else {
		l, err = lockoutForGroups(ctx, tx, groups...)
	}
	return l.check, err
}
