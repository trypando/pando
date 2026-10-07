package state

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/egress"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
)

// Deployment statuses.
const (
	DeployPending    = "pending"
	DeployBuilding   = "building"
	DeployApplying   = "applying"
	DeploySucceeded  = "succeeded"
	DeployFailed     = "failed"
	DeploySuperseded = "superseded"

	// Deploy approval (R-154 – R-159). A deploy that needs approval is a
	// deployment that waits in awaiting_approval, tied to one spec revision
	// (R-156), and moves to pending — the ordinary path — once it has its
	// approvals. Rejected and expired are where a request that never ran
	// ends. None of the three is in flight: a request waiting for somebody
	// does not stop the app being deployed some other way.
	DeployAwaitingApproval = "awaiting_approval"
	DeployRejected         = "rejected"
	DeployExpired          = "expired"
)

// Decisions on a deploy that needs approval.
const (
	DecisionApprove = "approve"
	DecisionReject  = "reject"
)

// Deployment triggers.
const (
	TriggerManual   = "manual"
	TriggerRollback = "rollback"
)

// Deployment is one attempt to make an app match a spec.
//
// Its own table, separate from the app, because a build failure fails the
// deployment and leaves the app untouched (R-146). Collapsing them would make
// that distinction impossible to express.
type Deployment struct {
	ID      string `json:"id"`
	AppID   string `json:"app_id"`
	SpecID  string `json:"spec_id"`
	Trigger string `json:"trigger"`
	Status  string `json:"status"`

	// ResultState is what the app was doing when this deploy finished:
	// "running", or "degraded" when it started and never reported healthy.
	// Empty for a deploy that failed before it got that far.
	//
	// The deploy's own status answers "did Pando do the work"; this answers
	// "did the app come up", which is what somebody reading a list of deploys
	// is asking. Three rows reading "Deployed" for an app that had never
	// served a request is a true answer to the wrong question.
	ResultState string `json:"result_state,omitempty"`

	ErrorCode   string     `json:"error_code,omitempty"`
	ErrorDetail string     `json:"error_detail,omitempty"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	CreatedBy   string     `json:"created_by"`

	// SpecRevision is the revision number SpecID is, and RequestedByName who
	// CreatedBy is, for showing: a person deciding whether to approve a
	// deploy wants "revision 7, asked for by Ada", not two IDs.
	SpecRevision    int    `json:"spec_revision,omitempty"`
	RequestedByName string `json:"requested_by_name,omitempty"`

	// Deploy approval (R-154 – R-159), on a deploy that needed it. The count
	// and the expiry are fixed when the request is made, so a policy edit
	// does not move a waiting request's goalposts.
	ApprovalsRequired int                `json:"approvals_required,omitempty"`
	ApprovalExpiresAt *time.Time         `json:"approval_expires_at,omitempty"`
	ApprovalReasons   []ApprovalReason   `json:"approval_reasons,omitempty"`
	Approvals         []ApprovalDecision `json:"approvals,omitempty"`

	// CanDecide is whether the caller may approve or reject this deploy. Set
	// by the approval service, per caller, on a deploy that is waiting; the
	// store never fills it in.
	CanDecide bool `json:"can_decide,omitempty"`
}

// ApprovalReason is why a deploy needed approval. The store holds the reason;
// the approval service, which owns the wording, fills in Message.
type ApprovalReason struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// ApprovalDecision is one person's answer to a deploy that needs approval.
type ApprovalDecision struct {
	PrincipalID   string    `json:"principal_id"`
	PrincipalName string    `json:"principal_name"`
	Decision      string    `json:"decision"`
	Comment       string    `json:"comment,omitempty"`
	DecidedAt     time.Time `json:"decided_at"`
}

// AwaitingApproval is a deploy waiting for approval, with the app it is for:
// the list of everything waiting across apps (GET /approvals).
type AwaitingApproval struct {
	Deployment
	AppName string `json:"app_name"`
	AppSlug string `json:"app_slug"`
}

// Deployments stores deployment records.
type Deployments struct{ db *DB }

func NewDeployments(db *DB) *Deployments { return &Deployments{db: db} }

// Create records a new deployment, queued: pending with no replica running it.
// Any replica's deploy queue claims it (Claim) when it has room (issue #72,
// O-32).
func (d *Deployments) Create(ctx context.Context, appID, specID, trigger, createdBy string) (Deployment, error) {
	dep := Deployment{
		ID:        id.New(id.Deployment),
		AppID:     appID,
		SpecID:    specID,
		Trigger:   trigger,
		Status:    DeployPending,
		CreatedBy: createdBy,
	}
	err := d.db.QueryRow(ctx, `
		INSERT INTO deployments (id, app_id, spec_id, trigger, status, created_by)
		VALUES ($1, $2, $3, $4, 'pending', $5) RETURNING started_at`,
		dep.ID, appID, specID, trigger, createdBy).Scan(&dep.StartedAt)
	if err != nil {
		return Deployment{}, errs.Wrap(errs.Internal, "Could not start the deploy.", err)
	}
	return dep, nil
}

// SetStatus advances a deployment.
//
// Only this replica's deployment, or one nobody has claimed: a replica that was
// taken for dead and whose deploy another replica has since claimed must not
// move it under the one now running it (fenced, as Finish is).
func (d *Deployments) SetStatus(ctx context.Context, deploymentID, status string) error {
	_, err := d.db.Exec(ctx, `UPDATE deployments SET status = $2 WHERE id = $1 AND `+fenced(3),
		deploymentID, status, d.db.replica)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not update the deploy.", err)
	}
	return nil
}

// Finish closes a deployment out.
// SetResultState records what the app was doing when the deploy finished.
func (d *Deployments) SetResultState(ctx context.Context, deploymentID, appState string) error {
	_, err := d.db.Exec(ctx,
		`UPDATE deployments SET result_state = NULLIF($2, '') WHERE id = $1`,
		deploymentID, appState)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record how the app came up.", err)
	}
	return nil
}

func (d *Deployments) Finish(ctx context.Context, deploymentID, status, errorCode, message string) error {
	var detail any
	if message != "" {
		encoded, err := json.Marshal(map[string]string{"message": message})
		if err == nil {
			detail = encoded
		}
	}
	_, err := d.db.Exec(ctx, `
		UPDATE deployments SET status = $2, error_code = $3, error_detail = $4, finished_at = now()
		WHERE id = $1 AND `+fenced(5), deploymentID, status, nullable(errorCode), detail, d.db.replica)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not update the deploy.", err)
	}
	return nil
}

// fenced is true of a deployment this replica may write the status of: one it
// claimed, or one nobody has (a deploy run directly rather than from the
// queue). The replica's ID is the query's parameter number param.
func fenced(param int) string {
	return `(replica_id IS NULL OR replica_id = $` + strconv.Itoa(param) + `)`
}

// MaxAttempts is how many times a deploy or a detection is started before a
// replica stopping under it is taken as the reason, rather than bad luck [P].
// A build that takes its replica down with it (out of memory, say) would
// otherwise move from replica to replica until every one had fallen over.
const MaxAttempts = 3

// Claim takes up to limit queued deployments for this replica, oldest first,
// and returns them (issue #72, O-32).
//
// FOR UPDATE SKIP LOCKED, so replicas claiming at once take different rows and
// none waits on another; claiming stamps replica_id, which is what makes a
// deployment this replica's to run, to fence (SetStatus, Finish) and to serve
// the live log of. The claimant's heartbeat is the lease: RecoverInFlight puts
// back what a stopped replica had claimed.
func (d *Deployments) Claim(ctx context.Context, limit int) ([]Deployment, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := d.db.Query(ctx, `
		WITH next AS (
		    SELECT id FROM deployments
		    WHERE status = $3 AND replica_id IS NULL
		    ORDER BY started_at, id
		    LIMIT $2
		    FOR UPDATE SKIP LOCKED)
		UPDATE deployments d SET replica_id = $1, claimed_at = now(), attempts = d.attempts + 1
		FROM next WHERE d.id = next.id
		RETURNING `+deploymentColumns, d.db.replica, limit, DeployPending)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not take deploys from the queue.", err)
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		dep, err := scanDeployment(rows)
		if err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not take deploys from the queue.", err)
		}
		out = append(out, dep)
	}
	return out, rows.Err()
}

// Release puts deployments this replica claimed back in the queue, at a
// shutdown that stopped them part-way. Not counted as an attempt: a rolling
// restart is not the deploy's fault. One that finished first is left alone.
func (d *Deployments) Release(ctx context.Context, deploymentIDs []string) (int64, error) {
	if len(deploymentIDs) == 0 {
		return 0, nil
	}
	tag, err := d.db.Exec(ctx, `
		UPDATE deployments
		SET status = $3, replica_id = NULL, claimed_at = NULL, attempts = greatest(attempts - 1, 0)
		WHERE id = ANY($1) AND replica_id = $2 AND status IN ($3, $4, $5)`,
		deploymentIDs, d.db.replica, DeployPending, DeployBuilding, DeployApplying)
	if err != nil {
		return 0, errs.Wrap(errs.Internal, "Could not return deploys to the queue.", err)
	}
	return tag.RowsAffected(), nil
}

// QueueDepth is how many deployments are waiting for a replica to take them.
func (d *Deployments) QueueDepth(ctx context.Context) (int, error) {
	var n int
	err := d.db.QueryRow(ctx,
		`SELECT count(*) FROM deployments WHERE status = $1 AND replica_id IS NULL`, DeployPending).Scan(&n)
	if err != nil {
		return 0, errs.Wrap(errs.Internal, "Could not read the deploy queue.", err)
	}
	return n, nil
}

// Waiting reports whether a deployment is queued and nobody has claimed it.
func (d *Deployments) Waiting(ctx context.Context, deploymentID string) (bool, error) {
	var waiting bool
	err := d.db.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM deployments
		WHERE id = $1 AND status = $2 AND replica_id IS NULL)`, deploymentID, DeployPending).Scan(&waiting)
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not read the deploy.", err)
	}
	return waiting, nil
}

