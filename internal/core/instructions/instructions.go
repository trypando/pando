// Package instructions is what every agent building on this installation is
// told (R-424): Pando's built-in guidance for writing apps that run well and
// safely here, then the company's own, which administrators write.
//
// The two are kept apart so each has one owner. The built-in part ships in
// the binary and changes with Pando's releases; the company part is stored
// and changes only when an administrator saves it. An upgrade that improves
// the guidance never overwrites what an administrator wrote, and an
// administrator never has to merge a release's changes into their own text.
//
// Agents receive the two as one document: in MCP's initialize answer, which
// clients load as instructions, and from GET /instructions for a client that
// ignores that field.
package instructions

import (
	"context"
	_ "embed"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

//go:embed builtin.md
var builtin string

// Builtin is Pando's own guidance, the same on every installation of a
// release.
func Builtin() string { return strings.TrimSpace(builtin) }

// MaxCompany is the longest company section, in bytes [P: 32 KiB]. An MCP
// client puts the whole document in the agent's context on every
// conversation, so a long one costs every agent on the installation, every
// time; this is several pages, which is more than an agent follows well.
const MaxCompany = 32 << 10

// companyHeading introduces the company's section in the combined document,
// and says which part wins where the two disagree: the administrators know
// their organization, and the built-in part is a default.
const companyHeading = "# This organization's instructions\n\n" +
	"These come from the administrators of this Pando installation. Where they differ from the " +
	"guidance above, follow these."

// Store keeps the company section.
type Store interface {
	Load(ctx context.Context) (state.StoredInstructions, error)
	Save(ctx context.Context, body, updatedBy string) (state.StoredInstructions, error)
}

// Document is the instructions as the API serves them.
type Document struct {
	// Text is what an agent receives: Builtin, then Company under its own
	// heading when there is any.
	Text string `json:"text"`
	// Builtin is Pando's own part.
	Builtin string `json:"builtin"`
	// Company is the administrators' part, empty when there is none.
	Company string `json:"company"`
	// UpdatedAt and UpdatedBy say who last saved Company, and when. Null
	// and empty when nobody ever has.
	UpdatedAt *time.Time `json:"updated_at"`
	UpdatedBy string     `json:"updated_by"`
}

// Service reads and writes the instructions.
type Service struct {
	Store Store
}

// Get returns the instructions.
func (s *Service) Get(ctx context.Context) (Document, error) {
	stored, err := s.Store.Load(ctx)
	if err != nil {
		return Document{}, err
	}
	return document(stored), nil
}

// Text returns what an agent receives, or Builtin alone when the company's
// part cannot be read: an agent connecting while the database is unreachable
// still gets Pando's guidance.
func (s *Service) Text(ctx context.Context) string {
	d, err := s.Get(ctx)
	if err != nil {
		return Builtin()
	}
	return d.Text
}

// Set replaces the company's part. Surrounding blank space is dropped, and
// empty clears it.
func (s *Service) Set(ctx context.Context, company, updatedBy string) (Document, error) {
	company = strings.TrimSpace(company)
	if len(company) > MaxCompany {
		return Document{}, errs.Newf(errs.ValidInvalid,
			"These instructions are %d KB, and the limit is %d KB. Every agent on this installation reads all of them in every conversation.",
			(len(company)+1023)/1024, MaxCompany/1024).
			WithRemedy("Shorten them, keeping the rules agents most need to follow.")
	}
	if !utf8.ValidString(company) || strings.ContainsRune(company, 0) {
		return Document{}, errs.New(errs.ValidInvalid, "These instructions contain bytes that are not text.").
			WithRemedy("Send them as UTF-8 text.")
	}
	stored, err := s.Store.Save(ctx, company, updatedBy)
	if err != nil {
		return Document{}, err
	}
	return document(stored), nil
}

func document(stored state.StoredInstructions) Document {
	d := Document{Builtin: Builtin(), Company: stored.Body, UpdatedBy: stored.UpdatedBy}
	if !stored.UpdatedAt.IsZero() {
		at := stored.UpdatedAt.UTC()
		d.UpdatedAt = &at
	}
	d.Text = d.Builtin
	if d.Company != "" {
		d.Text += "\n\n" + companyHeading + "\n\n" + d.Company
	}
	return d
}
