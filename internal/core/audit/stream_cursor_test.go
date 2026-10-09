package audit_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/errs"
)

// TestR381_ACursorRoundTripsThroughItsWireForm asserts R-381's resumable
// cursor: what String writes ParseCursor reads back, and empty is the start.
func TestR381_ACursorRoundTripsThroughItsWireForm(t *testing.T) {
	for _, c := range []audit.Cursor{{}, {TxID: 48213, ID: 10442}, {TxID: 0, ID: 7}, {TxID: 1<<64 - 1, ID: 1<<63 - 1}} {
		got, err := audit.ParseCursor(c.String())
		require.NoError(t, err)
		assert.Equal(t, c, got)
	}
	assert.Equal(t, "c1.48213.10442", audit.Cursor{TxID: 48213, ID: 10442}.String())

	start, err := audit.ParseCursor("")
	require.NoError(t, err)
	assert.True(t, start.IsZero(), "an absent cursor is the start of the live log")
	assert.False(t, audit.Cursor{ID: 1}.IsZero())
	assert.False(t, audit.Cursor{TxID: 1}.IsZero())
}

// TestR381_AMalformedCursorIsRefusedReadably asserts a cursor that is not one
// String wrote is refused as invalid input, with a message that names it and
// says what a valid one looks like (R-105).
func TestR381_AMalformedCursorIsRefusedReadably(t *testing.T) {
	for _, bad := range []string{
		"48213.10442",      // no version prefix
		"c2.48213.10442",   // a version this Pando does not write
		"c1.48213",         // no id
		"c1.x.10442",       // txid not a number
		"c1.48213.y",       // id not a number
		"c1.-1.10442",      // negative txid
		"c1.48213.-5",      // negative id
		"c1.48213.10442.1", // trailing part
		"now",              // resolved by Reader.ResolveCursor, not a cursor
	} {
		_, err := audit.ParseCursor(bad)
		e := errs.As(err)
		require.NotNil(t, e, bad)
		assert.Equal(t, errs.ValidInvalid, e.Code, bad)
		assert.Contains(t, e.Message, `"`+bad+`" is not an audit stream cursor.`)
		assert.Contains(t, e.Message, "such as c1.48213.10442")
	}
}

// TestStreamPageSizeDefaultsAndClamps asserts a read asks for 500 by default
// and never more than 1000.
func TestStreamPageSizeDefaultsAndClamps(t *testing.T) {
	assert.Equal(t, 500, audit.StreamPageSize(0))
	assert.Equal(t, 500, audit.StreamPageSize(-3))
	assert.Equal(t, 1, audit.StreamPageSize(1))
	assert.Equal(t, 1000, audit.StreamPageSize(1000))
	assert.Equal(t, 1000, audit.StreamPageSize(5000))
}
