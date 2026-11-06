package spec

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/errs"
)

func hookRoute() WebhookRoute {
	return WebhookRoute{Name: "noodle", Method: "POST", Path: "/api/v1/webhook/noodle",
		Verification: WebhookVerification{Kind: WebhookHMACSHA256, Header: "X-Hub-Signature-256", Prefix: "sha256="}}
}

// webhookProblems is what validation says about routes alone.
func webhookProblems(routes ...WebhookRoute) []string {
	var got []string
	s := &AppSpec{WebhookRoutes: routes}
	validateWebhookRoutes(s, func(e *errs.Error) { got = append(got, e.Message) })
	return got
}

// TestR425_ARouteIsExactAndCannotClaimPandosPaths asserts what a route may name.
func TestR425_ARouteIsExactAndCannotClaimPandosPaths(t *testing.T) {
	require.Empty(t, webhookProblems(hookRoute()))

	for _, path := range []string{
		"", "api/v1", "/", "/a/*", "/a//b", "/a/../b", "/a/./b", "/a b", "/a%2Fb", "/a?b=1",
		"/.pando", "/.pando/login",
	} {
		r := hookRoute()
		r.Path = path
		require.NotEmpty(t, webhookProblems(r), "path %q", path)
	}
	for _, method := range []string{"", "GET", "DELETE", "post"} {
		r := hookRoute()
		r.Method = method
		require.NotEmpty(t, webhookProblems(r), "method %q", method)
	}
	for _, name := range []string{"", "Noodle", "a b", "-a", "a-", strings.Repeat("a", 64)} {
		r := hookRoute()
		r.Name = name
		require.NotEmpty(t, webhookProblems(r), "name %q", name)
	}
}

// TestR425_RoutesAreUnique asserts a name and a target each mean one route.
func TestR425_RoutesAreUnique(t *testing.T) {
	other := hookRoute()
	other.Path = "/other"
	require.Len(t, webhookProblems(hookRoute(), other), 1, "two routes named the same")

	other = hookRoute()
	other.Name = "other"
	require.Len(t, webhookProblems(hookRoute(), other), 1, "two routes for one target")
}

// TestR426_AProofCannotBeInAHeaderPandoOwnsOrTheConnectionUses asserts R-426's
// refused headers, and what only hmac_sha256 may set.
func TestR426_AProofCannotBeInAHeaderPandoOwnsOrTheConnectionUses(t *testing.T) {
	for _, header := range []string{
		"", "Cookie", "cookie", "Host", "X-Pando-User", "x-pando-assertion",
		"Connection", "Upgrade", "Content-Length", "Transfer-Encoding", "Bad Header",
	} {
		r := hookRoute()
		r.Verification.Header = header
		require.NotEmpty(t, webhookProblems(r), "header %q", header)
	}

	r := hookRoute()
	r.Verification.Kind = "stripe"
	require.NotEmpty(t, webhookProblems(r))

	r = hookRoute()
	r.Verification.Kind = WebhookHeader
	require.NotEmpty(t, webhookProblems(r), "a prefix is hmac_sha256's")

	r.Verification.Prefix = ""
	require.Empty(t, webhookProblems(r))
	r.AllowRepeats = true
	require.NotEmpty(t, webhookProblems(r), "allow_repeats is hmac_sha256's")
}

// TestR433_OnlyAnHMACRouteChecksReplays asserts which routes can.
func TestR433_OnlyAnHMACRouteChecksReplays(t *testing.T) {
	r := hookRoute()
	require.True(t, r.ChecksReplays())
	r.AllowRepeats = true
	require.False(t, r.ChecksReplays())
	r = hookRoute()
	r.Verification.Kind = WebhookHeader
	require.False(t, r.ChecksReplays())
}

// TestR425_ARouteChangeNeedsConfirmation asserts the diff class (design 01 §4),
// either way, and that an unchanged list is no change.
func TestR425_ARouteChangeNeedsConfirmation(t *testing.T) {
	without, with := &AppSpec{}, &AppSpec{WebhookRoutes: []WebhookRoute{hookRoute()}}

	require.True(t, Compare(without, with).RequiresConfirmation())
	require.True(t, Compare(with, without).RequiresConfirmation())
	require.True(t, Compare(with, with).Empty())
	require.True(t, Compare(without, &AppSpec{WebhookRoutes: []WebhookRoute{}}).Empty())
}

// TestR425_ARouteSurvivesRedetection asserts a re-detection does not stop a
// sender: nothing about a route is read from the repository.
func TestR425_ARouteSurvivesRedetection(t *testing.T) {
	pinned := &AppSpec{WebhookRoutes: []WebhookRoute{hookRoute()}}
	require.Equal(t, pinned.WebhookRoutes, Carry(pinned, &AppSpec{}).WebhookRoutes)
}

func TestWebhookRouteForNeedsMethodAndPath(t *testing.T) {
	s := &AppSpec{WebhookRoutes: []WebhookRoute{hookRoute()}}
	_, ok := s.WebhookRouteFor("POST", "/api/v1/webhook/noodle")
	require.True(t, ok)
	_, ok = s.WebhookRouteFor("PUT", "/api/v1/webhook/noodle")
	require.False(t, ok)
}