// RecoverInFlight puts back in the queue every deployment claimed by a Pando
// process that is no longer running, and records as interrupted one that has
// been started MaxAttempts times. It returns how many it did either to.
//
// Resume rather than record-interrupted (issue #72, O-32): every step a deploy
// takes before it commits is safe to take again — fetching the pinned commit
// and building it produce the same image, a scan of the same source is reused,
// provisioning finds the instance it made, and applying a bundle converges on
// the spec whatever state the last attempt left it in. So the deploy starts
// again from the top on whichever replica claims it next, and its user sees
// it finish rather than a failure that says "deploy again".
//
// Only a stopped or silent process's work, never a live one's: a deploy
// belongs to the replica that claimed it until that replica stops
// heartbeating. Queued work nobody has claimed is nobody's to recover and is
// left in the queue. Called at startup and by the leader's sweeper.
func (d *Deployments) RecoverInFlight(ctx context.Context) (int64, error) {
	detail, err := json.Marshal(map[string]string{
		"message": "Pando stopped while this deploy was under way, " + strconv.Itoa(MaxAttempts) +
			" times, so it was not started again. Deploy again; if it stops Pando again, check the build's memory use.",
	})
	if err != nil {
		return 0, errs.Wrap(errs.Internal, "Could not record interrupted deploys.", err)
	}
	// A row from before the queue existed with no replica recorded, in
	// building or applying, is a dead process's too.
	claimed := `(d.status IN ($2, $3) OR (d.status = $1 AND d.replica_id IS NOT NULL))`
	failed, err := d.db.Exec(ctx, `
		UPDATE deployments d
		SET status = $4, error_code = $5, error_detail = $6, finished_at = now()
		WHERE `+claimed+` AND d.attempts >= $7 AND `+orphaned(8),
		DeployPending, DeployBuilding, DeployApplying,
		DeployFailed, string(errs.StateInvalid), detail, MaxAttempts, ReplicaStale.Seconds())
	if err != nil {
		return 0, errs.Wrap(errs.Internal, "Could not record interrupted deploys.", err)
	}
	requeued, err := d.db.Exec(ctx, `
		UPDATE deployments d
		SET status = $1, replica_id = NULL, claimed_at = NULL
		WHERE `+claimed+` AND `+orphaned(4),
		DeployPending, DeployBuilding, DeployApplying, ReplicaStale.Seconds())
	if err != nil {
		return 0, errs.Wrap(errs.Internal, "Could not return interrupted deploys to the queue.", err)
	}
	return failed.RowsAffected() + requeued.RowsAffected(), nil
}

