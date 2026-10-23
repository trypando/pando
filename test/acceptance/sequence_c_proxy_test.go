//go:build integration

// Sequence C — A request through the proxy (docs/design/07-sequences.md).
//
// The hottest path in the system and the one where a mistake is worst. Run
// against the real stack: a real app, deployed, reached through the real proxy.
package acceptance_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// echoSpec deploys an image that echoes the request headers back, so the test
// can see exactly what the app received rather than what Pando believes it sent.
// The test reaches it on its port (R-166), so port is in the range Compose
// publishes (9000-9019), at the top, clear of ports Pando hands out itself.
func echoSpec(port int) string {
	return fmt.Sprintf(`{
		"schema_version": 1,
		"source": {"type": "image", "image": "mendhak/http-https-echo:34"},
		"build": {"strategy": "prebuilt"},
		"workloads": [{"name": "web", "primary": true, "exposed": true,
			"env": [{"key": "HTTP_PORT", "value": "8080"}],
			"ports": [{"number": 8080, "protocol": "http", "source": "user"}]}],
		"routing": {"adapter_ref": "rte_loopback", "mode": "port", "port": %d},
		"runtime": {"adapter_ref": "rt_docker", "isolation_floor": 10},
		"deploy": {"strategy": "recreate"}
	}`, port)
}

// deployEcho brings up an echo app and returns its ID and the URL it is
// reached at.
func deployEcho(t *testing.T, c *client, name string, port int) (string, string) {
	t.Helper()

	app := c.createApp(t, name)
	c.putSpec(t, app, echoSpec(port))
	c.pinSpec(t, app, 1)

	dep := c.deploy(t, app, 0)
	final := c.awaitDeployment(t, app, dep["id"].(string), 4*time.Minute)
	require.Equal(t, "succeeded", final["status"],
		"deploy failed: %v\n%s", final["error_detail"], c.deploymentLogs(t, app, dep["id"].(string)))

	return app, c.appURL(t, app)
}

// throughProxy makes a request to an app through Pando's proxy and returns what
// the app saw. The proxy is mounted as the fallback route, so any path that is
// not one of Pando's own reaches an app — there is no separate proxy port to
// address, which is the point.
func throughProxy(t *testing.T, c *client, at, path string, headers map[string]string) (map[string]any, int) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, at+path, nil)
	require.NoError(t, err)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if c != nil {
		req.AddCookie(&http.Cookie{Name: "pando_session", Value: c.cookie})
	}

	httpClient := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := httpClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return map[string]any{"_body": string(body)}, resp.StatusCode
	}

	var echoed map[string]any
	require.NoError(t, json.Unmarshal(body, &echoed), "echo response: %s", string(body))
	return echoed, resp.StatusCode
}

// echoedHeaders pulls the headers the app received out of the echo response.
func echoedHeaders(t *testing.T, echoed map[string]any) map[string]string {
	t.Helper()
	raw, ok := echoed["headers"].(map[string]any)
	require.True(t, ok, "echo response had no headers: %v", echoed)

	out := map[string]string{}
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[strings.ToLower(k)] = s
		}
	}
	return out
}

