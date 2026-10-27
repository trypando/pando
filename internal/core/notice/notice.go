// Package notice is what an app's Overview asks of somebody: a configuration
// not yet deployed, variables with no value, a backup that was not taken, a
// security score below the installation's minimum, the pinned spec's warnings
// (R-412).
//
// Notices are computed every time they are asked for, from what Pando already
// holds. None is stored: a notice stored when it was raised would still be
// there after it was fixed. What is stored is a person's choice to ignore one,
// together with the notice's fingerprint at that moment — so a notice that
// says something new comes back.
//
// Ignoring hides a reminder and nothing more. Nothing that plans, deploys or
// gates on a security score reads an ignore: a deploy a notice warned about is
// refused for the same reason whether or not it was ignored.
package notice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/trypando/pando/internal/core/security"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// The notice keys. A spec warning's key is WarningPrefix followed by its code.
const (
	KeyUnshipped     = "config.unshipped"
	KeyUnsetVars     = "variables.unset"
	KeyBackup        = "backup.not_taken"
	KeySecurity      = "security.below_threshold"
	KeyNoStandby     = "standby.no_room"
	KeyStandby       = "standby.live_copy"
	WarningPrefix    = "warning."
	maxKeyLength     = 200
	shownVariables   = 4
	fingerprintBytes = 16
)

// Notice is one thing an app's Overview asks of somebody.
type Notice struct {
	// Key names the kind of notice and its subject, and is stable: the same
	// notice has the same key every time it is listed.
	Key string `json:"key"`

	// Message says what is wrong and what to do about it (R-105).
	Message string `json:"message"`

	// Fingerprint changes exactly when what the notice says changes. An
	// ignore stored against an older fingerprint does not hide it.
	Fingerprint string `json:"fingerprint"`

	// Ignored is whether the person asking has ignored this notice as it
	// reads now. Always false for a caller who is not a person.
	Ignored bool `json:"ignored"`
}

// Revisions is the app's spec revisions.
type Revisions interface {
	ListRevisions(ctx context.Context, appID string) ([]state.Revision, error)
	RevisionByID(ctx context.Context, specID string) (state.Revision, bool, error)
}

// Backups is the app's scheduled backup attempts.
type Backups interface {
	Attempts(ctx context.Context, appID string, limit int) ([]state.BackupAttempt, error)
}

// Security is where the app stands against the installation's minimum score.
type Security interface {
	Report(ctx context.Context, appID, specID string) (security.Report, error)
}

// StandbyRooms is where each app's warm standby stands (R-419, R-422).
type StandbyRooms interface {
	Standby(ctx context.Context, appID string) (StandbyStanding, error)
}

// StandbyStanding is an app's standby as its runtime last placed it.
type StandbyStanding struct {
	// Unplaced is the runtime's reason when the app should have a standby
	// and no host had room for one, naming the largest room any host has.
	// Remedy is what to do. Both empty when the app has its standby or wants
	// none.
	Unplaced string
	Remedy   string
}

// Standbys is each app's warm standby (R-419).
type Standbys interface {
	Get(ctx context.Context, appID string) (state.Standby, bool, error)
}

// Ignores is each person's ignored notices.
type Ignores interface {
	For(ctx context.Context, userID, appID string) (map[string]string, error)
	Ignore(ctx context.Context, userID, appID, key, fingerprint string) error
	Restore(ctx context.Context, userID, appID, key string) error
}

// Service lists an app's notices and keeps each person's ignores.
//
// Backups, Security and Standbys may be nil, on an installation without
// them: their notices are then never raised.
type Service struct {
	Revisions Revisions
	Backups   Backups
	Security  Security
	Standbys  Standbys
	Ignores   Ignores

	// Rooms is where each standby was placed, or why none was (R-422). Nil
	// on an installation that keeps none.
	Rooms StandbyRooms
}

// Inputs is what notices are computed from.
type Inputs struct {
	// Newest is the app's newest revision and Pinned the one it runs, with
	// its body. Either may be nil.
	Newest *state.Revision
	Pinned *state.Revision

	// LastBackup is the app's last scheduled backup attempt, if any.
	LastBackup *state.BackupAttempt

	// Standing is the app's security standing, if the installation has one.
	Standing *security.Standing

	// Room is where the app's standby stands, on an installation that keeps them.
	Room *StandbyStanding
	// Standby is the app's warm standby, if it has one.
	Standby *state.Standby
}