// orphaned is true of a row d whose replica_id names no live replica: a row
// from before replicas were recorded, or one started by a process that has
// stopped or gone silent. The staleness window is the query's last parameter
// (param), in seconds.
func orphaned(param int) string {
	return `NOT EXISTS (
	SELECT 1 FROM pando_replicas r
	WHERE r.id = d.replica_id AND r.stopped_at IS NULL
	  AND r.heartbeat_at > now() - make_interval(secs => $` + strconv.Itoa(param) + `))`
}

// Runner names the replica running a deployment, or "" when none is recorded.
// It is where the deploy's live log is (deploy.LogStore).
func (d *Deployments) Runner(ctx context.Context, deploymentID string) (string, error) {
	var replica *string
	err := d.db.QueryRow(ctx, `SELECT replica_id FROM deployments WHERE id = $1`, deploymentID).Scan(&replica)
	if errors.Is(err, pgx.ErrNoRows) || replica == nil {
		return "", nil
	}
	if err != nil {
		return "", errs.Wrap(errs.Internal, "Could not read which Pando process ran the deploy.", err)
	}
	return *replica, nil
}

// ByID returns one deployment.
func (d *Deployments) ByID(ctx context.Context, deploymentID string) (Deployment, bool, error) {
	dep, err := scanDeployment(d.db.QueryRow(ctx, `
		SELECT `+deploymentColumns+` FROM deployments d WHERE d.id = $1`, deploymentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Deployment{}, false, nil
	}
	if err != nil {
		return Deployment{}, false, errs.Wrap(errs.Internal, "Could not read the deploy.", err)
	}
	decisions, err := d.Decisions(ctx, []string{dep.ID})
	if err != nil {
		return Deployment{}, false, err
	}
	dep.Approvals = decisions[dep.ID]
	return dep, true, nil
}

// deploymentColumns is what scanDeployment reads, from a table aliased d.
const deploymentColumns = `d.id, d.app_id, d.spec_id, d.trigger, d.status, coalesce(d.result_state, ''),
	d.error_code, d.error_detail, d.started_at, d.finished_at, d.created_by,
	coalesce(d.approvals_required, 0), d.approval_expires_at, coalesce(d.approval_reasons, '{}'),
	coalesce((SELECT r.revision FROM spec_revisions r WHERE r.id = d.spec_id), 0),
	coalesce((SELECT coalesce(nullif(u.display_name, ''), nullif(u.email, ''), u.external_id)
	          FROM users u WHERE u.id = d.created_by),
	         (SELECT t.name FROM tokens t WHERE t.id = d.created_by),
	         d.created_by)`

func scanDeployment(row pgx.Row, extra ...any) (Deployment, error) {
	var dep Deployment
	var code *string
	var detail []byte
	var reasons []string
	dest := []any{&dep.ID, &dep.AppID, &dep.SpecID, &dep.Trigger, &dep.Status, &dep.ResultState,
		&code, &detail, &dep.StartedAt, &dep.FinishedAt, &dep.CreatedBy,
		&dep.ApprovalsRequired, &dep.ApprovalExpiresAt, &reasons, &dep.SpecRevision, &dep.RequestedByName}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return Deployment{}, err
	}
	if code != nil {
		dep.ErrorCode = *code
	}
	if len(detail) > 0 {
		var parsed map[string]string
		if json.Unmarshal(detail, &parsed) == nil {
			dep.ErrorDetail = parsed["message"]
		}
	}
	for _, r := range reasons {
		dep.ApprovalReasons = append(dep.ApprovalReasons, ApprovalReason{Reason: r})
	}
	return dep, nil
}

