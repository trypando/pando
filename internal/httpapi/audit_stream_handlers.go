package httpapi

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/auditstream"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/log"
)

// AuditStream reads the audit log in commit order (R-381, R-387).
type AuditStream interface {
	ResolveCursor(ctx context.Context, s string) (audit.Cursor, error)
	Wait(ctx context.Context, q audit.StreamQuery, wait time.Duration, c clock.Clock) (audit.StreamPage, error)
	Export(ctx context.Context, q audit.StreamQuery, enc audit.Encoder, write func([]byte) error) (int, error)
	Oldest(ctx context.Context) (time.Time, bool, error)
}

// AuditSinks reports where each audit sink has got to (R-383).
type AuditSinks interface {
	Statuses(ctx context.Context) ([]auditstream.Status, error)
}

// auditRead records that somebody read the audit log (R-388).
func (s *Server) auditRead(r *http.Request, p authz.Principal, action string, detail map[string]any) {
	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: action, TargetKind: "audit_log", TargetID: "install", Detail: detail,
	})
}

// encoderFor is the encoder for a requested format: nil for native.
func (s *Server) encoderFor(format string) (audit.Encoder, error) {
	switch format {
	case "", api.AuditFormatNative:
		return nil, nil
	case api.AuditFormatOCSF:
		if enc := s.AuditEncoders[api.AuditFormatOCSF]; enc != nil {
			return enc, nil
		}
	}
	return nil, errs.Newf(errs.ValidInvalid, "%q is not an audit event format. Valid answers: native, ocsf.", format)
}

// handleAuditStream serves one page of the audit log in commit order, from a
// cursor (R-381, design 12 §4).
func (s *Server) handleAuditStream(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallAuditRead)
	if !ok {
		return
	}
	if s.AuditStream == nil {
		Error(w, r, errs.New(errs.Internal, "The audit stream is not readable on this installation."))
		return
	}
	qs := r.URL.Query()
	after, err := s.AuditStream.ResolveCursor(r.Context(), qs.Get("after"))
	if err != nil {
		Error(w, r, err)
		return
	}
	enc, err := s.encoderFor(qs.Get("format"))
	if err != nil {
		Error(w, r, err)
		return
	}
	q := audit.StreamQuery{After: after, Actions: qs["action"], Exclude: qs["exclude"]}
	if v := qs.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			Error(w, r, errs.New(errs.ValidInvalid, "The limit must be a whole number of events, at most 1000."))
			return
		}
		q.Limit = n
	}
	var wait time.Duration
	if v := qs.Get("wait"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > int(audit.MaxStreamWait/time.Second) {
			Error(w, r, errs.New(errs.ValidInvalid, "wait is how many seconds to wait for an event when there is none yet: a whole number from 0 to 60."))
			return
		}
		wait = time.Duration(n) * time.Second
	}

	if s.AuditReads.Due(p.ID, time.Now()) {
		s.auditRead(r, p, "audit.read", map[string]any{"via": "stream"})
	}

	page, err := s.AuditStream.Wait(r.Context(), q, wait, clock.System{})
	if err != nil {
		Error(w, r, err)
		return
	}
	events := make([]json.RawMessage, 0, len(page.Events))
	for _, line := range page.Events {
		out := line
		if enc != nil {
			if out, err = enc(line); err != nil {
				Error(w, r, err)
				return
			}
		}
		events = append(events, out)
	}
	JSON(w, http.StatusOK, map[string]any{
		"events":    events,
		"cursor":    page.Cursor.String(),
		"caught_up": page.CaughtUp,
	})
}

// handleAuditExport writes a range of the live log as gzipped JSON lines, in
// commit order (R-387, design 12 §7).
func (s *Server) handleAuditExport(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallAuditRead)
	if !ok {
		return
	}
	if s.AuditStream == nil {
		Error(w, r, errs.New(errs.Internal, "The audit log is not readable on this installation."))
		return
	}
	qs := r.URL.Query()
	format := qs.Get("format")
	enc, err := s.encoderFor(format)
	if err != nil {
		Error(w, r, err)
		return
	}
	q := audit.StreamQuery{Actions: qs["action"], Exclude: qs["exclude"]}
	for _, bound := range []struct {
		name string
		into *time.Time
	}{{"since", &q.Since}, {"until", &q.Until}} {
		v := qs.Get(bound.name)
		if v == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			Error(w, r, errs.Newf(errs.ValidInvalid, "%s must be a time in RFC 3339 form, such as 2026-09-21T09:00:00Z.", bound.name))
			return
		}
		*bound.into = t
	}

	// Recorded before the export, like every privileged read (R-228, R-388).
	s.auditRead(r, p, "audit.export", map[string]any{
		"since": qs.Get("since"), "until": qs.Get("until"), "format": orNative(format), "actions": qs["action"],
	})

	h := w.Header()
	if oldest, ok, err := s.AuditStream.Oldest(r.Context()); err == nil && ok && (q.Since.IsZero() || q.Since.Before(oldest)) {
		// Archives hold what is before this (R-387).
		h.Set("Pando-Audit-Live-From", oldest.Format(time.RFC3339Nano))
	}
	name := "audit"
	if !q.Since.IsZero() {
		name += "-" + q.Since.UTC().Format("20060102T150405Z")
	}
	if !q.Until.IsZero() {
		name += "-" + q.Until.UTC().Format("20060102T150405Z")
	}
	h.Set("Content-Type", "application/gzip")
	h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name+".jsonl.gz"))
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	gz := gzip.NewWriter(w)
	n, err := s.AuditStream.Export(r.Context(), q, enc, func(line []byte) error {
		if _, err := gz.Write(line); err != nil {
			return err
		}
		_, err := gz.Write([]byte{'\n'})
		return err
	})
	if err != nil {
		// The status is already sent. A truncated gzip fails to decompress,
		// which is the client's signal; the log says why.
		log.From(r.Context()).Warn("audit export ended early", zap.Int("events", n), zap.Error(err))
		return
	}
	_ = gz.Close()
}

func orNative(format string) string {
	if format == "" {
		return api.AuditFormatNative
	}
	return format
}

// handleListAuditSinks lists every audit sink with its delivery state and
// what it sends where (R-383, R-385). install.audit.read: someone reading the
// log should be able to see that a copy of it leaves the installation.
func (s *Server) handleListAuditSinks(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireInstall(w, r, authz.InstallAuditRead); !ok {
		return
	}
	type view struct {
		auditstream.Status
		Disclosure string `json:"disclosure"`
	}
	out := []view{}
	if s.AuditSinks != nil {
		list, err := s.AuditSinks.Statuses(r.Context())
		if err != nil {
			Error(w, r, err)
			return
		}
		for _, st := range list {
			out = append(out, view{Status: st, Disclosure: auditstream.Describe(st)})
		}
	}
	JSON(w, http.StatusOK, map[string]any{"audit_sinks": out})
}
