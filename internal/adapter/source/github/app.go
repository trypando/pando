package github

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/trypando/pando/internal/adapter/source/forgekit"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// checkApp refuses an App connection Pando could not mint a token with, at
// configuration rather than at the first clone, and returns the parsed key.
// Nothing about the key is put in an error.
func checkApp(cfg *settings) (*rsa.PrivateKey, error) {
	cfg.AppID = strings.TrimSpace(cfg.AppID)
	cfg.InstallationID = strings.TrimSpace(cfg.InstallationID)
	if cfg.AppID == "" {
		return nil, errs.New(errs.ValidInvalid, "A GitHub App connection needs the App's ID.").
			WithRemedy("Copy the App ID from the App's settings page in GitHub, under General → About.")
	}
	if cfg.InstallationID != "" && !digits(cfg.InstallationID) {
		return nil, errs.Newf(errs.ValidInvalid,
			"%q is not a GitHub App installation ID. It is a number, such as 45678901.", cfg.InstallationID).
			WithRemedy("Copy the number at the end of the installation's settings address, or leave it empty to look it up from the owner.")
	}
	if cfg.InstallationID == "" && cfg.Scope == "" {
		return nil, errs.New(errs.ValidInvalid,
			"A GitHub App connection needs either the installation ID or the owner the App is installed on.").
			WithRemedy("Enter the organization or user the App is installed on as the owner, or enter the installation ID.")
	}
	key, err := parseAppKey(cfg.Credential(FieldAppPrivateKey))
	if err != nil {
		return nil, err
	}
	return key, nil
}

func parseAppKey(v secret.Value) (*rsa.PrivateKey, error) {
	if v.IsZero() {
		return nil, errs.New(errs.ValidInvalid, "A GitHub App connection needs the App's private key.").
			WithRemedy("Generate a private key on the App's settings page in GitHub and paste the whole .pem file here.")
	}
	bad := errs.New(errs.ValidInvalid, "The GitHub App private key could not be read. It may be incomplete.").
		WithRemedy("Paste the whole .pem file GitHub downloaded, including its BEGIN and END lines.")
	block, _ := pem.Decode([]byte(strings.TrimSpace(v.Reveal())))
	if block == nil {
		return nil, bad
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, bad
	}
	k, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errs.New(errs.ValidInvalid, "The GitHub App private key is not an RSA key, and GitHub Apps sign with RSA.").
			WithRemedy("Paste the .pem file GitHub generated for the App.")
	}
	return k, nil
}

func digits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// appJWT is the App's own short-lived token, signed with its private key:
// issued a minute in the past for clock drift, good for nine minutes (GitHub
// allows ten).
func (a *Adapter) appJWT() (string, error) {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: a.appKey},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return "", errs.Wrap(errs.Internal, "Pando could not sign a token with the GitHub App's private key.", err)
	}
	now := a.now()
	tok, err := jwt.Signed(signer).Claims(jwt.Claims{
		Issuer:   a.cfg.AppID,
		IssuedAt: jwt.NewNumericDate(now.Add(-60 * time.Second)),
		Expiry:   jwt.NewNumericDate(now.Add(9 * time.Minute)),
	}).Serialize()
	if err != nil {
		return "", errs.Wrap(errs.Internal, "Pando could not sign a token with the GitHub App's private key.", err)
	}
	return tok, nil
}

// installationToken mints an installation access token. Minted on every call,
// never cached (R-027); one lasts an hour, longer than any clone.
func (a *Adapter) installationToken(ctx context.Context) (secret.Value, time.Time, error) {
	appTok, err := a.appJWT()
	if err != nil {
		return secret.Value{}, time.Time{}, err
	}
	c, status := a.appClient(secret.New(appTok))

	id := a.cfg.InstallationID
	if id == "" {
		id, err = a.lookupInstallation(ctx, c, status)
		if err != nil {
			return secret.Value{}, time.Time{}, err
		}
	}

	var resp struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if _, err := c.Do(ctx, http.MethodPost, a.apiURL+"/app/installations/"+url.PathEscape(id)+"/access_tokens", nil, &resp); err != nil {
		if errs.CodeOf(err) == errs.NotFound {
			return secret.Value{}, time.Time{}, errs.Newf(errs.NotFound,
				"GitHub has no installation %s of the App %s.", id, a.cfg.AppID).
				WithRemedy("Check the installation ID in the connection's settings, or leave it empty to look it up from the owner.")
		}
		return secret.Value{}, time.Time{}, appErr(err, *status)
	}
	if resp.Token == "" {
		return secret.Value{}, time.Time{}, errs.New(errs.AdapterFailed,
			"GitHub answered without an installation token for the App.")
	}
	return secret.New(resp.Token), resp.ExpiresAt.UTC(), nil
}

// lookupInstallation finds the App's installation on the connection's owner,
// as an organization and then as a user.
func (a *Adapter) lookupInstallation(ctx context.Context, c forgekit.Client, status *int) (string, error) {
	owner := url.PathEscape(a.cfg.Scope)
	var inst struct {
		ID int64 `json:"id"`
	}
	_, err := c.Get(ctx, a.apiURL+"/orgs/"+owner+"/installation", &inst)
	if errs.CodeOf(err) == errs.NotFound {
		_, err = c.Get(ctx, a.apiURL+"/users/"+owner+"/installation", &inst)
	}
	if errs.CodeOf(err) == errs.NotFound {
		return "", errs.Newf(errs.NotFound,
			"The GitHub App %s is not installed on %s.", a.cfg.AppID, a.cfg.Scope).
			WithRemedy("Install the App on the organization or user from the App's public page in GitHub, or check the owner in the connection's settings.")
	}
	if err != nil {
		return "", appErr(err, *status)
	}
	if inst.ID == 0 {
		return "", errs.New(errs.AdapterFailed, "GitHub answered without an installation ID for the App.")
	}
	return strconv.FormatInt(inst.ID, 10), nil
}

// appErr names the App when GitHub refuses its own signed token, which means
// the ID or the key is wrong rather than a token having expired. status is the
// HTTP status of the call that failed.
func appErr(err error, status int) error {
	if status == http.StatusUnauthorized {
		// AdapterFailed, not an AUTH_* code: GitHub refusing the App is not
		// the caller failing to authenticate to Pando.
		return errs.New(errs.AdapterFailed,
			"GitHub did not accept the GitHub App's signed token. The App ID or the private key is wrong, or the key was revoked.").
			WithRemedy("Check the App ID and paste a current private key from the App's settings page in GitHub.")
	}
	return err
}

// appClient is the API client for calls signed as the App, recording the
// status of the last answer so a refusal of the App itself can be named.
// Local to one call, so the adapter keeps nothing between calls (R-027).
func (a *Adapter) appClient(token secret.Value) (forgekit.Client, *int) {
	c := a.client(token)
	status := new(int)
	base := a.http.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c.HTTP = &http.Client{Transport: statusTransport{base: base, status: status}, Timeout: a.http.Timeout}
	return c, status
}

type statusTransport struct {
	base   http.RoundTripper
	status *int
}

func (t statusTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if resp != nil {
		*t.status = resp.StatusCode
	}
	return resp, err
}