// ListForApp returns an app's deployments, newest first, with the decisions
// on any that needed approval.
func (d *Deployments) ListForApp(ctx context.Context, appID string) ([]Deployment, error) {
	rows, err := d.db.Query(ctx, `
		SELECT `+deploymentColumns+`
		FROM deployments d WHERE d.app_id = $1 ORDER BY d.started_at DESC LIMIT 50`, appID)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not list the app's deploys.", err)
	}
	defer rows.Close()

	var out []Deployment
	var needDecisions []string
	for rows.Next() {
		dep, err := scanDeployment(rows)
		if err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not list the app's deploys.", err)
		}
		if dep.ApprovalsRequired > 0 {
			needDecisions = append(needDecisions, dep.ID)
		}
		out = append(out, dep)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not list the app's deploys.", err)
	}
	if len(needDecisions) > 0 {
		decisions, err := d.Decisions(ctx, needDecisions)
		if err != nil {
			return nil, err
		}
		for i := range out {
			out[i].Approvals = decisions[out[i].ID]
		}
	}
	return out, nil
}

// --- deploy approval (R-154 – R-159) ---------------------------------------

// CreateAwaiting records a deploy that needs approval before it runs: tied to
// one spec revision (R-156), needing required approvals, waiting until
// expiresAt (nil waits until somebody answers), for the reasons given.
func (d *Deployments) CreateAwaiting(ctx context.Context, appID, specID, trigger, createdBy string,
	required int, expiresAt *time.Time, reasons []string) (Deployment, error) {
	if required < 1 {
		required = 1
	}
	if reasons == nil {
		reasons = []string{}
	}
	dep := Deployment{
		ID:                id.New(id.Deployment),
		AppID:             appID,
		SpecID:            specID,
		Trigger:           trigger,
		Status:            DeployAwaitingApproval,
		CreatedBy:         createdBy,
		ApprovalsRequired: required,
		ApprovalExpiresAt: expiresAt,
	}
	for _, r := range reasons {
		dep.ApprovalReasons = append(dep.ApprovalReasons, ApprovalReason{Reason: r})
	}
	err := d.db.QueryRow(ctx, `
		INSERT INTO deployments (id, app_id, spec_id, trigger, status, created_by,
		                         approvals_required, approval_expires_at, approval_reasons)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING started_at`,
		dep.ID, appID, specID, trigger, DeployAwaitingApproval, createdBy,
		required, expiresAt, reasons).Scan(&dep.StartedAt)
	if err != nil {
		return Deployment{}, errs.Wrap(errs.Internal, "Could not record the deploy request.", err)
	}
	return dep, nil
}

