package state

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
	"github.com/trypando/pando/internal/secret"
)

// Event is one row of the outbox (issue #50, R-366).
type Event struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	AppID      string         `json:"app_id,omitempty"`
	ActorKind  string         `json:"actor_kind"`
	ActorID    string         `json:"actor_id,omitempty"`
	OnBehalfOf string         `json:"on_behalf_of,omitempty"`
	RequestID  string         `json:"request_id,omitempty"`
	Data       map[string]any `json:"data"`
	OccurredAt time.Time      `json:"occurred_at"`
}

// Execer is a pool or a transaction.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// InsertEvent writes an event through q, which may be the transaction an
// audit row is being written in: the audit writer copies a catalogued action
// here in the same transaction, so the log and the outbox cannot disagree.
func InsertEvent(ctx context.Context, q Execer, e Event) error {
	_, err := insertEvent(ctx, q, e, false)
	return err
}

func insertEvent(ctx context.Context, q Execer, e Event, routed bool) (string, error) {
	data := e.Data
	if data == nil {
		data = map[string]any{}
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return "", errs.Wrap(errs.Internal, "Could not encode an event.", err)
	}
	kind := e.ActorKind
	if kind == "" {
		kind = "system"
	}
	var routedAt any
	if routed {
		routedAt = time.Now().UTC()
	}
	eventID := e.ID
	if eventID == "" {
		eventID = id.New(id.Event)
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO events (id, name, app_id, actor_kind, actor_id, on_behalf_of, request_id, data, routed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		eventID, e.Name, nullable(e.AppID), kind, nullable(e.ActorID), nullable(e.OnBehalfOf),
		nullable(e.RequestID), encoded, routedAt); err != nil {
		return "", errs.Wrap(errs.Internal, "Could not record an event.", err)
	}
	return eventID, nil
}

// Events is the outbox.
type Events struct{ db *DB }

func NewEvents(db *DB) *Events { return &Events{db: db} }

// Emit records an event core raises itself — adapter health, for one.
func (s *Events) Emit(ctx context.Context, e Event) (string, error) {
	return insertEvent(ctx, s.db, e, false)
}

// EmitRouted records an event that goes to one subscription and no other — a
// test delivery — and makes that delivery, in one transaction.
func (s *Events) EmitRouted(ctx context.Context, e Event, subscriptionID string) (Delivery, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Delivery{}, errs.Wrap(errs.Internal, "Could not record an event.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	eventID, err := insertEvent(ctx, tx, e, true)
	if err != nil {
		return Delivery{}, err
	}
	deliveryID := id.New(id.Delivery)
	if _, err := tx.Exec(ctx, `
		INSERT INTO event_deliveries (id, subscription_id, event_id, next_attempt_at)
		VALUES ($1, $2, $3, now())`, deliveryID, subscriptionID, eventID); err != nil {
		return Delivery{}, errs.Wrap(errs.Internal, "Could not queue the delivery.", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Delivery{}, errs.Wrap(errs.Internal, "Could not record an event.", err)
	}
	return Delivery{ID: deliveryID, SubscriptionID: subscriptionID, EventID: eventID, EventName: e.Name, Status: DeliveryPending}, nil
}

const eventColumns = `id, name, coalesce(app_id, ''), actor_kind, coalesce(actor_id, ''),
	coalesce(on_behalf_of, ''), coalesce(request_id, ''), data, occurred_at`

func scanEvent(row pgx.Row) (Event, error) {
	var e Event
	var data []byte
	if err := row.Scan(&e.ID, &e.Name, &e.AppID, &e.ActorKind, &e.ActorID, &e.OnBehalfOf, &e.RequestID, &data, &e.OccurredAt); err != nil {
		return Event{}, err
	}
	if err := json.Unmarshal(data, &e.Data); err != nil {
		return Event{}, err
	}
	e.OccurredAt = e.OccurredAt.UTC()
	return e, nil
}

// Get reads one event.
func (s *Events) Get(ctx context.Context, eventID string) (Event, bool, error) {
	e, err := scanEvent(s.db.QueryRow(ctx, `SELECT `+eventColumns+` FROM events WHERE id = $1`, eventID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, errs.Wrap(errs.Internal, "Could not read the event.", err)
	}
	return e, true, nil
}

// ForApp returns an app's recent events, newest first, older than the event
// ID before when one is given (R-378). As long as the outbox keeps them: this
// is a feed, and the audit log is the history.
func (s *Events) ForApp(ctx context.Context, appID, before string, limit int) ([]Event, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(ctx, `
		SELECT `+eventColumns+` FROM events
		WHERE app_id = $1 AND name <> 'subscription.test' AND ($2 = '' OR id < $2)
		ORDER BY id DESC
		LIMIT $3`, appID, before, limit)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read the app's events.", err)
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read the app's events.", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Route takes up to limit events nobody has routed yet that occurred before
// before (all of them, when it is zero), oldest first, asks
// route which subscriptions each one goes to, and queues a delivery for each,
// all in one transaction. Another Pando routing at the same time skips the
// rows this one holds, so an event is routed once.
//
// An event route cannot decide about is left for the next pass rather than
// marked routed: a database hiccup must not lose an event (R-366).
//
// The cutoff is for a router matching against a cached list of
// subscriptions: an event newer than the list could be meant for a
// subscription made after it, so it waits for the next list.
func (s *Events) Route(ctx context.Context, limit int, before time.Time, route func(context.Context, Event) ([]string, error)) ([]Event, error) {
	if before.IsZero() {
		before = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not route events.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT `+eventColumns+` FROM events
		WHERE routed_at IS NULL AND occurred_at < $2
		ORDER BY seq
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, limit, before)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not route events.", err)
	}
	var pending []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			rows.Close()
			return nil, errs.Wrap(errs.Internal, "Could not route events.", err)
		}
		pending = append(pending, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not route events.", err)
	}

	var routed []Event
	for _, e := range pending {
		subs, err := route(ctx, e)
		if err != nil {
			continue
		}
		for _, sub := range subs {
			if _, err := tx.Exec(ctx, `
				INSERT INTO event_deliveries (id, subscription_id, event_id, next_attempt_at)
				VALUES ($1, $2, $3, now())
				ON CONFLICT (subscription_id, event_id) DO NOTHING`,
				id.New(id.Delivery), sub, e.ID); err != nil {
				return nil, errs.Wrap(errs.Internal, "Could not queue a delivery.", err)
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE events SET routed_at = now() WHERE id = $1`, e.ID); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not route events.", err)
		}
		routed = append(routed, e)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not route events.", err)
	}
	return routed, nil
}

// Prune removes up to limit events older than before that have nothing left
// to deliver, and their deliveries and delivery attempts with them, and
// returns how many it removed. The outbox is not a history: the audit log is
// (R-224, R-366).
//
// A batch at a time, so a backlog of a month's events is removed in short
// transactions the retention job repeats rather than in one that holds locks
// across the whole outbox (issue #72). The cascade into event_deliveries is
// by event_id, which is indexed (migration 000050).
func (s *Events) Prune(ctx context.Context, before time.Time, limit int) (int64, error) {
	tag, err := s.db.Exec(ctx, `
		DELETE FROM events WHERE id IN (
		    SELECT e.id FROM events e
		    WHERE e.occurred_at < $1 AND e.routed_at IS NOT NULL
		      AND NOT EXISTS (SELECT 1 FROM event_deliveries d WHERE d.event_id = e.id AND d.status = 'pending')
		    ORDER BY e.seq
		    LIMIT $2)`,
		before, limit)
	if err != nil {
		return 0, errs.Wrap(errs.Internal, "Could not prune old events.", err)
	}
	return tag.RowsAffected(), nil
}

// --- subscriptions ---------------------------------------------------------

// Subscription destinations.
const (
	DestinationWebhook = "webhook"
	DestinationNotify  = "notify"
)

// Subscription is which events, about what, sent where (R-367).
type Subscription struct {
	ID string `json:"id"`

	// OwnerID is the person who owns it; OwnerTokenID the account token, for
	// one an account token made (R-060). Exactly one is set.
	OwnerID      string `json:"owner_id,omitempty"`
	OwnerTokenID string `json:"owner_token_id,omitempty"`
	OwnerName    string `json:"owner_name,omitempty"`

	AppID       string   `json:"app_id,omitempty"`
	AppName     string   `json:"app_name,omitempty"`
	Events      []string `json:"events"`
	Destination string   `json:"destination"`
	URL         string   `json:"url,omitempty"`
	AdapterID   string   `json:"adapter_id,omitempty"`

	// How a webhook is sent (R-375). Header values are sealed in
	// subscription_secrets; only their names are here.
	Method          string   `json:"method,omitempty"`
	ContentType     string   `json:"content_type,omitempty"`
	PayloadTemplate string   `json:"payload_template,omitempty"`
	HeaderNames     []string `json:"header_names,omitempty"`

	Description         string     `json:"description"`
	Enabled             bool       `json:"enabled"`
	DisabledReason      string     `json:"disabled_reason,omitempty"`
	FailingSince        *time.Time `json:"failing_since,omitempty"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	CreatedBy           string     `json:"created_by"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

// Subscriptions stores subscriptions.
type Subscriptions struct{ db *DB }

func NewSubscriptions(db *DB) *Subscriptions { return &Subscriptions{db: db} }

const subscriptionColumns = `s.id, coalesce(s.owner_user_id, ''), coalesce(s.owner_token_id, ''),
	coalesce(nullif(u.display_name, ''), u.email, u.external_id, t.name, ''),
	coalesce(s.app_id, ''), coalesce(a.name, ''), s.events, s.destination, coalesce(s.url, ''),
	coalesce(s.adapter_id, ''), s.method, s.content_type, s.payload_template, s.header_names,
	s.description, s.enabled, s.disabled_reason, s.failing_since,
	s.consecutive_failures, s.created_by, s.created_at, s.updated_at`

const subscriptionFrom = ` FROM subscriptions s
	LEFT JOIN users u ON u.id = s.owner_user_id
	LEFT JOIN tokens t ON t.id = s.owner_token_id
	LEFT JOIN apps a ON a.id = s.app_id`

func scanSubscription(row pgx.Row) (Subscription, error) {
	var s Subscription
	err := row.Scan(&s.ID, &s.OwnerID, &s.OwnerTokenID, &s.OwnerName, &s.AppID, &s.AppName, &s.Events,
		&s.Destination, &s.URL, &s.AdapterID, &s.Method, &s.ContentType, &s.PayloadTemplate, &s.HeaderNames,
		&s.Description, &s.Enabled, &s.DisabledReason, &s.FailingSince,
		&s.ConsecutiveFailures, &s.CreatedBy, &s.CreatedAt, &s.UpdatedAt)
	if s.Events == nil {
		s.Events = []string{}
	}
	if s.Destination != DestinationWebhook {
		s.Method, s.ContentType = "", ""
	}
	return s, err
}

// Create stores a subscription. s.ID must be set.
func (s *Subscriptions) Create(ctx context.Context, sub Subscription) (Subscription, error) {
	method, contentType := sub.Method, sub.ContentType
	if method == "" {
		method = "POST"
	}
	if contentType == "" {
		contentType = "application/json"
	}
	headers := sub.HeaderNames
	if headers == nil {
		headers = []string{}
	}
	if _, err := s.db.Exec(ctx, `
		INSERT INTO subscriptions (id, owner_user_id, owner_token_id, app_id, events, destination, url, adapter_id,
		                           method, content_type, payload_template, header_names, description, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		sub.ID, nullable(sub.OwnerID), nullable(sub.OwnerTokenID), nullable(sub.AppID), sub.Events, sub.Destination,
		nullable(sub.URL), nullable(sub.AdapterID), method, contentType, sub.PayloadTemplate, headers,
		sub.Description, sub.CreatedBy); err != nil {
		return Subscription{}, errs.Wrap(errs.Internal, "Could not save the subscription.", err)
	}
	got, _, err := s.Get(ctx, sub.ID)
	return got, err
}

// Get reads one subscription.
func (s *Subscriptions) Get(ctx context.Context, subscriptionID string) (Subscription, bool, error) {
	sub, err := scanSubscription(s.db.QueryRow(ctx, `SELECT `+subscriptionColumns+subscriptionFrom+` WHERE s.id = $1`, subscriptionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Subscription{}, false, nil
	}
	if err != nil {
		return Subscription{}, false, errs.Wrap(errs.Internal, "Could not read the subscription.", err)
	}
	return sub, true, nil
}

// SubscriptionFilter narrows a list. Empty fields do not narrow.
type SubscriptionFilter struct {
	// OwnerID is a person's ID or an account token's: both columns are
	// matched, and the two kinds of ID cannot collide.
	OwnerID string
	AppID   string

	// InstallOnly keeps only subscriptions with no app.
	InstallOnly bool

	// EnabledOnly keeps only subscriptions that are on.
	EnabledOnly bool
}

// Enabled returns every enabled subscription, and the database's time just
// before it read them: a subscription made before asOf is in the list, give
// or take a transaction still committing (issue #72, the dispatcher's cache).
func (s *Subscriptions) Enabled(ctx context.Context) ([]Subscription, time.Time, error) {
	var asOf time.Time
	if err := s.db.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&asOf); err != nil {
		return nil, time.Time{}, errs.Wrap(errs.Internal, "Could not list subscriptions.", err)
	}
	subs, err := s.List(ctx, SubscriptionFilter{EnabledOnly: true})
	return subs, asOf, err
}

// List returns subscriptions, newest first.
func (s *Subscriptions) List(ctx context.Context, f SubscriptionFilter) ([]Subscription, error) {
	rows, err := s.db.Query(ctx, `SELECT `+subscriptionColumns+subscriptionFrom+`
		WHERE ($1 = '' OR s.owner_user_id = $1 OR s.owner_token_id = $1)
		  AND ($2 = '' OR s.app_id = $2)
		  AND (NOT $3 OR s.app_id IS NULL)
		  AND (NOT $4 OR s.enabled)
		ORDER BY s.created_at DESC`, f.OwnerID, f.AppID, f.InstallOnly, f.EnabledOnly)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not list subscriptions.", err)
	}
	defer rows.Close()
	out := []Subscription{}
	for rows.Next() {
		sub, err := scanSubscription(rows)
		if err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not list subscriptions.", err)
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// SubscriptionPatch is a partial update. Nil fields are left alone.
type SubscriptionPatch struct {
	Events          *[]string
	URL             *string
	AdapterID       *string
	Description     *string
	Enabled         *bool
	Method          *string
	ContentType     *string
	PayloadTemplate *string
	HeaderNames     *[]string
}

// Update applies a patch. Turning a subscription on clears the record of its
// failures, so an endpoint somebody fixed starts with a clean slate.
func (s *Subscriptions) Update(ctx context.Context, subscriptionID string, p SubscriptionPatch) (Subscription, error) {
	var events, headers []string
	if p.Events != nil {
		events = *p.Events
	}
	if p.HeaderNames != nil {
		headers = *p.HeaderNames
		if headers == nil {
			headers = []string{}
		}
	}
	_, err := s.db.Exec(ctx, `
		UPDATE subscriptions SET
			events = CASE WHEN $2 THEN $3 ELSE events END,
			url = CASE WHEN $4 THEN $5 ELSE url END,
			adapter_id = CASE WHEN $6 THEN $7 ELSE adapter_id END,
			description = CASE WHEN $8 THEN $9 ELSE description END,
			enabled = CASE WHEN $10 THEN $11 ELSE enabled END,
			disabled_reason = CASE WHEN $10 AND $11 THEN '' ELSE disabled_reason END,
			failing_since = CASE WHEN $10 AND $11 THEN NULL ELSE failing_since END,
			consecutive_failures = CASE WHEN $10 AND $11 THEN 0 ELSE consecutive_failures END,
			method = CASE WHEN $12 THEN $13 ELSE method END,
			content_type = CASE WHEN $14 THEN $15 ELSE content_type END,
			payload_template = CASE WHEN $16 THEN $17 ELSE payload_template END,
			header_names = CASE WHEN $18 THEN $19 ELSE header_names END,
			updated_at = now()
		WHERE id = $1`,
		subscriptionID,
		p.Events != nil, events,
		p.URL != nil, deref(p.URL),
		p.AdapterID != nil, deref(p.AdapterID),
		p.Description != nil, deref(p.Description),
		p.Enabled != nil, p.Enabled != nil && *p.Enabled,
		p.Method != nil, deref(p.Method),
		p.ContentType != nil, deref(p.ContentType),
		p.PayloadTemplate != nil, deref(p.PayloadTemplate),
		p.HeaderNames != nil, headers)
	if err != nil {
		return Subscription{}, errs.Wrap(errs.Internal, "Could not update the subscription.", err)
	}
	got, _, err := s.Get(ctx, subscriptionID)
	return got, err
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Delete removes a subscription, its key and its deliveries.
func (s *Subscriptions) Delete(ctx context.Context, subscriptionID string) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM subscriptions WHERE id = $1`, subscriptionID); err != nil {
		return errs.Wrap(errs.Internal, "Could not delete the subscription.", err)
	}
	return nil
}

// Succeeded clears a subscription's run of failures.
func (s *Subscriptions) Succeeded(ctx context.Context, subscriptionID string) error {
	_, err := s.db.Exec(ctx, `
		UPDATE subscriptions SET failing_since = NULL, consecutive_failures = 0
		WHERE id = $1 AND (failing_since IS NOT NULL OR consecutive_failures <> 0)`, subscriptionID)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not update the subscription.", err)
	}
	return nil
}

// Failed counts one more failed attempt and returns when the run of failures
// began and how long it is.
func (s *Subscriptions) Failed(ctx context.Context, subscriptionID string, at time.Time) (time.Time, int, error) {
	var since time.Time
	var count int
	err := s.db.QueryRow(ctx, `
		UPDATE subscriptions SET failing_since = coalesce(failing_since, $2),
		       consecutive_failures = consecutive_failures + 1
		WHERE id = $1
		RETURNING failing_since, consecutive_failures`, subscriptionID, at).Scan(&since, &count)
	if err != nil {
		return time.Time{}, 0, errs.Wrap(errs.Internal, "Could not update the subscription.", err)
	}
	return since, count, nil
}

// Disable turns a subscription off and says why. It reports whether this call
// is the one that did it, so the owner is told once.
func (s *Subscriptions) Disable(ctx context.Context, subscriptionID, reason string) (bool, error) {
	tag, err := s.db.Exec(ctx, `
		UPDATE subscriptions SET enabled = false, disabled_reason = $2, updated_at = now()
		WHERE id = $1 AND enabled`, subscriptionID, reason)
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not turn the subscription off.", err)
	}
	return tag.RowsAffected() == 1, nil
}

// --- deliveries ------------------------------------------------------------

// Delivery statuses.
const (
	DeliveryPending   = "pending"
	DeliverySucceeded = "succeeded"
	DeliveryFailed    = "failed"
)

// Delivery is one event to one subscription.
type Delivery struct {
	ID             string            `json:"id"`
	SubscriptionID string            `json:"subscription_id"`
	EventID        string            `json:"event_id"`
	EventName      string            `json:"event"`
	Status         string            `json:"status"`
	Attempts       int               `json:"attempts"`
	NextAttemptAt  *time.Time        `json:"next_attempt_at,omitempty"`
	LastAttemptAt  *time.Time        `json:"last_attempt_at,omitempty"`
	LastStatusCode *int              `json:"last_status_code,omitempty"`
	LastError      string            `json:"last_error,omitempty"`
	RedeliveredBy  string            `json:"redelivered_by,omitempty"`
	RoundBase      int               `json:"-"`
	CreatedAt      time.Time         `json:"created_at"`
	AttemptLog     []DeliveryAttempt `json:"attempt_log,omitempty"`
}

// DeliveryAttempt is one try.
type DeliveryAttempt struct {
	Attempt     int       `json:"attempt"`
	AttemptedAt time.Time `json:"attempted_at"`
	StatusCode  *int      `json:"status_code,omitempty"`
	Error       string    `json:"error,omitempty"`
	DurationMS  int       `json:"duration_ms"`
}

// Deliveries stores deliveries and their attempts.
type Deliveries struct{ db *DB }

func NewDeliveries(db *DB) *Deliveries { return &Deliveries{db: db} }

const deliveryColumns = `d.id, d.subscription_id, d.event_id, e.name, d.status, d.attempts, d.next_attempt_at,
	d.last_attempt_at, d.last_status_code, d.last_error, coalesce(d.redelivered_by, ''), d.round_base, d.created_at`

func scanDelivery(row pgx.Row) (Delivery, error) {
	var d Delivery
	err := row.Scan(&d.ID, &d.SubscriptionID, &d.EventID, &d.EventName, &d.Status, &d.Attempts,
		&d.NextAttemptAt, &d.LastAttemptAt, &d.LastStatusCode, &d.LastError, &d.RedeliveredBy,
		&d.RoundBase, &d.CreatedAt)
	return d, err
}

// Claim takes up to limit deliveries that are due, on subscriptions that are
// on, and pushes each one's next attempt out by lease. A Pando that stops
// mid-send leaves the delivery to be retried when the lease runs out: at least
// once, never zero times (R-366).
func (s *Deliveries) Claim(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]Delivery, error) {
	rows, err := s.db.Query(ctx, `
		WITH due AS (
			SELECT d.id FROM event_deliveries d
			JOIN subscriptions s ON s.id = d.subscription_id
			WHERE d.status = 'pending' AND d.next_attempt_at <= $1 AND s.enabled
			ORDER BY d.next_attempt_at
			LIMIT $3
			FOR UPDATE OF d SKIP LOCKED
		)
		UPDATE event_deliveries d SET next_attempt_at = $2
		FROM due, events e
		WHERE d.id = due.id AND e.id = d.event_id
		RETURNING `+deliveryColumns, now, now.Add(lease), limit)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read deliveries that are due.", err)
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read deliveries that are due.", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Record writes an attempt and where the delivery stands after it: status, and
// when to try next if it is still pending.
func (s *Deliveries) Record(ctx context.Context, deliveryID string, a DeliveryAttempt, status string, next *time.Time) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record the delivery attempt.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO delivery_attempts (delivery_id, attempt, attempted_at, status_code, error, duration_ms)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		deliveryID, a.Attempt, a.AttemptedAt, a.StatusCode, a.Error, a.DurationMS); err != nil {
		return errs.Wrap(errs.Internal, "Could not record the delivery attempt.", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE event_deliveries SET attempts = $2, status = $3, next_attempt_at = $4,
		       last_attempt_at = $5, last_status_code = $6, last_error = $7
		WHERE id = $1`,
		deliveryID, a.Attempt, status, next, a.AttemptedAt, a.StatusCode, a.Error); err != nil {
		return errs.Wrap(errs.Internal, "Could not record the delivery attempt.", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return errs.Wrap(errs.Internal, "Could not record the delivery attempt.", err)
	}
	return nil
}

// Drop ends a delivery without an attempt — its owner can no longer see what
// it is about (R-368). Recorded, so the log says why nothing was sent.
func (s *Deliveries) Drop(ctx context.Context, deliveryID, reason string) error {
	_, err := s.db.Exec(ctx, `
		UPDATE event_deliveries SET status = 'failed', next_attempt_at = NULL, last_error = $2
		WHERE id = $1`, deliveryID, reason)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not update the delivery.", err)
	}
	return nil
}

// List returns one subscription's deliveries, newest first, before the
// delivery ID given (a cursor) when one is.
func (s *Deliveries) List(ctx context.Context, subscriptionID, before string, limit int) ([]Delivery, error) {
	rows, err := s.db.Query(ctx, `
		SELECT `+deliveryColumns+`
		FROM event_deliveries d JOIN events e ON e.id = d.event_id
		WHERE d.subscription_id = $1 AND ($2 = '' OR d.id < $2)
		ORDER BY d.id DESC
		LIMIT $3`, subscriptionID, before, limit)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not list deliveries.", err)
	}
	defer rows.Close()
	out := []Delivery{}
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not list deliveries.", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Get reads one delivery and every attempt at it.
func (s *Deliveries) Get(ctx context.Context, deliveryID string) (Delivery, bool, error) {
	d, err := scanDelivery(s.db.QueryRow(ctx, `
		SELECT `+deliveryColumns+`
		FROM event_deliveries d JOIN events e ON e.id = d.event_id
		WHERE d.id = $1`, deliveryID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Delivery{}, false, nil
	}
	if err != nil {
		return Delivery{}, false, errs.Wrap(errs.Internal, "Could not read the delivery.", err)
	}
	rows, err := s.db.Query(ctx, `
		SELECT attempt, attempted_at, status_code, error, duration_ms
		FROM delivery_attempts WHERE delivery_id = $1 ORDER BY attempt`, deliveryID)
	if err != nil {
		return Delivery{}, false, errs.Wrap(errs.Internal, "Could not read the delivery.", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a DeliveryAttempt
		if err := rows.Scan(&a.Attempt, &a.AttemptedAt, &a.StatusCode, &a.Error, &a.DurationMS); err != nil {
			return Delivery{}, false, errs.Wrap(errs.Internal, "Could not read the delivery.", err)
		}
		d.AttemptLog = append(d.AttemptLog, a)
	}
	return d, true, rows.Err()
}

// Redeliver queues a delivery again, now, with the whole retry schedule ahead
// of it.
func (s *Deliveries) Redeliver(ctx context.Context, deliveryID, by string) error {
	_, err := s.db.Exec(ctx, `
		UPDATE event_deliveries SET status = 'pending', next_attempt_at = now(),
		       redelivered_by = $2, round_base = attempts
		WHERE id = $1`, deliveryID, by)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not queue the delivery again.", err)
	}
	return nil
}

// --- subscription secrets --------------------------------------------------

// SigningKeyField is the field a webhook's signing key is kept under.
const SigningKeyField = "signing_key"

// HeaderField is the field a custom header's value is kept under.
func HeaderField(name string) string { return "header:" + name }

// SubscriptionSecrets stores a webhook's secrets — its signing key and the
// values of its custom headers — sealed by the secrets adapter (R-371, R-375,
// R-190). The same arrangement as AdapterCredentials: ciphertext or an
// external reference, never the value, and no column on the subscription a
// value could be put in.
type SubscriptionSecrets struct {
	db         *DB
	adapter    api.SecretsAdapter
	adapterRef string
}

func NewSubscriptionSecrets(db *DB, adapter api.SecretsAdapter, adapterRef string) *SubscriptionSecrets {
	return &SubscriptionSecrets{db: db, adapter: adapter, adapterRef: adapterRef}
}

// subscriptionSecretRef scopes a secret to its subscription. "subscription:"
// cannot collide with an app ID or an adapter's scope, so one sealed value
// cannot be replayed as another kind of secret.
func subscriptionSecretRef(subscriptionID, field string) api.SecretRef {
	return api.SecretRef{AppID: "subscription:" + subscriptionID, Key: field}
}

// Put stores or replaces one secret.
func (k *SubscriptionSecrets) Put(ctx context.Context, subscriptionID, field string, v secret.Value) error {
	if k == nil || k.adapter == nil {
		return errs.New(errs.StateInvalid,
			"Pando has no secrets adapter configured, so it cannot keep a webhook's signing key or headers.").
			WithRemedy("Configure a secrets adapter, restart Pando, and try again.")
	}
	stored, err := k.adapter.Put(ctx, subscriptionSecretRef(subscriptionID, field), v)
	if err != nil {
		return err
	}
	_, err = k.db.Exec(ctx, `
		INSERT INTO subscription_secrets AS t (subscription_id, field, adapter_ref, ciphertext, external_ref)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (subscription_id, field) DO UPDATE SET
			adapter_ref = EXCLUDED.adapter_ref, ciphertext = EXCLUDED.ciphertext,
			external_ref = EXCLUDED.external_ref, version = t.version + 1, updated_at = now()`,
		subscriptionID, field, k.adapterRef, stored.Ciphertext, nullable(stored.Handle))
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not store the webhook's secret.", err)
	}
	return nil
}

// DeleteHeaders removes every stored header value, before a new set is put.
func (k *SubscriptionSecrets) DeleteHeaders(ctx context.Context, subscriptionID string) error {
	if _, err := k.db.Exec(ctx,
		`DELETE FROM subscription_secrets WHERE subscription_id = $1 AND field LIKE 'header:%'`, subscriptionID); err != nil {
		return errs.Wrap(errs.Internal, "Could not replace the webhook's headers.", err)
	}
	return nil
}

// All opens every secret a subscription has, keyed by field, for one send.
func (k *SubscriptionSecrets) All(ctx context.Context, subscriptionID string) (map[string]secret.Value, error) {
	rows, err := k.db.Query(ctx, `
		SELECT field, ciphertext, external_ref FROM subscription_secrets WHERE subscription_id = $1`, subscriptionID)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read the webhook's secrets.", err)
	}
	type sealed struct {
		field      string
		ciphertext []byte
		handle     *string
	}
	var all []sealed
	for rows.Next() {
		var s sealed
		if err := rows.Scan(&s.field, &s.ciphertext, &s.handle); err != nil {
			rows.Close()
			return nil, errs.Wrap(errs.Internal, "Could not read the webhook's secrets.", err)
		}
		all = append(all, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read the webhook's secrets.", err)
	}
	out := make(map[string]secret.Value, len(all))
	if len(all) == 0 {
		return out, nil
	}
	if k.adapter == nil {
		return nil, errs.New(errs.StateInvalid,
			"A webhook's secrets are stored and no secrets adapter is configured to open them.")
	}
	for _, s := range all {
		ref := subscriptionSecretRef(subscriptionID, s.field)
		stored := api.StoredRef{AppID: ref.AppID, Key: ref.Key, Ciphertext: s.ciphertext}
		if s.handle != nil {
			stored.Handle = *s.handle
		}
		v, err := k.adapter.Get(ctx, stored)
		if err != nil {
			return nil, err
		}
		out[s.field] = v
	}
	return out, nil
}

// --- preferences -----------------------------------------------------------

// NotificationPreference is one choice a person made: this kind of
// notification, on this channel, on or off (R-373).
type NotificationPreference struct {
	Kind    string `json:"kind"`
	Channel string `json:"channel"`
	Enabled bool   `json:"enabled"`
}

// NotificationPreferences stores those choices.
type NotificationPreferences struct{ db *DB }

func NewNotificationPreferences(db *DB) *NotificationPreferences {
	return &NotificationPreferences{db: db}
}

// List returns one person's choices. Anything absent is the default.
func (s *NotificationPreferences) List(ctx context.Context, userID string) ([]NotificationPreference, error) {
	rows, err := s.db.Query(ctx, `
		SELECT kind, channel, enabled FROM notification_preferences
		WHERE user_id = $1 ORDER BY kind, channel`, userID)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read notification preferences.", err)
	}
	defer rows.Close()
	out := []NotificationPreference{}
	for rows.Next() {
		var p NotificationPreference
		if err := rows.Scan(&p.Kind, &p.Channel, &p.Enabled); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read notification preferences.", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Set records choices, replacing any earlier choice for the same kind and
// channel.
func (s *NotificationPreferences) Set(ctx context.Context, userID string, prefs []NotificationPreference) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not save notification preferences.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, p := range prefs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO notification_preferences (user_id, kind, channel, enabled)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (user_id, kind, channel) DO UPDATE SET enabled = excluded.enabled, updated_at = now()`,
			userID, p.Kind, p.Channel, p.Enabled); err != nil {
			return errs.Wrap(errs.Internal, "Could not save notification preferences.", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return errs.Wrap(errs.Internal, "Could not save notification preferences.", err)
	}
	return nil
}
