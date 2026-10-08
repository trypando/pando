package autodeploy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/reconciler"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// Delivery is one webhook request from a git host, as it arrived.
type Delivery struct {
	// Event is the X-GitHub-Event header: push, release, create or ping.
	Event string

	// Signature is the X-Hub-Signature-256 header: "sha256=" and the hex
	// HMAC-SHA256 of Body, keyed by the app's webhook secret.
	Signature string

	Body []byte
}

// Outcome is what Pando did with a webhook.
type Outcome struct {
	// Checking is whether the delivery started a check. It never deploys
	// directly; the check decides.
	Checking bool   `json:"checking"`
	Message  string `json:"message"`
}

// checkTimeout bounds a check a webhook started. It runs after the response,
// so the git host is not kept waiting on a `git ls-remote` against itself.
const checkTimeout = 2 * time.Minute

// Deliver verifies a webhook and, when it concerns what the app watches,
// starts a check (R-142). The check runs in the background: a git host times
// a webhook out after a few seconds, and a missed one costs nothing, since
// the poll still runs.
//
// Every refusal is the same refusal, whatever the reason — no such app, no
// secret, a wrong signature — so the endpoint does not tell a stranger which
// app IDs exist or which have a webhook.
func (s *Service) Deliver(ctx context.Context, appID string, d Delivery) (Outcome, error) {
	if !s.verify(ctx, appID, d) {
		return Outcome{}, errs.New(errs.AuthInvalid,
			"Pando could not verify this webhook. The signature does not match this app's webhook secret.").
			WithRemedy("Copy the webhook URL and secret from the app's deploy settings into the git host's webhook, with content type application/json. Making a new secret replaces the old one.")
	}

	if d.Event == "ping" {
		return Outcome{Message: "Pando received the test delivery. Pushes and releases to this repository will now start a check."}, nil
	}

	app, found, err := s.Apps.ByID(ctx, appID)
	if err != nil {
		return Outcome{}, err
	}
	if !found || app.PinnedSpecID == "" {
		return Outcome{Message: "This app isn't deployed yet, so there is nothing for this delivery to check."}, nil
	}
	pinned, found, err := s.Apps.RevisionByID(ctx, app.PinnedSpecID)
	if err != nil || !found || pinned.Body == nil {
		return Outcome{}, err
	}
	ad := pinned.Body.Deploy.AutoDeploy
	if !ad.Enabled {
		return Outcome{Message: "This app does not deploy automatically, so Pando ignored this delivery."}, nil
	}
	if why := concerns(d, pinned.Body.Source, ad); why != "" {
		return Outcome{Message: why}, nil
	}

	// Detached from the request, which ends when this returns. The ID is the
	// one the store gave back, never the request's path: what reaches the log
	// is an app that exists, not whatever a caller typed.
	known := app.ID
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), checkTimeout)
		defer cancel()
		if err := s.Checker.CheckApp(ctx, known, reconciler.DeliveryWebhook); err != nil && s.Logger != nil {
			s.Logger.Warn("could not check for new commits after a webhook",
				zap.String("app_id", known), zap.Error(err))
		}
	}()
	return Outcome{Checking: true, Message: "Pando is checking this app for something new to deploy."}, nil
}

func (s *Service) verify(ctx context.Context, appID string, d Delivery) bool {
	hexSig, ok := strings.CutPrefix(d.Signature, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(hexSig)
	if err != nil {
		return false
	}
	key, found, err := s.Secrets.Get(ctx, appID)
	if err != nil || !found {
		return false
	}
	mac := hmac.New(sha256.New, []byte(key.Reveal()))
	mac.Write(d.Body)
	return hmac.Equal(got, mac.Sum(nil))
}

// concerns says why a delivery has nothing to do with what the app watches,
// or "" when it does. Only which ref moved is read from the payload, to skip
// a check that could not find anything; what to deploy is never taken from it.
func concerns(d Delivery, src spec.Source, ad spec.AutoDeploy) string {
	var payload struct {
		Ref     string `json:"ref"`
		RefType string `json:"ref_type"`
	}
	_ = json.Unmarshal(d.Body, &payload)

	if reconciler.TriggerOf(ad) == spec.TriggerReleaseTagged {
		switch {
		case d.Event == "release",
			d.Event == "push" && strings.HasPrefix(payload.Ref, "refs/tags/"),
			d.Event == "create" && payload.RefType == "tag":
			return ""
		}
		return "This app deploys new releases, and this delivery is not about a tag or a release, so Pando ignored it."
	}

	branch := ad.Branch
	if branch == "" {
		branch = src.Ref
	}
	if !strings.HasPrefix(branch, "refs/") {
		branch = "refs/heads/" + branch
	}
	if d.Event == "push" && payload.Ref == branch {
		return ""
	}
	return "This app follows " + strings.TrimPrefix(branch, "refs/heads/") +
		", and this delivery is not a push to it, so Pando ignored it."
}