// Decide records one principal's decision on a deploy. A principal decides
// once per request (the table's primary key): deciding again replaces the
// earlier answer rather than counting twice, so a retried approval is
// harmless and two approvals from one person are one.
func (d *Deployments) Decide(ctx context.Context, deploymentID, principalID, decision, comment string) error {
	_, err := d.db.Exec(ctx, `
		INSERT INTO deployment_approvals (deployment_id, principal_id, decision, comment)
		VALUES ($1, $2, $3, NULLIF($4, ''))
		ON CONFLICT (deployment_id, principal_id) DO UPDATE
		   SET decision = EXCLUDED.decision, comment = EXCLUDED.comment, decided_at = now()`,
		deploymentID, principalID, decision, comment)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record the decision on this deploy.", err)
	}
	return nil
}

// Decisions returns the decisions on each deployment, oldest first, with
// each decider's name for showing.
func (d *Deployments) Decisions(ctx context.Context, deploymentIDs []string) (map[string][]ApprovalDecision, error) {
	out := map[string][]ApprovalDecision{}
	if len(deploymentIDs) == 0 {
		return out, nil
	}
	rows, err := d.db.Query(ctx, `
		SELECT a.deployment_id, a.principal_id,
		       coalesce(nullif(u.display_name, ''), nullif(u.email, ''), u.external_id, t.name, a.principal_id),
		       a.decision, coalesce(a.comment, ''), a.decided_at
		FROM deployment_approvals a
		LEFT JOIN users u ON u.id = a.principal_id
		LEFT JOIN tokens t ON t.id = a.principal_id
		WHERE a.deployment_id = ANY($1)
		ORDER BY a.decided_at, a.principal_id`, deploymentIDs)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read the decisions on this deploy.", err)
	}
	defer rows.Close()
	for rows.Next() {
		var depID string
		var dec ApprovalDecision
		if err := rows.Scan(&depID, &dec.PrincipalID, &dec.PrincipalName, &dec.Decision, &dec.Comment, &dec.DecidedAt); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read the decisions on this deploy.", err)
		}
		out[depID] = append(out[depID], dec)
	}
	return out, rows.Err()
}

// StartApproved moves a deploy that has its approvals from awaiting_approval
// to pending — queued, the ordinary path — and reports whether it did.
//
// Conditional, so that two approvals arriving together start it once: the
// second finds it no longer waiting. And conditional on nothing else being in
// flight for the app, in the same statement, so an approval cannot start a
// deploy alongside one that began a moment earlier (the check-then-start a
// caller makes first has that gap). started_at becomes the time it started,
// so a list of deploys reads in the order they ran; when it was asked for is
// the deploy.request audit event.
func (d *Deployments) StartApproved(ctx context.Context, deploymentID string) (bool, error) {
	tag, err := d.db.Exec(ctx, `
		UPDATE deployments d SET status = $2, started_at = now(), replica_id = NULL
		WHERE d.id = $1 AND d.status = $3
		  AND NOT EXISTS (
		      SELECT 1 FROM deployments other
		      WHERE other.app_id = d.app_id AND other.status IN ($2, $4, $5))`,
		deploymentID, DeployPending, DeployAwaitingApproval, DeployBuilding, DeployApplying)
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not start the approved deploy.", err)
	}
	return tag.RowsAffected() == 1, nil
}

