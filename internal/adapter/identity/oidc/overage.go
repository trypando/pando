package oidc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/trypando/pando/internal/errs"
)

// Microsoft Entra ID leaves the groups claim out of a token when a person is
// in more groups than fit (200 in a JWT), and says so instead: a distributed
// claim, _claim_names {"groups": "src1"} with the source in _claim_sources, or
// hasgroups: true from the implicit flow. Signing such a person in with the
// groups the token carried — none — would record that they left every group
// they are in. So the groups are read from Microsoft Graph, and when they
// cannot be, the sign-in is refused: a person whose groups are unknown is not
// a person in no groups (R-078, R-079).

// graphHosts are the Microsoft Graph hosts of each Microsoft cloud. The
// endpoint a token names is used only to pick one of these: the access token
// is never sent anywhere a token merely says to send it.
var graphHosts = map[string]string{
	"graph.microsoft.com":             "https://graph.microsoft.com",
	"graph.windows.net":               "https://graph.microsoft.com", // retired Azure AD Graph
	"graph.microsoft.us":              "https://graph.microsoft.us",
	"dod-graph.microsoft.us":          "https://dod-graph.microsoft.us",
	"microsoftgraph.chinacloudapi.cn": "https://microsoftgraph.chinacloudapi.cn",
}

// entraIssuerHosts are the sign-in hosts of each Microsoft cloud.
var entraIssuerHosts = map[string]bool{
	"login.microsoftonline.com":        true,
	"sts.windows.net":                  true, // v1.0 issuer
	"login.microsoftonline.us":         true,
	"login.partner.microsoftonline.cn": true,
	"login.chinacloudapi.cn":           true,
}

// overage reports whether the token says the named claim was left out, and
// the endpoint it gave for it, if any.
func overage(claims map[string]any, name string) (bool, string) {
	if _, present := claims[name]; present {
		return false, ""
	}
	if names, ok := claims["_claim_names"].(map[string]any); ok {
		if src, ok := names[name].(string); ok && src != "" {
			endpoint := ""
			if sources, ok := claims["_claim_sources"].(map[string]any); ok {
				if s, ok := sources[src].(map[string]any); ok {
					endpoint, _ = s["endpoint"].(string)
				}
			}
			return true, endpoint
		}
	}
	if name == "groups" && claimBool(claims, "hasgroups") {
		return true, ""
	}
	return false, ""
}

// overageGroups reads the groups a token left out. Only Microsoft Entra ID's
// overage can be followed; any other provider's distributed groups claim is
// refused with what to change.
func (a *Adapter) overageGroups(ctx context.Context, endpoint, accessToken string) ([]string, error) {
	base := a.graphURL
	if base == "" {
		base = graphBase(a.cfg.Issuer, endpoint)
	}
	if base == "" {
		return nil, errs.New(errs.AuthInvalid,
			"The identity provider left this person's groups out of the ID token and pointed Pando somewhere else to read them, which Pando does not do for this provider.").
			WithRemedy("Configure the provider to put the groups in the ID token or the userinfo response, or limit which groups it sends so they fit.")
	}
	if accessToken == "" {
		return nil, graphRefused("it returned no access token to read them with")
	}

	// /me/getMemberObjects needs only the delegated User.Read permission, and
	// is transitive like the groups claim. Security groups only, as the
	// groups claim's default "Security groups" setting sends.
	body := []byte(`{"securityEnabledOnly":true}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1.0/me/getMemberObjects", bytes.NewReader(body))
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not build the request for the person's groups.", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, errs.Wrap(errs.AdapterUnavailable,
			"Microsoft Entra ID left this person's groups out of the ID token because they are in more than 200, and Pando could not reach Microsoft Graph to read them.", err).
			WithRemedy("Check that Pando can reach " + base + ", then sign in again.")
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, graphRefused(fmt.Sprintf("Microsoft Graph answered HTTP %d%s", resp.StatusCode, graphReason(raw)))
	}
	var out struct {
		Value []string `json:"value"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, graphRefused("Microsoft Graph's answer could not be read")
	}
	groups := make([]string, 0, len(out.Value))
	for _, g := range out.Value {
		if g = strings.TrimSpace(g); g != "" {
			groups = append(groups, g)
		}
	}
	return groups, nil
}

func graphRefused(why string) error {
	return errs.Newf(errs.AuthInvalid,
		"Microsoft Entra ID left this person's groups out of the ID token because they are in more than 200, and Pando could not read them from Microsoft Graph: %s. Pando does not sign someone in whose groups it cannot tell.", why).
		WithRemedy("In the app registration, grant the delegated Microsoft Graph permission User.Read and add User.Read to this " +
			"provider's scopes. Or, under Token configuration, send only groups assigned to the application, so they fit in the token.")
}

// graphReason is Microsoft Graph's own error code, when it gave one.
func graphReason(raw []byte) string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error.Code != "" {
		return " (" + e.Error.Code + ")"
	}
	return ""
}

// graphBase is the Microsoft Graph to ask for an Entra ID issuer: the cloud
// the token's endpoint names, or the global one. "" for any other issuer.
func graphBase(issuer, endpoint string) string {
	if !entraIssuerHosts[issuerHost(issuer)] {
		return ""
	}
	if u, err := url.Parse(endpoint); err == nil {
		if b := graphHosts[strings.ToLower(u.Hostname())]; b != "" {
			return b
		}
	}
	return "https://graph.microsoft.com"
}

func issuerHost(issuer string) string {
	u, err := url.Parse(issuer)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}
