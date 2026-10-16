package proxy

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR173_AnAppCannotSetPandosCookies asserts R-173 in the other direction:
// an app's response may not set a cookie in Pando's namespace. On Pando's
// origin (path routing) or host (port routing) it would be one Pando reads,
// and setting pando_session is how an app would sign its visitor in as the
// account of its choosing. The app's own cookies pass untouched, in order.
func TestR173_AnAppCannotSetPandosCookies(t *testing.T) {
	h := http.Header{}
	h.Add("Set-Cookie", "theme=dark; Path=/")
	h.Add("Set-Cookie", "pando_session=ses_attacker; Path=/")
	h.Add("Set-Cookie", "__Host-pando_session=ses_attacker; Path=/; Secure")
	h.Add("Set-Cookie", " pando_pass_app_01=forged")
	h.Add("Set-Cookie", "__secure-pando_later=forged")
	h.Add("Set-Cookie", "cart=two-items")
	h.Add("Set-Cookie", "__Host-theme=light; Path=/; Secure")
	// Cookie names are case-sensitive: this is not one Pando reads.
	h.Add("Set-Cookie", "PANDO_theirs=1")

	stripSetCookies(h)

	require.Equal(t, []string{
		"theme=dark; Path=/",
		"cart=two-items",
		"__Host-theme=light; Path=/; Secure",
		"PANDO_theirs=1",
	}, h.Values("Set-Cookie"))
	for _, v := range h.Values("Set-Cookie") {
		require.NotContains(t, v, "attacker")
		require.NotContains(t, v, "forged")
	}
}
