//go:build integration

package state_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

func sha(c string) string { return strings.Repeat(c, 64) }

// TestR421_ARenewedCertificateIsRecordedWithTheOneBefore asserts R-421
// against the database: a renewal records the new certificate, with its PEM,
// and keeps the one presented as the host's until the host presents the new
// one. A certificate is renewed once: a second renewal presenting it, at the
// same time or after the first's answer was lost, changes nothing and is
// given the row with what the first was answered with; a certificate that is
// not the host's renews nothing.
func TestR421_ARenewedCertificateIsRecordedWithTheOneBefore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, _ := clusterStore(t)
	issued, err := c.CreateJoinToken(ctx, []string{"app"}, 1, "usr_ada", time.Now().Add(time.Hour))
	require.NoError(t, err)
	h, err := c.AddHost(ctx, state.Host{Name: "app-1", Roles: []string{"app"}, Address: "10.0.0.2:7443", CertSHA256: sha("a"), JoinedWith: issued.Token.ID})
	require.NoError(t, err)
	require.Equal(t, sha("a"), h.Pins())

	h, won, err := c.RenewHostCert(ctx, h.ID, sha("a"), sha("b"), []byte("pem-b"))
	require.NoError(t, err)
	require.True(t, won)
	require.Equal(t, sha("b")+","+sha("a"), h.Pins())

	// Renewing the same one again records nothing: the first's answer stands.
	h, won, err = c.RenewHostCert(ctx, h.ID, sha("a"), sha("c"), []byte("pem-c"))
	require.NoError(t, err)
	require.False(t, won)
	require.Equal(t, sha("b")+","+sha("a"), h.Pins())
	require.Equal(t, []byte("pem-b"), h.NextCertPEM)

	_, _, err = c.RenewHostCert(ctx, h.ID, sha("d"), sha("e"), nil)
	require.Equal(t, errs.AuthInvalid, errs.CodeOf(err), "a certificate that is not its own")

	changed, err := c.ConfirmHostCert(ctx, h.ID, sha("a"))
	require.NoError(t, err)
	require.False(t, changed, "the old one is not the newest")
	changed, err = c.ConfirmHostCert(ctx, h.ID, sha("b"))
	require.NoError(t, err)
	require.True(t, changed)
	got, err := c.Host(ctx, h.ID)
	require.NoError(t, err)
	require.Equal(t, sha("b"), got.Pins())
	require.Empty(t, got.NextCertPEM)
}

// TestR415_ARemovedHostsCertificatesAreRevoked asserts R-415 against the
// database: removing a host revokes both certificates it held, in the same
// transaction, and a revoked certificate renews nothing.
func TestR415_ARemovedHostsCertificatesAreRevoked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, _ := clusterStore(t)
	issued, err := c.CreateJoinToken(ctx, []string{"app"}, 1, "usr_ada", time.Now().Add(time.Hour))
	require.NoError(t, err)
	h, err := c.AddHost(ctx, state.Host{Name: "app-1", Roles: []string{"app"}, Address: "10.0.0.2:7443", CertSHA256: sha("1"), JoinedWith: issued.Token.ID})
	require.NoError(t, err)
	_, _, err = c.RenewHostCert(ctx, h.ID, sha("1"), sha("2"), nil)
	require.NoError(t, err)

	_, err = c.RemoveHost(ctx, h.ID)
	require.NoError(t, err)
	for _, s := range []string{sha("1"), sha("2")} {
		revoked, err := c.CertRevoked(ctx, s)
		require.NoError(t, err)
		require.True(t, revoked, s)
	}
	revoked, err := c.CertRevoked(ctx, sha("3"))
	require.NoError(t, err)
	require.False(t, revoked)
}

// TestR414_TheInstallHostCannotBeRemovedOrBecomeOne asserts R-414 against the
// database: the install's own first host is refused removal, and a join
// token cannot be deleted while a host joined with it, so no joined host is
// ever taken for the install's first.
func TestR414_TheInstallHostCannotBeRemovedOrBecomeOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, db := clusterStore(t)
	first, ok, err := c.RecordFirstHost(ctx, state.Host{Name: "control", Roles: []string{"app", "control"}, Address: "10.0.0.1:7443", CertSHA256: sha("0")})
	require.NoError(t, err)
	require.True(t, ok)
	_, err = c.RemoveHost(ctx, first.ID)
	require.ErrorIs(t, err, state.ErrInstallHost)
	_, err = c.Host(ctx, first.ID)
	require.NoError(t, err, "still there")

	issued, err := c.CreateJoinToken(ctx, []string{"app"}, 1, "usr_ada", time.Now().Add(time.Hour))
	require.NoError(t, err)
	_, err = c.AddHost(ctx, state.Host{Name: "app-1", Roles: []string{"app"}, Address: "10.0.0.2:7443", CertSHA256: sha("e"), JoinedWith: issued.Token.ID})
	require.NoError(t, err)
	_, err = db.Exec(ctx, `DELETE FROM join_tokens WHERE id = $1`, issued.Token.ID)
	require.Error(t, err, "a token a host joined with is kept")
}
