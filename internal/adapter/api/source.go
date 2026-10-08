package api

import (
	"context"
	"time"

	"github.com/trypando/pando/internal/secret"
)

// The source category (R-091, design 03 §10).
//
// The eleventh category. A source adapter is one connection from this install
// to a place git repositories live — GitHub, GitLab, Azure DevOps, Bitbucket,
// Gitea or Forgejo, or any git host reached with a token or an SSH key. It
// answers three questions: is this repository mine (Covers), what does Pando
// clone it with (GitCredential), and what repositories can be picked from
// (ListRepositories). Core asks them; the adapter knows the forge's API and
// URL shapes, so core never parses "dev.azure.com/{org}/{project}/_git/{repo}"
// itself (R-251).
//
// The connection belongs to the install (O-3, re-resolved with issue #127):
// an administrator connects it once, and any app whose repository it covers is
// cloned with it. The adapter is stateless like every other (R-027). Its
// credentials arrive in Configure, sealed at rest by core (R-190), and anything
// it learns that must be kept — an OAuth token, a refreshed one — goes back to
// core in a result for core to store.
//
// Pando clones; the builder never sees a credential (R-112). A GitCredential is
// used by core's git client in Pando's own process and goes nowhere else.

// SourceAdapter is one connection to a source host.
type SourceAdapter interface {
	Adapter

	SourceCapabilities() SourceCapabilities

	// Covers reports whether this connection is for the repository at repoURL,
	// and how specifically. Zero means it is not. A connection limited to an
	// owner, group, workspace or project scores higher than one for the whole
	// host, and core uses the highest scoring connection that covers a URL —
	// so "acme on github.com with an App" wins over "github.com with a token"
	// for acme's repositories and only for them.
	//
	// Pure: no network, no clock. It is called for every connection on every
	// app create and fetch.
	Covers(repoURL string) int

	// GitCredential is what to clone repoURL with. Minted here when the
	// provider issues short-lived tokens (a GitHub App installation token,
	// an Entra ID token for a service principal), refreshed here when an
	// OAuth token has expired.
	GitCredential(ctx context.Context, repoURL string) (GitCredential, error)

	// Refresh renews an OAuth access token that has expired or is about to,
	// and returns the credentials to store in place of the old ones, keyed
	// like the adapter's credential fields. Nil when nothing changed — every
	// method but oauth, and an oauth token still good. Core calls it before
	// ListRepositories and ListBranches and configures the connection again
	// with what it stored, because a provider that rotates refresh tokens
	// (GitLab) invalidates the old one the moment it is used: a listing that
	// refreshed on its own would lose the new one.
	Refresh(ctx context.Context) (map[string]secret.Value, error)

	// ListRepositories lists repositories the connection can read, for a
	// person to pick from rather than type. Only when
	// SourceCapabilities.ListRepositories; otherwise it returns an error saying
	// to enter the address instead.
	ListRepositories(ctx context.Context, req ListRepositoriesRequest) ([]Repository, error)

	// ListBranches lists a repository's branches, under the same capability.
	ListBranches(ctx context.Context, repoURL string) ([]string, error)

	// BeginAuthorization starts an OAuth authorization, for a connection
	// whose method is oauth. Device mode needs no address Pando can be
	// reached at; web mode returns a URL to send the person to, which comes
	// back to req.RedirectURL.
	BeginAuthorization(ctx context.Context, req AuthorizationRequest) (Authorization, error)

	// CompleteAuthorization finishes one: polls the device code, or
	// exchanges the web code. A device authorization the person has not
	// approved yet returns a result with Pending set and no error.
	CompleteAuthorization(ctx context.Context, req AuthorizationCompletion) (AuthorizationResult, error)
}

// SourceCapabilities is what a connection can do, as data (R-254).
type SourceCapabilities struct {
	// Method is how the connection authenticates: "token", "ssh", "app",
	// "oauth" or "service_principal". Shown, never branched on in core.
	Method string `json:"method"`

	// Host is the host the connection is for, such as "github.com" or
	// "gitlab.acme.internal". Shown beside the connection.
	Host string `json:"host"`

	// Scope is what on the host it is limited to — an owner, group,
	// workspace, organization or project — or empty for the whole host.
	Scope string `json:"scope,omitempty"`

	// ListRepositories says repositories can be picked from a list. A deploy
	// key or a bare git credential has no API access, and the address is
	// typed instead.
	ListRepositories bool `json:"list_repositories"`

	// DeviceAuthorization and WebAuthorization say which OAuth flows the
	// connection can run. Both false for every method but oauth.
	DeviceAuthorization bool `json:"device_authorization"`
	WebAuthorization    bool `json:"web_authorization"`

	// Authorized says the connection holds what it needs to clone: a token,
	// a key, or a completed OAuth authorization. An oauth connection that
	// has not been authorized yet is false, and covers nothing.
	Authorized bool `json:"authorized"`
}