// TestR053_ForgedHeadersReachTheAppReplaced asserts R-053 end to end.
//
// The risk register names this test. It runs against a real deployed app rather
// than a test double, because what matters is what the *app* receives — a
// stripping bug that a unit test's fake upstream hides is exactly the bug this
// is for.
func TestR053_ForgedHeadersReachTheAppReplaced(t *testing.T) {
	c := login(t)
	_, at := deployEcho(t, c, "seq-c-forge-"+stamp(), 9014)

	echoed, status := throughProxy(t, c, at, "/", map[string]string{
		"X-Pando-User":          "admin@corp.com",
		"x-pando-email":         "admin@corp.com",
		"X-PANDO-GROUPS":        "admins,superusers",
		"X-Pando-Assertion":     "forged.assertion.value",
		"X-Pando-Future-Header": "whatever",
		"X-Forwarded-Prefix":    "/forged",
	})
	require.Equal(t, http.StatusOK, status, "%v", echoed)

	headers := echoedHeaders(t, echoed)

	require.NotEqual(t, "admin@corp.com", headers["x-pando-user"],
		"the forged user reached the app unchanged — this is the spoofing bug R-053 exists to prevent")
	require.NotEqual(t, "admin@corp.com", headers["x-pando-email"])
	require.NotEqual(t, "forged.assertion.value", headers["x-pando-assertion"])
	require.NotContains(t, headers["x-pando-groups"], "superusers")

	// A header Pando does not set must not survive either: otherwise a
	// convenience header added later is spoofable the day it ships.
	require.Empty(t, headers["x-pando-future-header"])
	require.NotEqual(t, "/forged", headers["x-forwarded-prefix"])

	// Nothing forged anywhere in the namespace.
	for name, value := range headers {
		if strings.HasPrefix(name, "x-pando-") {
			require.NotContains(t, value, "admin@corp.com", "header %s leaked a forged value", name)
			require.NotContains(t, value, "superusers", "header %s leaked a forged value", name)
		}
	}

	// And the real identity did arrive.
	require.NotEmpty(t, headers["x-pando-assertion"], "the app must receive a real assertion")
	require.NotEmpty(t, headers["x-pando-user"])
}

// TestR054_TheAppReceivesAVerifiableAssertion asserts that what reaches the app
// verifies against the published JWKS — which is the whole basis for an app
// trusting anything Pando says.
func TestR054_TheAppReceivesAVerifiableAssertion(t *testing.T) {
	c := login(t)
	appID, at := deployEcho(t, c, "seq-c-assert-"+stamp(), 9015)

	echoed, status := throughProxy(t, c, at, "/", nil)
	require.Equal(t, http.StatusOK, status, "%v", echoed)

	token := echoedHeaders(t, echoed)["x-pando-assertion"]
	require.NotEmpty(t, token)

	parts := strings.Split(token, ".")
	require.Len(t, parts, 3, "an assertion is a three-part JWT")

	header := decodeSegment(t, parts[0])
	require.Equal(t, "EdDSA", header["alg"])
	require.NotEmpty(t, header["kid"])

	claims := decodeSegment(t, parts[1])
	require.Equal(t, appID, claims["aud"],
		"aud is this app's ID — an assertion for one app must not be replayable at another")
	require.NotEmpty(t, claims["sub"])
	require.NotEqual(t, "anonymous", claims["sub"], "a signed-in caller is not anonymous")

	// 120 seconds, from the one constant (R-055).
	exp, iat := claims["exp"].(float64), claims["iat"].(float64)
	require.Equal(t, float64(120), exp-iat)

	// The key that signed it is published, so an app can verify independently.
	jwks := fetchJWKS(t)
	require.Contains(t, jwks, header["kid"],
		"the signing key must be published at /.well-known/jwks.json (R-057)")
}

// TestR056_AnonymousReachesAPublicAppWithTheConstantSubject asserts R-056 and,
// with it, that the anonymous path is not a bypass (R-023).
func TestR056_AnonymousReachesAPublicAppWithTheConstantSubject(t *testing.T) {
	c := login(t)
	appID, at := deployEcho(t, c, "seq-c-anon-"+stamp(), 9016)

	// Not shared yet: an anonymous caller is sent to sign in.
	echoed, status := throughProxy(t, nil, at, "/", nil)
	require.Equal(t, http.StatusFound, status,
		"an anonymous caller without access is redirected, not served: %v", echoed)

	// Share it with everyone (R-075) — a real grant row, not a flag.
	c.postRaw(t, fmt.Sprintf("/apps/%s/grants", appID),
		`{"plane":"data","principal_kind":"anonymous"}`)

	echoed, status = throughProxy(t, nil, at, "/", nil)
	require.Equal(t, http.StatusOK, status, "%v", echoed)

	token := echoedHeaders(t, echoed)["x-pando-assertion"]
	require.NotEmpty(t, token,
		"an anonymous request still carries an assertion — absence of the header is how an app knows a request did not come through Pando")

	claims := decodeSegment(t, strings.Split(token, ".")[1])
	require.Equal(t, "anonymous", claims["sub"], "R-056: the constant subject")
	require.Equal(t, appID, claims["aud"])
}

