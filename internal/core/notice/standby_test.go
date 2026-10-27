package notice

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
)

type fakeStandbys struct {
	sb    *state.Standby
	err   error
	calls int
}

func (f *fakeStandbys) Get(context.Context, string) (state.Standby, bool, error) {
	f.calls++
	if f.sb == nil {
		return state.Standby{}, false, f.err
	}
	return *f.sb, true, f.err
}

// TestR420_AnAppWithVolumesSaysItsStandbysCopyMayBeInconsistent asserts the
// notice R-420 asks for: on an app whose standby holds copies of volumes,
// naming the host, and never on one with nothing to copy. It does not come
// back after each copy, since it says the same thing.
func TestR420_AnAppWithVolumesSaysItsStandbysCopyMayBeInconsistent(t *testing.T) {
	shipped := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	sb := &state.Standby{AppID: "app_1", Host: "app-2", Volumes: 1, Copies: 2, ShippedAt: &shipped}

	n := find(t, Compute(Inputs{Standby: sb}), KeyStandby)
	require.Contains(t, n.Message, "standby on app-2")
	require.Contains(t, n.Message, "may be inconsistent")
	require.NotContains(t, n.Message, "services adapter")

	later := shipped.Add(5 * time.Minute)
	moved := *sb
	moved.ShippedAt, moved.Copies = &later, 3
	require.Equal(t, n.Fingerprint, find(t, Compute(Inputs{Standby: &moved}), KeyStandby).Fingerprint)

	withDB := *sb
	withDB.Unreplicated = []string{"postgres"}
	n2 := find(t, Compute(Inputs{Standby: &withDB}), KeyStandby)
	require.Contains(t, n2.Message, "Its postgres data is copied the same way")
	require.NotEqual(t, n.Fingerprint, n2.Fingerprint)

	for _, none := range []*state.Standby{nil, {Host: "app-2"}, {Volumes: 1}} {
		require.NotContains(t, keys(Compute(Inputs{Standby: none})), KeyStandby)
	}
}

func TestR420_TheStandbyNoticeIsReadFromTheStandby(t *testing.T) {
	ctx := context.Background()
	s, _ := newService()
	standbys := &fakeStandbys{sb: &state.Standby{Host: "app-2", Volumes: 1}}
	s.Standbys = standbys
	got, err := s.List(ctx, "usr_a", app)
	require.NoError(t, err)
	require.Contains(t, keys(got), KeyStandby)

	n, err := s.Ignore(ctx, "usr_a", app, KeyStandby)
	require.NoError(t, err, "it can be ignored, as any notice (R-412)")
	require.True(t, n.Ignored)

	standbys.err = errors.New("boom")
	_, err = s.List(ctx, "usr_a", app)
	require.Error(t, err)
}
