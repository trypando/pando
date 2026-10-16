//go:build integration

package state_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/migrations"
)

// TestR045_AnEmailOfUnknownProvenanceIsNotLinkedAfterUpgrade asserts the
// backfill in migration 000074: before it, an account holder could set their
// own email and nothing said so. The audit log's last event that set the email
// decides — an administrator's is trusted for linking by email; the holder's
// own, or none at all, is not.
func TestR045_AnEmailOfUnknownProvenanceIsNotLinkedAfterUpgrade(t *testing.T) {
	ctx := context.Background()
	db, ownerURL := statetest.Connect(t)
	owner, err := pgx.Connect(ctx, ownerURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = owner.Close(ctx) })

	users := state.NewUsers(db)
	require.NoError(t, users.EnsureLocalAdapter(ctx))
	account := func(name string) string {
		u, err := users.Create(ctx, state.LocalAdapterID, name, name+"@example.com", name, "", false)
		require.NoError(t, err)
		return u.ID
	}
	event := func(action, principal, onBehalfOf, target, detail string) {
		_, err := owner.Exec(ctx, `
			INSERT INTO audit_events (principal_kind, principal_id, on_behalf_of, action, target_kind, target_id, detail)
			VALUES ('user', $1, nullif($2, ''), $3, 'user', $4, $5::jsonb)`,
			principal, onBehalfOf, action, target, detail)
		require.NoError(t, err)
	}

	admin := "usr_01ADMINISTRATOR0000000000"
	created := account("created")
	event("user.create", admin, admin, created, `{}`)
	selfSet := account("selfset")
	event("user.update", selfSet, selfSet, selfSet, `{"email":"selfset@example.com"}`)
	byToken := account("bytoken")
	event("user.update", "tok_01SELF", byToken, byToken, `{"email":"bytoken@example.com"}`)
	unknown := account("unknown")
	thenAdmin := account("thenadmin")
	event("user.update", thenAdmin, thenAdmin, thenAdmin, `{"email":"x@example.com"}`)
	event("user.update", admin, admin, thenAdmin, `{"email":"thenadmin@example.com"}`)
	nameOnly := account("nameonly")
	event("user.create", admin, admin, nameOnly, `{}`)
	event("user.update", nameOnly, nameOnly, nameOnly, `{"display_name":"N"}`)

	up, err := migrations.FS.ReadFile("000074_email_set_by_self.up.sql")
	require.NoError(t, err)
	backfill := string(up)[strings.Index(string(up), "UPDATE users u"):]
	_, err = owner.Exec(ctx, `UPDATE users SET email_set_by_self = false`)
	require.NoError(t, err)
	_, err = owner.Exec(ctx, backfill)
	require.NoError(t, err)

	bySelf := func(userID string) bool {
		var v bool
		require.NoError(t, owner.QueryRow(ctx, `SELECT email_set_by_self FROM users WHERE id = $1`, userID).Scan(&v))
		return v
	}
	require.False(t, bySelf(created), "an administrator made the account with its email")
	require.True(t, bySelf(selfSet), "the holder set it")
	require.True(t, bySelf(byToken), "the holder set it, through a token of their own")
	require.True(t, bySelf(unknown), "nothing says who set it")
	require.False(t, bySelf(thenAdmin), "an administrator set it last")
	require.False(t, bySelf(nameOnly), "the holder changed only their name")
}

// TestR045_TwoAccountsCannotTakeOneEmailAtOnce asserts that the check that no
// two accounts share an email holds when two changes race: of two accounts
// asking for one address at the same moment, one gets it.
func TestR045_TwoAccountsCannotTakeOneEmailAtOnce(t *testing.T) {
	ctx := context.Background()
	db, _ := statetest.Connect(t)
	users := state.NewUsers(db)
	require.NoError(t, users.EnsureLocalAdapter(ctx))

	for round := range 5 {
		email := fmt.Sprintf("Contested%d@example.com", round)
		ids := make([]string, 2)
		for i := range ids {
			u, err := users.Create(ctx, state.LocalAdapterID, fmt.Sprintf("racer%d-%d", round, i), "", "", "", false)
			require.NoError(t, err)
			ids[i] = u.ID
		}
		results := make([]error, 2)
		var wg sync.WaitGroup
		for i, userID := range ids {
			wg.Go(func() {
				e := email
				if i == 1 {
					e = strings.ToLower(email)
				}
				results[i] = users.UpdateProfile(ctx, userID, state.Profile{Email: &e, EmailBySelf: true})
			})
		}
		wg.Wait()
		succeeded := 0
		for _, err := range results {
			if err == nil {
				succeeded++
				continue
			}
			require.Contains(t, err.Error(), "Another Pando account already has this email address")
		}
		require.Equal(t, 1, succeeded, "round %d", round)
	}
}