// GitCredential is what core's git client clones with. Exactly one of
// Password, BearerToken or SSHPrivateKey is set.
type GitCredential struct {
	// URL is the address to clone instead of the one the app names, when
	// the connection reaches it another way — an SSH key for a repository
	// entered as https. Empty means the app's own address. The spec keeps
	// the app's address either way.
	URL string

	// Username and Password are HTTPS basic authentication. A token goes in
	// Password; Username is whatever the host expects beside it
	// ("x-access-token", "oauth2", "x-token-auth", or the account name).
	Username string
	Password secret.Value

	// BearerToken is HTTPS bearer authentication, for a host that takes an
	// OAuth access token as "Authorization: Bearer" rather than as a
	// password — Azure DevOps with a Microsoft Entra ID token.
	BearerToken secret.Value

	// SSHUser, SSHPrivateKey and SSHPassphrase are SSH authentication.
	// SSHUser is "git" unless the host says otherwise.
	SSHUser       string
	SSHPrivateKey secret.Value
	SSHPassphrase secret.Value

	// KnownHosts pins the host's SSH key, in known_hosts format. Required
	// with an SSH key: a connection that would accept any host key is one a
	// network attacker can read the repository through.
	KnownHosts string

	// CABundle is PEM certificates trusted beside the system's for HTTPS,
	// for a self-managed host on a private certificate authority.
	CABundle []byte

	// ExpiresAt is when the credential stops working, when the provider
	// says. Zero when it does not expire or nobody knows.
	ExpiresAt time.Time

	// Rotated is credentials the adapter refreshed while producing this one
	// — an OAuth access token and its new refresh token — keyed like the
	// adapter's credential fields. Core stores them in place of the old ones
	// (R-190) before using the credential. Nil when nothing changed.
	Rotated map[string]secret.Value
}

// ListRepositoriesRequest narrows a repository listing.
type ListRepositoriesRequest struct {
	// Query matches part of a repository's full name. Empty lists all.
	Query string

	// Limit caps the result. Zero means the adapter's own default, 100.
	Limit int
}

// Repository is one repository a connection can read.
type Repository struct {
	// URL is the HTTPS address to clone, as a person would enter it.
	URL string `json:"url"`

	// FullName is the name the host shows: "acme/api",
	// "acme/platform/api", "acme/Payments/api".
	FullName string `json:"full_name"`

	DefaultBranch string `json:"default_branch,omitempty"`
	Private       bool   `json:"private"`
}

// AuthorizationMode is which OAuth flow runs.
type AuthorizationMode string

const (
	// AuthorizationDevice shows the person a code to enter on the
	// provider's site. Pando needs no address the provider can reach, so it
	// works from a laptop, the CLI and an assistant over MCP.
	AuthorizationDevice AuthorizationMode = "device"

	// AuthorizationWeb sends the person's browser to the provider and back
	// to Pando's callback.
	AuthorizationWeb AuthorizationMode = "web"
)

// AuthorizationRequest starts an OAuth authorization.
type AuthorizationRequest struct {
	Mode AuthorizationMode

	// RedirectURL is Pando's callback, for web mode.
	RedirectURL string

	// State is what the provider returns to the callback, for web mode.
	// Core makes it and checks it; the adapter only passes it along.
	State string
}

// Authorization is an OAuth authorization in progress.
type Authorization struct {
	Mode AuthorizationMode

	// UserCode and VerificationURL are what the person is shown in device
	// mode: "Go to VerificationURL and enter UserCode."
	UserCode        string
	VerificationURL string

	// AuthorizeURL is where to send the person's browser in web mode.
	AuthorizeURL string

	// Interval is how often to poll in device mode.
	Interval time.Duration

	// ExpiresAt is when the code stops working.
	ExpiresAt time.Time

	// Flow is what CompleteAuthorization needs back — the device code, the
	// PKCE verifier. Core keeps it and returns it; the adapter remembers
	// nothing between the two calls (R-027).
	Flow secret.Value
}

// AuthorizationCompletion finishes an OAuth authorization.
type AuthorizationCompletion struct {
	Mode        AuthorizationMode
	Flow        secret.Value
	Code        string
	RedirectURL string
}

// AuthorizationResult is the outcome of CompleteAuthorization.
type AuthorizationResult struct {
	// Pending says the person has not approved a device authorization yet.
	// SlowDown says the provider asked for polling to slow down.
	Pending  bool
	SlowDown bool

	// Credentials are what to store, keyed like the adapter's credential
	// fields: an access token and, where the provider issues one, a refresh
	// token.
	Credentials map[string]secret.Value

	// ExpiresAt is when the access token expires, if it does.
	ExpiresAt time.Time
}