// EndAwaiting closes a deploy that is still waiting for approval — rejected,
// expired, superseded, or failed because its plan no longer passes — and
// reports whether it was still waiting. Conditional for the same reason as
// StartApproved: whichever of a rejection, an expiry and a final approval
// lands first decides.
func (d *Deployments) EndAwaiting(ctx context.Context, deploymentID, status, errorCode, message string) (bool, error) {
	var detail any
	if message != "" {
		if encoded, err := json.Marshal(map[string]string{"message": message}); err == nil {
			detail = encoded
		}
	}
	tag, err := d.db.Exec(ctx, `
		UPDATE deployments SET status = $2, error_code = $3, error_detail = $4, finished_at = now()
		WHERE id = $1 AND status = $5`,
		deploymentID, status, nullable(errorCode), detail, DeployAwaitingApproval)
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not update the deploy request.", err)
	}
	return tag.RowsAffected() == 1, nil
}

// SupersedeAwaiting ends every request for an app still waiting for approval,
// except the one named, and returns the ones it ended. A newer request
// supersedes an older one (R-156): approving the older would deploy a
// revision nobody is asking for any more.
func (d *Deployments) SupersedeAwaiting(ctx context.Context, appID, exceptID string) ([]string, error) {
	rows, err := d.db.Query(ctx, `
		UPDATE deployments SET status = $3, finished_at = now()
		WHERE app_id = $1 AND id <> $2 AND status = $4
		RETURNING id`, appID, exceptID, DeploySuperseded, DeployAwaitingApproval)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not supersede the app's waiting deploy requests.", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var depID string
		if err := rows.Scan(&depID); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not supersede the app's waiting deploy requests.", err)
		}
		out = append(out, depID)
	}
	return out, rows.Err()
}

// ExpireDue ends every request whose wait ran out at or before now, and
// returns them. A request with no expiry waits until somebody answers.
func (d *Deployments) ExpireDue(ctx context.Context, now time.Time) ([]Deployment, error) {
	detail, err := json.Marshal(map[string]string{
		"message": "Nobody approved this deploy before its request expired. Deploy again to ask again.",
	})
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not expire deploy requests.", err)
	}
	rows, err := d.db.Query(ctx, `
		UPDATE deployments d SET status = $1, error_code = $2, error_detail = $3, finished_at = now()
		WHERE d.status = $4 AND d.approval_expires_at IS NOT NULL AND d.approval_expires_at <= $5
		RETURNING `+deploymentColumns,
		DeployExpired, string(errs.StateInvalid), detail, DeployAwaitingApproval, now)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not expire deploy requests.", err)
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		dep, err := scanDeployment(rows)
		if err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not expire deploy requests.", err)
		}
		out = append(out, dep)
	}
	return out, rows.Err()
}

// ListAwaiting returns every deploy waiting for approval on an app that has
// not been deleted, oldest request first, with its decisions so far. Who may
// see each is the caller's to decide.
func (d *Deployments) ListAwaiting(ctx context.Context) ([]AwaitingApproval, error) {
	rows, err := d.db.Query(ctx, `
		SELECT `+deploymentColumns+`, a.name, a.slug
		FROM deployments d JOIN apps a ON a.id = d.app_id
		WHERE d.status = $1 AND a.deleted_at IS NULL
		ORDER BY d.started_at, d.id`, DeployAwaitingApproval)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not list the deploys waiting for approval.", err)
	}
	defer rows.Close()
	var out []AwaitingApproval
	var ids []string
	for rows.Next() {
		var item AwaitingApproval
		dep, err := scanDeployment(rows, &item.AppName, &item.AppSlug)
		if err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not list the deploys waiting for approval.", err)
		}
		item.Deployment = dep
		ids = append(ids, dep.ID)
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not list the deploys waiting for approval.", err)
	}
	decisions, err := d.Decisions(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Approvals = decisions[out[i].ID]
	}
	return out, nil
}

