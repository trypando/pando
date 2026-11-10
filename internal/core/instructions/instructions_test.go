package instructions

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
)

type fakeStore struct {
	row     state.StoredInstructions
	loadErr error
	saves   int
}

func (f *fakeStore) Load(context.Context) (state.StoredInstructions, error) {
	return f.row, f.loadErr
}

func (f *fakeStore) Save(_ context.Context, body, by string) (state.StoredInstructions, error) {
	f.saves++
	f.row = state.StoredInstructions{Body: body, UpdatedBy: by, UpdatedAt: time.Date(2026, 11, 4, 9, 0, 0, 0, time.FixedZone("x", 3600))}
	return f.row, nil
}

// TestR424_AgentsGetPandosGuidanceAndThenTheCompanys asserts R-424: an agent
// is told Pando's own guidance, then what the administrators wrote, under a
// heading saying theirs wins where the two differ.
func TestR424_AgentsGetPandosGuidanceAndThenTheCompanys(t *testing.T) {
	store := &fakeStore{}
	s := &Service{Store: store}

	d, err := s.Get(context.Background())
	require.NoError(t, err)
	require.Equal(t, Builtin(), d.Text, "nothing written: Pando's guidance alone")
	require.Empty(t, d.Company)
	require.Nil(t, d.UpdatedAt)
	require.Contains(t, d.Builtin, "X-Pando-Assertion")
	require.Contains(t, d.Builtin, "Never commit a secret")

	d, err = s.Set(context.Background(), "\n  Use Go for new services.\n\n", "usr_admin")
	require.NoError(t, err)
	require.Equal(t, "Use Go for new services.", d.Company, "surrounding blank space is dropped")
	require.Equal(t, "usr_admin", d.UpdatedBy)
	require.NotNil(t, d.UpdatedAt)
	require.Equal(t, time.UTC, d.UpdatedAt.Location())
	require.True(t, strings.HasPrefix(d.Text, Builtin()), "Pando's guidance first")
	require.True(t, strings.HasSuffix(d.Text, "\n\nUse Go for new services."), "the company's last")
	require.Contains(t, d.Text, "follow these")

	cleared, err := s.Set(context.Background(), "   ", "usr_admin")
	require.NoError(t, err)
	require.Equal(t, Builtin(), cleared.Text, "cleared: Pando's guidance alone again")
	require.NotNil(t, cleared.UpdatedAt, "and who cleared it is kept")
}

func TestSetRefusesTooLongOrNotText(t *testing.T) {
	store := &fakeStore{}
	s := &Service{Store: store}

	_, err := s.Set(context.Background(), strings.Repeat("a", MaxCompany+1), "usr_admin")
	var e *errs.Error
	require.ErrorAs(t, err, &e)
	require.Equal(t, errs.ValidInvalid, e.Code)
	require.Contains(t, e.Message, "the limit is 32 KB")

	_, err = s.Set(context.Background(), "a\x00b", "usr_admin")
	require.ErrorAs(t, err, &e)
	require.Contains(t, e.Message, "not text")

	_, err = s.Set(context.Background(), "\xff\xfe", "usr_admin")
	require.ErrorAs(t, err, &e)

	require.Zero(t, store.saves, "nothing refused is saved")

	_, err = s.Set(context.Background(), strings.Repeat("a", MaxCompany), "usr_admin")
	require.NoError(t, err, "the limit itself is allowed")
}

// An agent connecting while the company's part cannot be read still gets
// Pando's guidance; the API reports the failure.
func TestTextFallsBackToBuiltin(t *testing.T) {
	s := &Service{Store: &fakeStore{loadErr: errors.New("down")}}
	require.Equal(t, Builtin(), s.Text(context.Background()))
	_, err := s.Get(context.Background())
	require.Error(t, err)

	s = &Service{Store: &fakeStore{row: state.StoredInstructions{Body: "Be brief.", UpdatedAt: time.Now()}}}
	require.True(t, strings.HasSuffix(s.Text(context.Background()), "Be brief."))
}

// TestR054_AgentsAreToldTheGroupsClaimHoldsGroupIDs asserts R-054 as an agent
// reads it: an app deciding by group reads the signed `groups` claim, and
// finds IDs there, not the names a person sees.
func TestR054_AgentsAreToldTheGroupsClaimHoldsGroupIDs(t *testing.T) {
	text := Builtin()
	require.Contains(t, text, "`groups` claim of the verified assertion")
	require.Contains(t, text, "group IDs such as `"+string(id.Group)+"_")
	require.Contains(t, text, "`X-Pando-Groups` are not verified")
}
