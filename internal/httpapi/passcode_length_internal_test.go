package httpapi

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR075a_APasscodeIsCountedInCharacters asserts R-075a's minimum length in
// characters, as the console counts it, not bytes: three "é" are six bytes and
// three characters, and are refused.
func TestR075a_APasscodeIsCountedInCharacters(t *testing.T) {
	_, err := passcodeDigest("ééé")
	require.Error(t, err)
	require.Contains(t, err.Error(), "at least 6 characters")

	_, err = passcodeDigest("  abcde  ")
	require.Error(t, err, "surrounding spaces are not characters of the passcode")

	digest, err := passcodeDigest("éééééé")
	require.NoError(t, err)
	require.NotEmpty(t, digest)
}
