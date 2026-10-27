package edgecert

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/state"
)

// TestR417_TwoControlHostsServeOneCertificateAndEitherAnswersTheChallenge
// asserts O-68's answer: with two control hosts sharing one database, the
// leader orders a hostname's certificate once, the CA's HTTP-01 request is
// answered whichever host it reaches, and the host that does not lead serves
// the very certificate the leader was issued, ordering nothing itself.
func TestR417_TwoControlHostsServeOneCertificateAndEitherAnswersTheChallenge(t *testing.T) {
	ctx := context.Background()
	leader, store, ca, now := setup(t)

	// The CA's request reaches the host that does not lead: its proxy reads
	// the database the leader wrote the answer to.
	hostA, hostB := Challenges{Store: store}, Challenges{Store: store}
	var answeredBy []string
	ca.answer = func(token string) (string, bool) {
		for name, host := range map[string]Challenges{"b": hostB, "a": hostA} {
			_, ok, err := host.KeyAuthorization(ctx, token)
			require.NoError(t, err)
			require.True(t, ok, "host %s answers a challenge it did not ask for", name)
			answeredBy = append(answeredBy, name)
		}
		ka, ok, _ := hostB.KeyAuthorization(ctx, token)
		return ka, ok
	}

	fromLeader, err := leader.Ensure(ctx, httpPlan())
	require.NoError(t, err)
	require.Equal(t, 1, ca.orders)
	require.True(t, ca.answered)
	require.ElementsMatch(t, []string{"a", "b"}, answeredBy)

	follower := Held{Store: store, Clock: fixedClock{now}}
	fromFollower, err := follower.Ensure(ctx, httpPlan())
	require.NoError(t, err)
	require.Equal(t, 1, ca.orders, "the follower orders nothing")
	require.Len(t, fromFollower, 1)
	require.Equal(t, fromLeader[0].CertPEM, fromFollower[0].CertPEM, "both edges serve one certificate")
	require.Equal(t, fromLeader[0].KeyPEM.Reveal(), fromFollower[0].KeyPEM.Reveal())

	// A hostname the leader has not issued for yet is left out, not ordered.
	plan := httpPlan()
	plan.Orders = append(plan.Orders, api.CertificateOrder{Name: "pando-tls-b.example.com", Domains: []string{"b.example.com"}})
	fromFollower, err = follower.Ensure(ctx, plan)
	require.NoError(t, err)
	require.Len(t, fromFollower, 1)
	require.Equal(t, 1, ca.orders)

	// Expired: no longer served.
	*now = now.Add(91 * 24 * time.Hour)
	fromFollower, err = follower.Ensure(ctx, httpPlan())
	require.NoError(t, err)
	require.Empty(t, fromFollower)
}

type failingStore struct{}

func (failingStore) Certificate(context.Context, string) (state.EdgeCertificate, bool, error) {
	return state.EdgeCertificate{}, false, errors.New("the database is down")
}

// TestAFollowerReportsWhatItCouldNotRead asserts a read failure is returned
// and a nil plan is nothing to serve.
func TestAFollowerReportsWhatItCouldNotRead(t *testing.T) {
	got, err := Held{Store: failingStore{}}.Ensure(context.Background(), httpPlan())
	require.Error(t, err)
	require.Empty(t, got)

	got, err = Held{Store: failingStore{}}.Ensure(context.Background(), nil)
	require.NoError(t, err)
	require.Nil(t, got)
}
