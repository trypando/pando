// Package fakeapi is an API package in httpapi's shape, for the generator's
// tests: one handler for each way a handler answers.
package fakeapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/trypando/pando/internal/errs"
)

type Server struct{}

type Thing struct {
	ID     string    `json:"id"`
	Name   string    `json:"name,omitempty"`
	Tags   []string  `json:"tags"`
	At     time.Time `json:"at"`
	Parent *Thing    `json:"parent"`
	Hidden string    `json:"-"`
	secret string
	Embedded
}

type Embedded struct {
	Extra int `json:"extra"`
}

func JSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func Error(w http.ResponseWriter, r *http.Request, err error) {
	JSON(w, errs.As(err).Status(), errs.As(err))
}

func withTotal(body map[string]any, total int) map[string]any {
	body["total"] = total
	return body
}

func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/things", s.handleListThings)
		r.Post("/things", s.handleCreateThing)
		r.Route("/things/{thingID}", func(r chi.Router) {
			r.Get("/", s.handleGetThing)
			r.Delete("/", s.handleDeleteThing)
		})
		r.Get("/loose", s.handleLoose)
		r.Get("/go", s.handleRedirect)
		r.Get("/text", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
		})
	})
	return r
}

func (s *Server) handleListThings(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if _, err := strconv.Atoi(q.Get("limit")); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "No."))
		return
	}
	all := q.Get("all") == "true"
	out := []map[string]any{}
	for range 2 {
		out = append(out, map[string]any{"id": "x", "all": all})
	}
	JSON(w, http.StatusOK, withTotal(map[string]any{"things": out, "ids": q["id"]}, len(out)))
}

func (s *Server) handleCreateThing(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, r, err)
		return
	}
	status := http.StatusOK
	if req.Name != "" {
		status = http.StatusCreated
	}
	JSON(w, status, Thing{Name: req.Name})
}

func (s *Server) handleGetThing(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "thingID") == "" {
		JSON(w, http.StatusOK, map[string]any{"ok": false})
		return
	}
	s.answer(w, Thing{})
}

func (s *Server) answer(w http.ResponseWriter, t Thing) {
	JSON(w, http.StatusOK, map[string]any{"ok": true, "thing": t})
}

func (s *Server) handleDeleteThing(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLoose(w http.ResponseWriter, _ *http.Request) {
	var v any
	_ = json.Unmarshal([]byte("1"), &v)
	JSON(w, http.StatusOK, v)
}

func (s *Server) handleRedirect(w http.ResponseWriter, r *http.Request) {
	_ = r.Header.Get("X-Thing")
	http.Redirect(w, r, "/", http.StatusFound)
}
