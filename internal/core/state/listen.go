package state

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Listening on a Postgres channel, so a change committed by any replica
// reaches every replica's in-memory copy (issue #72).
//
// NOTIFY is delivered when the notifying transaction commits, to every session
// that LISTENs on the channel, and to none that is not connected at that
// moment. So a listener reports whether it is connected: a copy kept only
// because nothing said it changed is good only while something could have.

// listenBackoff bounds how long a lost listener waits before connecting
// again. Variables so a test need not wait them out.
var (
	listenBackoffMin = 500 * time.Millisecond
	listenBackoffMax = 30 * time.Second
)

// Listen holds a connection of its own LISTENing on channel until ctx ends,
// calling changed for each notification. connected is told true once the
// LISTEN is in place and false whenever the connection is lost; a lost
// connection is opened again, with backoff, and told true again.
//
// Its own connection rather than one from the pool: a pooled connection goes
// back to the pool still listening, and a listener holds its connection for
// as long as the process runs.
func (db *DB) Listen(ctx context.Context, channel string, changed func(payload string), connected func(bool)) {
	wait := listenBackoffMin
	for ctx.Err() == nil {
		up := db.listenOnce(ctx, channel, changed, connected)
		connected(false)
		if up {
			wait = listenBackoffMin
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, listenBackoffMax)
	}
}

// listenOnce listens until the connection fails or ctx ends, and reports
// whether it got as far as listening.
func (db *DB) listenOnce(ctx context.Context, channel string, changed func(string), connected func(bool)) bool {
	conn, err := pgx.ConnectConfig(ctx, db.Config().ConnConfig.Copy())
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
		return false
	}
	connected(true)
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return true
		}
		changed(n.Payload)
	}
}
