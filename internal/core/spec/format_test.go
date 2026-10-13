package spec_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// TestR130_ATypedSecretRefusesAValueTheAppWouldRefuse asserts R-130 and
// R-105. A hole with a type is checked against it: a placeholder somebody
// typed for a Laravel APP_KEY deployed, and every page failed with
// "Unsupported cipher or incorrect key length". Refused when it is set, the
// message says what a valid one looks like.
func TestR130_ATypedSecretRefusesAValueTheAppWouldRefuse(t *testing.T) {
	key := "base64:" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
	cases := []struct {
		format spec.SecretFormat
		good   []string
		bad    []string
	}{
		{spec.FormatLaravelKey, []string{key, strings.Repeat("a", 32)},
			[]string{"qa-placeholder", "base64:short", "base64:" + base64.StdEncoding.EncodeToString([]byte("16 bytes only!!!"))}},
		{spec.FormatRandomLong, []string{strings.Repeat("ab", 32)}, []string{"secret", strings.Repeat("a", 63)}},
		{spec.FormatDjangoKey, []string{"abcdefghij" + strings.Repeat("x", 40)},
			[]string{strings.Repeat("a", 60), "django-insecure-" + strings.Repeat("abcdef", 10), "short"}},
		{spec.FormatRandom, []string{"anything"}, nil},
		{"", []string{"anything"}, nil},
	}
	for _, tc := range cases {
		for _, v := range tc.good {
			require.NoError(t, tc.format.Check("KEY", v), "%s accepts %q", tc.format, v)
		}
		for _, v := range tc.bad {
			err := tc.format.Check("KEY", v)
			require.Error(t, err, "%s refuses %q", tc.format, v)
			e := errs.As(err)
			require.Equal(t, errs.ValidInvalid, e.Code)
			require.Contains(t, e.Message, "KEY", "the message names the variable")
			require.NotEmpty(t, e.Remedy, "and says how to make a valid one")
		}
	}
}

// A slot's format is part of the spec, so an unknown one is refused and a
// value kept readable in the spec is checked like a literal is.
func TestASlotFormatIsValidated(t *testing.T) {
	s := valid()
	s.Slots = []spec.Slot{{Key: "APP_KEY", Type: spec.SlotUnknown, Format: "rot13"}}
	require.Error(t, spec.Validate(s))

	s.Slots = []spec.Slot{{Key: "APP_KEY", Type: spec.SlotUnknown, Format: spec.FormatLaravelKey,
		Resolution: &spec.Resolution{Mode: spec.ResolutionBound, Target: "changeme"}}}
	require.Error(t, spec.Validate(s))

	require.Error(t, spec.CheckSlotValue(s, "APP_KEY", "changeme"))
	require.NoError(t, spec.CheckSlotValue(s, "OTHER", "changeme"), "no slot, nothing to check against")
}
