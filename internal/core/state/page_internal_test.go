package state

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestACursorRoundTripsAndAForgedOneIsRefused(t *testing.T) {
	at := time.Date(2026, 10, 6, 9, 0, 0, 123456000, time.UTC)
	c := encodeCursor(at, "app_01")

	var gotAt time.Time
	var gotID string
	have, err := decodeCursor(c, &gotAt, &gotID)
	require.NoError(t, err)
	require.True(t, have)
	require.True(t, at.Equal(gotAt), "to the microsecond Postgres keeps")
	require.Equal(t, "app_01", gotID)

	have, err = decodeCursor("", &gotAt, &gotID)
	require.NoError(t, err)
	require.False(t, have)

	for _, bad := range []string{"!!!", encodeCursor("only one"), encodeCursor(1, 2)} {
		_, err := decodeCursor(bad, &gotAt, &gotID)
		require.ErrorIs(t, err, ErrBadCursor, bad)
	}
}

func TestAPageIsNeverLargerThanTheMaximum(t *testing.T) {
	require.Equal(t, DefaultPageSize, Page{}.Size())
	require.Equal(t, 7, Page{Limit: 7}.Size())
	require.Equal(t, MaxPageSize, Page{Limit: 1_000_000}.Size())
}

// TestSCIMTotalIsCountedUnlessTheFirstPageShowsIt covers the SCIM lists'
// totalResults (RFC 7644 §3.4.2), which is required on every page.
func TestSCIMTotalIsCountedUnlessTheFirstPageShowsIt(t *testing.T) {
	total, ok := totalFromPage(0, 100, 1)
	require.True(t, ok)
	require.Equal(t, 1, total)

	_, ok = totalFromPage(0, 100, 100) // full: there may be more
	require.False(t, ok)
	_, ok = totalFromPage(100, 100, 3) // a later page cannot see the earlier ones
	require.False(t, ok)
}