// List returns every current notice on an app, each marked with whether the
// person asking has ignored it. userID is empty for a caller who is not a
// person, who sees every notice unignored.
func (s *Service) List(ctx context.Context, userID string, app state.App) ([]Notice, error) {
	in, err := s.inputs(ctx, app)
	if err != nil {
		return nil, err
	}
	notices := Compute(in)
	if userID == "" || len(notices) == 0 {
		return notices, nil
	}
	ignored, err := s.Ignores.For(ctx, userID, app.ID)
	if err != nil {
		return nil, err
	}
	for i := range notices {
		fp, ok := ignored[notices[i].Key]
		notices[i].Ignored = ok && fp == notices[i].Fingerprint
	}
	return notices, nil
}

// countWorkers bounds how many apps' notices Counts computes at once: a page
// of the list is up to a few hundred apps, each a handful of queries.
const countWorkers = 8

// Counts is, for each app, how many of its notices the person asking has not
// ignored — the number the apps list shows beside each app's status (R-412).
// Each is List's own answer, so the list and the app's Overview cannot
// disagree. userID is empty for a caller who is not a person, who has ignored
// nothing.
func (s *Service) Counts(ctx context.Context, userID string, apps []state.App) (map[string]int, error) {
	counts := make([]int, len(apps))
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(countWorkers)
	for i := range apps {
		g.Go(func() error {
			notices, err := s.List(ctx, userID, apps[i])
			if err != nil {
				return err
			}
			for _, n := range notices {
				if !n.Ignored {
					counts[i]++
				}
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	out := make(map[string]int, len(apps))
	for i, a := range apps {
		out[a.ID] = counts[i]
	}
	return out, nil
}

// Ignore hides a notice from one person until what it says changes. It is
// refused when the app has no such notice now, because there would be nothing
// to record the fingerprint of.
func (s *Service) Ignore(ctx context.Context, userID string, app state.App, key string) (Notice, error) {
	if err := needPerson(userID); err != nil {
		return Notice{}, err
	}
	notices, err := s.List(ctx, userID, app)
	if err != nil {
		return Notice{}, err
	}
	for _, n := range notices {
		if n.Key != key {
			continue
		}
		if err := s.Ignores.Ignore(ctx, userID, app.ID, n.Key, n.Fingerprint); err != nil {
			return Notice{}, err
		}
		n.Ignored = true
		return n, nil
	}
	return Notice{}, errs.Newf(errs.NotFound, "This app has no notice %q right now, so there is nothing to ignore.", truncate(key)).
		WithRemedy("List the app's notices to see the keys of the ones it has now.")
}

// Restore shows a notice to one person again. Restoring a notice that is not
// ignored, or that no longer exists, is not an error.
func (s *Service) Restore(ctx context.Context, userID string, app state.App, key string) error {
	if err := needPerson(userID); err != nil {
		return err
	}
	if key == "" || len(key) > maxKeyLength {
		return errs.Newf(errs.ValidInvalid, "A notice key is between 1 and %d characters.", maxKeyLength)
	}
	return s.Ignores.Restore(ctx, userID, app.ID, key)
}

func needPerson(userID string) error {
	if userID != "" {
		return nil
	}
	return errs.New(errs.AuthRequired, "Ignoring a notice is a person's own choice, and this request is not signed in as a person.").
		WithRemedy("Sign in, or use a token you minted for yourself rather than a service token.")
}

func truncate(key string) string {
	if len(key) > 64 {
		return key[:64] + "…"
	}
	return key
}

// inputs reads what Compute needs for one app.
func (s *Service) inputs(ctx context.Context, app state.App) (Inputs, error) {
	var in Inputs
	revs, err := s.Revisions.ListRevisions(ctx, app.ID)
	if err != nil {
		return in, err
	}
	if len(revs) > 0 {
		in.Newest = &revs[0] // newest first
	}
	if app.PinnedSpecID != "" {
		pinned, found, err := s.Revisions.RevisionByID(ctx, app.PinnedSpecID)
		if err != nil {
			return in, err
		}
		if found {
			in.Pinned = &pinned
		}
	}
	if s.Backups != nil {
		attempts, err := s.Backups.Attempts(ctx, app.ID, 1)
		if err != nil {
			return in, err
		}
		if len(attempts) > 0 {
			in.LastBackup = &attempts[0]
		}
	}
	if s.Security != nil {
		report, err := s.Security.Report(ctx, app.ID, app.PinnedSpecID)
		if err != nil {
			return in, err
		}
		in.Standing = &report.Standing
	}
	if s.Standbys != nil {
		sb, found, err := s.Standbys.Get(ctx, app.ID)
		if err != nil {
			return in, err
		}
		if found {
			in.Standby = &sb
		}
	}
	if s.Rooms != nil {
		room, err := s.Rooms.Standby(ctx, app.ID)
		if err != nil {
			return in, err
		}
		in.Room = &room
	}
	return in, nil
}

// Compute is an app's notices, in the order the Overview lists them.
func Compute(in Inputs) []Notice {
	out := []Notice{}
	if n, ok := unshipped(in.Newest, in.Pinned); ok {
		out = append(out, n)
	}
	if n, ok := insecure(in.Standing); ok {
		out = append(out, n)
	}
	if n, ok := backupNotTaken(in.LastBackup); ok {
		out = append(out, n)
	}
	if n, ok := noStandby(in.Room); ok {
		out = append(out, n)
	}
	var body *spec.AppSpec
	if in.Pinned != nil {
		body = in.Pinned.Body
	}
	if n, ok := unsetVariables(body); ok {
		out = append(out, n)
	}
	if n, ok := liveCopy(in.Standby); ok {
		out = append(out, n)
	}
	return append(out, warnings(body)...)
}

// liveCopy: the app's standby holds copies of its volumes taken while it ran,
// so a copy may be inconsistent (R-420). A notice, never a blocker: nothing
// that plans or deploys reads it. Fingerprinted on where the standby is and
// what travels in the copy, not on when it was taken, so it does not come
// back after every copy.
func liveCopy(sb *state.Standby) (Notice, bool) {
	if sb == nil || sb.Host == "" || sb.Volumes == 0 {
		return Notice{}, false
	}
	msg := fmt.Sprintf("This app's standby on %s holds copies of its storage taken while the app runs, so a file written during a copy may be inconsistent there. "+
		"If the standby is started, it starts from the newest copy, or the one before if the app does not come up healthy.", sb.Host)
	if len(sb.Unreplicated) > 0 {
		msg += fmt.Sprintf(" Its %s data is copied the same way, because its services adapter does not replicate it.", strings.Join(sb.Unreplicated, " and "))
	}
	return newNotice(KeyStandby, msg, sb.Host, strings.Join(sb.Unreplicated, ",")), true
}

// unshipped: a revision newer than the one the app runs. Fingerprinted on the
// newest revision's number, so a further edit after ignoring brings it back.
func unshipped(newest, pinned *state.Revision) (Notice, bool) {
	if newest == nil || pinned == nil || newest.Revision <= pinned.Revision {
		return Notice{}, false
	}
	return newNotice(KeyUnshipped,
		"The configuration has changed since this app was last deployed. Deploy to apply it.",
		fmt.Sprint(newest.Revision)), true
}

// insecure: a score below the installation's minimum (R-314). Only the
// reminder; the deploy gate reads the standing itself.
func insecure(standing *security.Standing) (Notice, bool) {
	if standing == nil || standing.Verdict != security.VerdictInsecure || standing.Score == nil {
		return Notice{}, false
	}
	score := *standing.Score
	msg := fmt.Sprintf("This app's security score is %d, below this installation's requirement of %d. Fix the findings its security scan reports and deploy again.",
		score, standing.Threshold)
	stopAt := ""
	if !standing.StopAt.IsZero() {
		stopAt = standing.StopAt.UTC().Format(time.RFC3339)
		msg += fmt.Sprintf(" Pando will stop it at %s unless its score reaches %d.", stopAt, standing.Threshold)
	}
	return newNotice(KeySecurity, msg, fmt.Sprint(score), fmt.Sprint(standing.Threshold), stopAt), true
}

// backupNotTaken: the last scheduled backup was skipped or failed (R-211). The
// message is the attempt's own, which already says why and what to do.
func backupNotTaken(attempt *state.BackupAttempt) (Notice, bool) {
	if attempt == nil || attempt.Outcome == state.AttemptTaken {
		return Notice{}, false
	}
	parts := []string{}
	for _, p := range []string{attempt.Message, attempt.Remedy} {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	msg := strings.Join(parts, " ")
	if msg == "" {
		msg = fmt.Sprintf("This app's last scheduled backup, at %s, was %s.",
			attempt.AttemptedAt.UTC().Format(time.RFC3339), attempt.Outcome)
	}
	return newNotice(KeyBackup, msg,
		attempt.AttemptedAt.UTC().Format(time.RFC3339Nano), attempt.Outcome, attempt.Message, attempt.Remedy), true
}

// noStandby: the app runs without the standby it should have, because no
// host had room for one (R-422). A notice and never a blocker: the app
// deployed. The message is the runtime's, which names the largest room any
// host has; it changes as the room does, and the notice comes back with it.
func noStandby(standing *StandbyStanding) (Notice, bool) {
	if standing == nil || standing.Unplaced == "" {
		return Notice{}, false
	}
	msg := strings.TrimSpace(standing.Unplaced + " " + standing.Remedy)
	return newNotice(KeyNoStandby, msg, standing.Unplaced, standing.Remedy), true
}

// unsetVariables: variables the pinned spec declares with no value, no secret
// and no slot. Counted from the spec rather than carried in it, so it is gone
// the moment they are filled. Fingerprinted on the sorted names: one more
// empty variable brings it back.
func unsetVariables(body *spec.AppSpec) (Notice, bool) {
	if body == nil {
		return Notice{}, false
	}
	var keys []string
	seen := map[string]bool{}
	for _, w := range body.Workloads {
		for _, e := range w.Env {
			if e.SecretRef != nil || e.SlotRef != nil || (e.Value != nil && *e.Value != "") || seen[e.Key] {
				continue
			}
			seen[e.Key] = true
			keys = append(keys, e.Key)
		}
	}
	if len(keys) == 0 {
		return Notice{}, false
	}
	sorted := slices.Clone(keys)
	slices.Sort(sorted)
	return newNotice(KeyUnsetVars, unsetMessage(keys), sorted...), true
}

// unsetMessage names the variables first, because the person reading knows
// their own app; four of them, then a count.
func unsetMessage(keys []string) string {
	if len(keys) == 1 {
		return keys[0] + " is declared with no value."
	}
	shown := keys[:min(len(keys), shownVariables)]
	rest := ""
	if len(keys) > shownVariables {
		rest = fmt.Sprintf(", and %d more", len(keys)-shownVariables)
	}
	return fmt.Sprintf("%d variables are declared with no value: %s%s.", len(keys), strings.Join(shown, ", "), rest)
}

// warnings: the pinned spec's warnings not yet dismissed, one notice per code.
func warnings(body *spec.AppSpec) []Notice {
	if body == nil {
		return nil
	}
	var codes []string
	messages := map[string][]string{}
	for _, w := range body.Warnings {
		if w.Dismissed {
			continue
		}
		if _, ok := messages[w.Code]; !ok {
			codes = append(codes, w.Code)
		}
		messages[w.Code] = append(messages[w.Code], w.Message)
	}
	out := make([]Notice, 0, len(codes))
	for _, code := range codes {
		out = append(out, newNotice(WarningPrefix+code, strings.Join(messages[code], " "), messages[code]...))
	}
	return out
}

func newNotice(key, message string, facts ...string) Notice {
	return Notice{Key: key, Message: message, Fingerprint: Fingerprint(key, facts...)}
}

// Fingerprint is a short, stable digest of what a notice says.
func Fingerprint(key string, facts ...string) string {
	h := sha256.New()
	h.Write([]byte(key))
	for _, f := range facts {
		// A separator that cannot occur in a fact keeps ["a","bc"] and
		// ["ab","c"] apart.
		h.Write([]byte{0})
		h.Write([]byte(f))
	}
	return hex.EncodeToString(h.Sum(nil)[:fingerprintBytes])
}
