package state

import (
	"context"
	"time"

	"github.com/trypando/pando/internal/secret"
)

// A prepared database, copied rather than prepared again (issue #31).
//
// Connect replays every migration, then provisions AppRole with a password it
// generates on the spot. That is right for a server, which starts once and
// wants every grant re-applied. It is wrong for a test suite that wants a
// hundred databases: replaying the migrations is most of the cost of each, and
// AppRole is a role of the whole Postgres cluster rather than of one database,
// so two Connects at once change its password under each other's pools.
//
// So a suite prepares one database with PrepareTemplate, makes each test's with
// CREATE DATABASE … TEMPLATE, and connects to the copy with ConnectCopy. The
// copy carries the schema, the seeded rows, and the table grants and ownership,
// all of which live inside a database; AppRole and its password live outside
// it and are set once.

// PrepareTemplate migrates, grants and verifies a database to be copied, and
// returns the passwords it gave AppRole and ArchiverRole.
//
// Nothing may be connected to the database when it is copied, so the caller
// makes its copies after this returns and never connects to it again.
func PrepareTemplate(ctx context.Context, ownerURL string) (Passwords, error) {
	owner, err := waitForPostgres(ctx, ownerURL, 60*time.Second)
	if err != nil {
		return Passwords{}, err
	}
	defer owner.Close()

	if _, err := migrateUp(ctx, ownerURL); err != nil {
		return Passwords{}, err
	}
	passwords, err := provisionRoles(ctx, owner, ownerURL)
	if err != nil {
		return Passwords{}, err
	}
	if err := applyGrants(ctx, owner); err != nil {
		return Passwords{}, err
	}
	if err := verifyAuditImmutability(ctx, owner); err != nil {
		return Passwords{}, err
	}
	return passwords, nil
}

// ConnectCopy connects as AppRole to a copy of a database PrepareTemplate
// prepared, whose password it returned.
//
// The audit log's protection is verified again on the copy rather than
// assumed from the template: it is the one property that must hold of every
// database Pando serves from (R-027).
func ConnectCopy(ctx context.Context, ownerURL string, appPassword secret.Value) (*DB, error) {
	owner, err := waitForPostgres(ctx, ownerURL, 60*time.Second)
	if err != nil {
		return nil, err
	}
	defer owner.Close()

	version, err := readSchemaVersion(ctx, owner)
	if err != nil {
		return nil, err
	}
	if err := verifyAuditImmutability(ctx, owner); err != nil {
		return nil, err
	}
	return connectAsApp(ctx, ownerURL, appPassword, version)
}
