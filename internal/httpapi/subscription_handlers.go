package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/events"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/subscription"
	"github.com/trypando/pando/internal/errs"
)

// Event subscriptions (issue #50, R-364 – R-374). Every decision is the
// service's; these decode, call, and encode (R-261).

// signedIn returns the caller, or answers 401 for an anonymous one.
func (s *Server) signedIn(w http.ResponseWriter, r *http.Request) (authz.Principal, bool) {
	p := PrincipalFrom(r.Context())
	if p.Kind == authz.KindAnonymous {
		Error(w, r, errs.New(errs.AuthRequired, "You need to sign in."))
		return p, false
	}
	if s.Subscriptions == nil {
		Error(w, r, errs.New(errs.StateInvalid, "Event subscriptions are not available on this server."))
		return p, false
	}
	return p, true
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read. Send a JSON object."))
		return false
	}
	return true
}

// handleListEvents serves the event catalog (R-364): every event a
// subscription can name, with its fields.
func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	if p := PrincipalFrom(r.Context()); p.Kind == authz.KindAnonymous {
		Error(w, r, errs.New(errs.AuthRequired, "You need to sign in."))
		return
	}
	destinations := []subscription.Destination{}
	if s.Subscriptions != nil {
		destinations = s.Subscriptions.Destinations()
	}
	JSON(w, http.StatusOK, map[string]any{"events": events.Catalog(), "destinations": destinations})
}

func (s *Server) handleListSubscriptions(w http.ResponseWriter, r *http.Request) {
	p, ok := s.signedIn(w, r)
	if !ok {
		return
	}
	subs, err := s.Subscriptions.List(r.Context(), p, subscription.ListRequest{
		AppID:    r.URL.Query().Get("app_id"),
		Everyone: r.URL.Query().Get("everyone") == "true",
	})
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, map[string]any{"subscriptions": subs})
}

func (s *Server) handleCreateSubscription(w http.ResponseWriter, r *http.Request) {
	p, ok := s.signedIn(w, r)
	if !ok {
		return
	}
	var req subscription.CreateRequest
	if !decode(w, r, &req) {
		return
	}
	created, err := s.Subscriptions.Create(r.Context(), p, req)
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusCreated, created)
}

func (s *Server) handleGetSubscription(w http.ResponseWriter, r *http.Request) {
	p, ok := s.signedIn(w, r)
	if !ok {
		return
	}
	sub, err := s.Subscriptions.Get(r.Context(), p, chi.URLParam(r, "subscriptionID"))
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, sub)
}

func (s *Server) handlePatchSubscription(w http.ResponseWriter, r *http.Request) {
	p, ok := s.signedIn(w, r)
	if !ok {
		return
	}
	var req subscription.UpdateRequest
	if !decode(w, r, &req) {
		return
	}
	sub, err := s.Subscriptions.Update(r.Context(), p, chi.URLParam(r, "subscriptionID"), req)
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, sub)
}

func (s *Server) handleDeleteSubscription(w http.ResponseWriter, r *http.Request) {
	p, ok := s.signedIn(w, r)
	if !ok {
		return
	}
	if err := s.Subscriptions.Delete(r.Context(), p, chi.URLParam(r, "subscriptionID")); err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusNoContent, nil)
}

func (s *Server) handleRotateSigningKey(w http.ResponseWriter, r *http.Request) {
	p, ok := s.signedIn(w, r)
	if !ok {
		return
	}
	out, err := s.Subscriptions.RotateKey(r.Context(), p, chi.URLParam(r, "subscriptionID"))
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, out)
}

func (s *Server) handleTestSubscription(w http.ResponseWriter, r *http.Request) {
	p, ok := s.signedIn(w, r)
	if !ok {
		return
	}
	d, err := s.Subscriptions.Test(r.Context(), p, chi.URLParam(r, "subscriptionID"))
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusAccepted, d)
}

func (s *Server) handleListDeliveries(w http.ResponseWriter, r *http.Request) {
	p, ok := s.signedIn(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := s.Subscriptions.ListDeliveries(r.Context(), p, chi.URLParam(r, "subscriptionID"),
		r.URL.Query().Get("before"), limit)
	if err != nil {
		Error(w, r, err)
		return
	}
	next := ""
	if len(list) > 0 && (limit <= 0 || len(list) == limit) {
		next = list[len(list)-1].ID
	}
	JSON(w, http.StatusOK, map[string]any{"deliveries": list, "next_before": next})
}

func (s *Server) handleGetDelivery(w http.ResponseWriter, r *http.Request) {
	p, ok := s.signedIn(w, r)
	if !ok {
		return
	}
	d, err := s.Subscriptions.Delivery(r.Context(), p, chi.URLParam(r, "subscriptionID"), chi.URLParam(r, "deliveryID"))
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, d)
}

func (s *Server) handleRedeliver(w http.ResponseWriter, r *http.Request) {
	p, ok := s.signedIn(w, r)
	if !ok {
		return
	}
	d, err := s.Subscriptions.Redeliver(r.Context(), p, chi.URLParam(r, "subscriptionID"), chi.URLParam(r, "deliveryID"))
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusAccepted, d)
}

func (s *Server) handleGetNotificationPreferences(w http.ResponseWriter, r *http.Request) {
	p, ok := s.signedIn(w, r)
	if !ok {
		return
	}
	view, err := s.Subscriptions.Preferences(r.Context(), p)
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, view)
}

func (s *Server) handlePutNotificationPreferences(w http.ResponseWriter, r *http.Request) {
	p, ok := s.signedIn(w, r)
	if !ok {
		return
	}
	var req struct {
		Choices []state.NotificationPreference `json:"choices"`
	}
	if !decode(w, r, &req) {
		return
	}
	view, err := s.Subscriptions.SetPreferences(r.Context(), p, req.Choices)
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, view)
}