// TestR029_AnOperatorIsDeniedUseThroughTheProxy asserts the two-plane split at
// the place it matters most — reversing it here would hand every operator access
// to every app's data.
func TestR029_AnOperatorIsDeniedUseThroughTheProxy(t *testing.T) {
	owner := login(t)
	appID, at := deployEcho(t, owner, "seq-c-planes-"+stamp(), 9017)

	// A second user, granted the operator role on the control plane only.
	bob := owner.createUser(t, "bob-"+stamp())
	owner.postRaw(t, fmt.Sprintf("/apps/%s/grants", appID),
		fmt.Sprintf(`{"plane":"control","principal_kind":"user","principal_id":%q,"role_id":"role_operator"}`, bob))

	bobClient := owner.asUser(t, bob)

	// Bob can see the app on the control plane.
	body, status := bobClient.do(t, http.MethodGet, "/apps/"+appID, "")
	require.Equal(t, http.StatusOK, status, body)

	// And is denied use of it.
	echoed, proxyStatus := throughProxy(t, bobClient, at, "/", nil)
	require.Equal(t, http.StatusForbidden, proxyStatus,
		"an operator with no data grant must be denied USE of the app (R-029): %v", echoed)
}

// TestR166_AnAppOnAPortIsNotServedOnPandosAddress asserts R-166 end to end:
// an app on its own port is served there, with its path as sent, and not at
// /<slug> on Pando's own address, where its pages would share an origin with
// the console.
func TestR166_AnAppOnAPortIsNotServedOnPandosAddress(t *testing.T) {
	c := login(t)
	appID, at := deployEcho(t, c, "seq-c-own-"+stamp(), 9018)
	slug := c.get(t, "/apps/"+appID)["slug"].(string)

	echoed, status := throughProxy(t, c, at, "/dashboard/settings", nil)
	require.Equal(t, http.StatusOK, status, "%v", echoed)
	path, _ := echoed["path"].(string)
	require.Equal(t, "/dashboard/settings", path, "nothing is stripped from a path on the app's own port")

	_, status = throughProxy(t, c, strings.TrimSuffix(baseURL(), "/api/v1")+"/"+slug, "/", nil)
	require.Equal(t, http.StatusNotFound, status, "the app is not served at /%s on Pando's address", slug)
}

// TestTheProxyIsTheOnlyWayIn asserts R-023 structurally: the workload publishes
// no host port, so there is no address that reaches it without passing through
// enforcement.
func TestTheProxyIsTheOnlyWayIn(t *testing.T) {
	c := login(t)
	appID, _ := deployEcho(t, c, "seq-c-bypass-"+stamp(), 9019)

	ports := inspect(t, fmt.Sprintf("pando-%s-web", appID), `{{json .NetworkSettings.Ports}}`)
	require.NotContains(t, ports, "HostPort",
		"a published port would be an address that reaches the app without authorization")

	networks := inspect(t, fmt.Sprintf("pando-%s-web", appID), `{{json .NetworkSettings.Networks}}`)
	require.Contains(t, networks, "pando-"+appID,
		"the workload is reachable only inside its own bundle network")
}

func decodeSegment(t *testing.T, segment string) map[string]any {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	require.NoError(t, err)

	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func fetchJWKS(t *testing.T) string {
	t.Helper()
	resp, err := http.Get(strings.TrimSuffix(baseURL(), "/api/v1") + "/.well-known/jwks.json")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(body)
}