// RanSuccessfully reports whether a spec revision of an app has been deployed
// successfully before — what makes rolling back to it free of approval
// (R-157). A deploy that succeeded is the proof it ran; being pinned is not,
// since a revision can be pinned by hand without ever being deployed.
func (d *Deployments) RanSuccessfully(ctx context.Context, appID, specID string) (bool, error) {
	var exists bool
	err := d.db.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM deployments
		WHERE app_id = $1 AND spec_id = $2 AND status = $3)`, appID, specID, DeploySucceeded).Scan(&exists)
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not read the app's deploys.", err)
	}
	return exists, nil
}

// InFlight reports whether a deployment is already running for an app.
//
// Used to refuse a concurrent deploy rather than queue one: two deploys racing
// on the same bundle is how an app ends up in a state neither of them intended.
func (d *Deployments) InFlight(ctx context.Context, appID string) (bool, error) {
	var exists bool
	err := d.db.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM deployments
		WHERE app_id = $1 AND status IN ('pending', 'building', 'applying'))`, appID).Scan(&exists)
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not check for a running deploy.", err)
	}
	return exists, nil
}

// SetImageRef records the image a deployment ran, and what the runtime
// resolved it to.
//
// The reference is what the reconciler restores a missing workload with, rather
// than rebuilding: rebuilding to correct drift would turn "someone killed a
// container" into "ship whatever is on the branch now", a far larger action
// than the one being corrected (R-120).
//
// The digest is what makes "a workload exists with the wrong image" detectable.
// A reference cannot be compared against a running container — the container
// reports a digest — and a tag can point somewhere new without changing.
func (d *Deployments) SetImageRef(ctx context.Context, deploymentID, imageRef, digest string, perWorkload map[string]WorkloadImage) error {
	if imageRef == "" && digest == "" && len(perWorkload) == 0 {
		return nil
	}

	// Per workload as well, because one image is the whole story only for an
	// app built from one Dockerfile. A compose app builds per service, and a
	// reconciler restoring every workload from the app's single recorded image
	// replaces the application with a second copy of its proxy.
	var encoded []byte
	if len(perWorkload) > 0 {
		var err error
		if encoded, err = json.Marshal(perWorkload); err != nil {
			return errs.Wrap(errs.Internal, "Could not record the deployed images.", err)
		}
	}

	_, err := d.db.Exec(ctx,
		`UPDATE deployments
		    SET image_ref = NULLIF($2, ''), image_digest = NULLIF($3, ''), workload_images = $4
		  WHERE id = $1`, deploymentID, imageRef, digest, encoded)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record the deployed image.", err)
	}
	return nil
}

// SetEgressRules records the egress rules a deployment runs with, so the
// reconciler can restore them rather than re-resolving policy under a running
// app (R-183, O-10).
func (d *Deployments) SetEgressRules(ctx context.Context, deploymentID string, rules egress.Rules) error {
	encoded, err := json.Marshal(rules)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record the deploy's egress rules.", err)
	}
	if _, err := d.db.Exec(ctx, `UPDATE deployments SET egress_rules = $2 WHERE id = $1`, deploymentID, encoded); err != nil {
		return errs.Wrap(errs.Internal, "Could not record the deploy's egress rules.", err)
	}
	return nil
}

// WorkloadImage is what one part of an app ran.
type WorkloadImage struct {
	Ref    string `json:"ref,omitempty"`
	Digest string `json:"digest,omitempty"`
}

// LastImage returns the image the app's newest successful deploy shipped.
//
// What a rescan looks at (R-312): the image that is running, not one derived
// from the app's name. Empty for an app that has never deployed, or one whose
// deploys never carried an image — an app that runs somebody else's published
// image has one, and an app that has never been built does not.
func (d *Deployments) LastImage(ctx context.Context, appID string) (string, error) {
	var ref *string
	// The digest when there is no reference. A deploy from before the
	// reference was recorded correctly still names what ran — a digest is a
	// perfectly good thing to hand a scanner, and is in fact the more exact of
	// the two.
	err := d.db.QueryRow(ctx, `
		SELECT coalesce(image_ref, image_digest)
		FROM deployments
		WHERE app_id = $1 AND status = $2
		  AND (image_ref IS NOT NULL OR image_digest IS NOT NULL)
		ORDER BY started_at DESC
		LIMIT 1`, appID, DeploySucceeded).Scan(&ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", errs.Wrap(errs.Internal, "Could not read the app's deploys.", err)
	}
	if ref == nil {
		return "", nil
	}
	return *ref, nil
}
