package httpapi

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/idp"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/log"
)

// External identity (issue #51): the provider pages an administrator uses,
// redirect sign-in through a provider, and linking. Business logic is in
// core/idp; what is here is reading requests and writing cookies and
// redirects, which only an HTTP handler can do.

// SSOBindCookie binds a redirect sign-in to the browser that started it
// (design 06 §3.2). In Pando's namespace, so the proxy strips it from every
// request an app receives (R-173).
const SSOBindCookie = "pando_sso_bind"

// origin is where a request came from, as a browser would be sent back to it.
// The scheme is the operator's, as for the session cookie (O-19).
func (s *Server) origin(r *http.Request) idp.Origin {
	scheme := "http"
	if s.secureCookie(r) {
		scheme = "https"
	}
	return idp.Origin{Scheme: scheme, Host: r.Host}
}

// servesHost says whether a sign-in may finish on this hostname: Pando's own,
// or one an app is reached at. With no external URL configured, any — which
// is the laptop install, where there is only one.
func (s *Server) servesHost(r *http.Request) bool {
	if s.ExternalURL == nil || s.ExternalURL.Host == "" {
		return true
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if strings.EqualFold(r.Host, s.ExternalURL.Host) || strings.EqualFold(host, s.ExternalURL.Hostname()) {
		return true
	}
	if s.AppHosts != nil {
		if ok, err := s.AppHosts.IsAppHostname(r.Context(), host); err == nil && ok {
			return true
		}
	}
	return false
}

func (s *Server) requireIDP(w http.ResponseWriter, r *http.Request) bool {
	if s.IDP == nil {
		Error(w, r, errs.New(errs.Internal, "External identity is not set up on this installation."))
		return false
	}
	return true
}

func (s *Server) setBindCookie(w http.ResponseWriter, r *http.Request, value string) {
	//nolint:gosec // G124: Secure is decided by secureCookie, not left unset.
	http.SetCookie(w, &http.Cookie{
		Name: SSOBindCookie, Value: value, Path: "/", HttpOnly: true,
		Secure: s.secureCookie(r), SameSite: http.SameSiteLaxMode,
		MaxAge: int(idp.FlowLifetime / time.Second),
	})
}

// --- public: signing in --------------------------------------------------------

// handleSignInOptions tells the sign-in page what to offer. Public.
func (s *Server) handleSignInOptions(w http.ResponseWriter, r *http.Request) {
	if s.IDP == nil {
		JSON(w, http.StatusOK, idp.SignInOptions{PasswordSignIn: true, Providers: []idp.PublicProvider{}})
		return
	}
	opts, err := s.IDP.Options(r.Context())
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, opts)
}

// handleSSOStart sends the browser to a provider. A browser navigation, so
// every failure is a redirect to the sign-in page that explains it rather
// than a JSON body nobody will read.
func (s *Server) handleSSOStart(w http.ResponseWriter, r *http.Request) {
	if !s.requireIDP(w, r) {
		return
	}
	next := r.URL.Query().Get("next")
	if !s.servesHost(r) {
		Error(w, r, errs.New(errs.ValidInvalid, "Pando does not serve this hostname, so it cannot sign you in here."))
		return
	}
	started, err := s.IDP.Start(r.Context(), idp.StartRequest{
		ProviderID: chi.URLParam(r, "providerID"), Origin: s.origin(r), Next: next,
	})
	if err != nil {
		log.From(r.Context()).Warn("sign-in could not start", zap.Error(err))
		http.Redirect(w, r, idp.LoginPath+"?sso_error=unavailable&next="+url.QueryEscape(idp.SafeNext(next)), http.StatusFound)
		return
	}
	s.setBindCookie(w, r, started.Bind)
	http.Redirect(w, r, started.RedirectURL, http.StatusFound)
}

// handleSSOCallback receives the provider's response: a redirect for OIDC, a
// form post for SAML. Either way the answer is a redirect.
func (s *Server) handleSSOCallback(w http.ResponseWriter, r *http.Request) {
	if !s.requireIDP(w, r) {
		return
	}
	params := r.URL.Query()
	if r.Method == http.MethodPost {
		// A SAML response is at most a few tens of kilobytes; a body larger
		// than a megabyte is not one.
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if err := r.ParseForm(); err != nil {
			Error(w, r, errs.New(errs.ValidInvalid, "The identity provider's response could not be read."))
			return
		}
		params = r.PostForm
	}
	target := s.IDP.Callback(r.Context(), chi.URLParam(r, "providerID"), params, s.origin(r))
	// 303 so a POST callback becomes a GET.
	//
	// Not an open redirect: the target is built by core from the origin the
	// flow started on — a hostname Pando serves, checked at the start (servesHost)
	// — or from the external URL, and a path of Pando's own. Nothing the
	// provider sent chooses where the browser goes.
	//nolint:gosec // G710: see above.
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// handleSSOComplete finishes a sign-in in the browser that started it: the
// session cookie is set on this hostname, and the browser goes where it was
// going.
func (s *Server) handleSSOComplete(w http.ResponseWriter, r *http.Request) {
	if !s.requireIDP(w, r) {
		return
	}
	bind := ""
	if c, err := r.Cookie(SSOBindCookie); err == nil {
		bind = c.Value
	}
	done, err := s.IDP.Complete(r.Context(), r.URL.Query().Get("code"), bind, s.origin(r), r.UserAgent(), clientIP(r))
	// The binding is spent either way.
	//nolint:gosec // G124: Secure is decided by secureCookie, not left unset.
	http.SetCookie(w, &http.Cookie{Name: SSOBindCookie, Value: "", Path: "/", HttpOnly: true,
		Secure: s.secureCookie(r), SameSite: http.SameSiteLaxMode, MaxAge: -1})
	if err != nil {
		reason := "expired"
		if e := errs.As(err); e != nil && strings.Contains(e.Message, "different browser") {
			reason = "browser"
		}
		http.Redirect(w, r, idp.LoginPath+"?sso_error="+reason, http.StatusFound)
		return
	}
	//nolint:gosec // G124: CookieScheme.sessionCookie sets Secure, HttpOnly and SameSite.
	cookie := s.cookies().sessionCookie(r, done.Session.Cookie.Reveal())
	cookie.Expires = done.Session.Session.ExpiresAt
	http.SetCookie(w, cookie)
	http.Redirect(w, r, done.Next, http.StatusFound)
}

// handleSSOFailure says why a sign-in failed, for the sign-in page. Public:
// the flow ID is 256 random bits the failing browser was just sent, and the
// answer is a message about that browser's own attempt.
func (s *Server) handleSSOFailure(w http.ResponseWriter, r *http.Request) {
	if !s.requireIDP(w, r) {
		return
	}
	problem, ok, err := s.IDP.FailureMessage(r.Context(), chi.URLParam(r, "flowID"))
	if err != nil {
		Error(w, r, err)
		return
	}
	if !ok {
		Error(w, r, errs.New(errs.NotFound, "There is no failed sign-in with that ID."))
		return
	}
	JSON(w, http.StatusOK, problem)
}

// handleSAMLMetadata publishes Pando's SAML metadata for a provider. Public:
// the provider fetches it without credentials.
func (s *Server) handleSAMLMetadata(w http.ResponseWriter, r *http.Request) {
	if !s.requireIDP(w, r) {
		return
	}
	md, err := s.IDP.ServiceMetadata(r.Context(), s.origin(r), chi.URLParam(r, "providerID"))
	if err != nil {
		Error(w, r, err)
		return
	}
	w.Header().Set("Content-Type", md.ContentType)
	w.Header().Set("Content-Disposition", `inline; filename="pando-saml-metadata.xml"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	// XML Pando generated from its own endpoints, served as SAML metadata
	// with sniffing off; nothing in it came from the request.
	//nolint:gosec // G705: see above.
	_, _ = w.Write(md.Body)
}

// --- administration: providers ---------------------------------------------------

func (s *Server) handleListIdentityProviders(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireInstall(w, r, authz.InstallView); !ok {
		return
	}
	if !s.requireIDP(w, r) {
		return
	}
	providers, err := s.IDP.ListProviders(r.Context(), s.origin(r))
	if err != nil {
		Error(w, r, err)
		return
	}
	// A new provider's addresses, with {id} for the ID it is created with:
	// registered with the provider while it is set up, before it is saved.
	next := s.IDP.Endpoints(s.origin(r), "{id}")
	JSON(w, http.StatusOK, map[string]any{"providers": providers, "kinds": s.IDP.KindInfos(),
		"endpoints": map[string]string{"callback_url": next.CallbackURL, "entity_id": next.EntityID,
			"login_url": s.IDP.LoginURL(s.origin(r), "{id}")}})
}

func (s *Server) handleGetIdentityProvider(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireInstall(w, r, authz.InstallView); !ok {
		return
	}
	if !s.requireIDP(w, r) {
		return
	}
	p, err := s.IDP.Provider(r.Context(), s.origin(r), chi.URLParam(r, "providerID"))
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, p)
}

// vouches is why configuring an identity provider needs every installation
// permission (R-080a).
const vouches = "an identity provider decides who can sign in as which account, administrators included"

func (s *Server) handleCreateIdentityProvider(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallAdaptersManage)
	if !ok || !s.requireIDP(w, r) {
		return
	}
	// Whoever configures a provider decides who it vouches for, and so who can
	// sign in as which account (R-080a).
	if err := s.Authz.CheckHoldsEveryInstallVerb(r.Context(), p, "add an identity provider", vouches); err != nil {
		Error(w, r, err)
		return
	}
	var in idp.ProviderInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read."))
		return
	}
	created, err := s.IDP.CreateProvider(r.Context(), p, in, s.origin(r))
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusCreated, created)
}

func (s *Server) handlePatchIdentityProvider(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallAdaptersManage)
	if !ok || !s.requireIDP(w, r) {
		return
	}
	// Whoever configures a provider decides who it vouches for, and so who can
	// sign in as which account (R-080a).
	if err := s.Authz.CheckHoldsEveryInstallVerb(r.Context(), p, "change an identity provider", vouches); err != nil {
		Error(w, r, err)
		return
	}
	var in idp.ProviderInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read."))
		return
	}
	updated, err := s.IDP.UpdateProvider(r.Context(), p, chi.URLParam(r, "providerID"), in, s.origin(r))
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteIdentityProvider(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallAdaptersManage)
	if !ok || !s.requireIDP(w, r) {
		return
	}
	if err := s.IDP.DeleteProvider(r.Context(), p, chi.URLParam(r, "providerID")); err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusNoContent, nil)
}

// handleCheckIdentityProvider asks the provider whether it answers.
func (s *Server) handleCheckIdentityProvider(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireInstall(w, r, authz.InstallAdaptersManage); !ok || !s.requireIDP(w, r) {
		return
	}
	if err := s.IDP.CheckProvider(r.Context(), chi.URLParam(r, "providerID")); err != nil {
		e := errs.As(err)
		if e == nil || e.Code == errs.NotFound {
			Error(w, r, err)
			return
		}
		JSON(w, http.StatusOK, map[string]any{"ok": false, "message": e.Message, "remedy": e.Remedy})
		return
	}
	JSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleRotateSCIMToken turns SCIM on, or replaces its token. The token is in
// this response and nowhere else, ever.
func (s *Server) handleRotateSCIMToken(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallAdaptersManage)
	if !ok || !s.requireIDP(w, r) {
		return
	}
	// A SCIM token creates accounts and puts them in groups, including ones
	// linked to groups holding installation roles (R-080a).
	if err := s.Authz.CheckHoldsEveryInstallVerb(r.Context(), p, "issue a SCIM token for an identity provider",
		"a SCIM token creates accounts and puts them in groups, including groups that hold installation roles"); err != nil {
		Error(w, r, err)
		return
	}
	token, err := s.IDP.RotateSCIMToken(r.Context(), p, chi.URLParam(r, "providerID"))
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, map[string]any{
		"token":         token.Reveal(),
		"scim_base_url": s.IDP.SCIMBaseURL(s.origin(r)),
	})
}

func (s *Server) handleDisableSCIM(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallAdaptersManage)
	if !ok || !s.requireIDP(w, r) {
		return
	}
	if err := s.IDP.DisableSCIM(r.Context(), p, chi.URLParam(r, "providerID")); err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusNoContent, nil)
}

// handleTestIdentityProvider starts a test sign-in: the administrator signs
// in at the provider, and comes back to a report of what it sent and what
// Pando would do with it. Nobody is signed in, and it works on a provider that
// is still off — which is when it is needed.
func (s *Server) handleTestIdentityProvider(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallAdaptersManage)
	if !ok || !s.requireIDP(w, r) {
		return
	}
	started, err := s.IDP.Start(r.Context(), idp.StartRequest{
		ProviderID: chi.URLParam(r, "providerID"), Origin: s.origin(r), Test: true, InitiatedBy: p,
	})
	if err != nil {
		Error(w, r, err)
		return
	}
	s.setBindCookie(w, r, started.Bind)
	http.Redirect(w, r, started.RedirectURL, http.StatusFound)
}

func (s *Server) handleIdentityProviderTest(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallAdaptersManage)
	if !ok || !s.requireIDP(w, r) {
		return
	}
	report, err := s.IDP.TestResult(r.Context(), p, chi.URLParam(r, "providerID"), chi.URLParam(r, "flowID"))
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, report)
}

// --- administration: linking ------------------------------------------------------

func (s *Server) handleListIdentities(w http.ResponseWriter, r *http.Request) {
	userID := chi.URLParam(r, "userID")
	if _, ok := s.requireSelfOrInstall(w, r, userID, authz.InstallView); !ok || !s.requireIDP(w, r) {
		return
	}
	ids, err := s.IDP.IdentitiesFor(r.Context(), userID)
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, map[string]any{"identities": ids})
}

func (s *Server) handleLinkIdentity(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallUsersManage)
	if !ok || !s.requireIDP(w, r) {
		return
	}
	var req struct {
		AdapterID      string `json:"adapter_id"`
		ExternalID     string `json:"external_id"`
		ReplaceAccount bool   `json:"replace_account"`
		Confirm        bool   `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read."))
		return
	}
	userID := chi.URLParam(r, "userID")
	// An identity that signs in to the account is a way into it, and moving
	// one from another account changes that one too (R-080a).
	if err := s.Authz.CheckMayChangeAccount(r.Context(), p, userID, "link an identity to this account"); err != nil {
		Error(w, r, err)
		return
	}
	if holder, found, err := s.IDP.Identities.Resolve(r.Context(), req.AdapterID, req.ExternalID); err != nil {
		Error(w, r, err)
		return
	} else if found && holder.ID != userID {
		if err := s.Authz.CheckMayChangeAccount(r.Context(), p, holder.ID,
			"move an identity from the account that has it"); err != nil {
			Error(w, r, err)
			return
		}
	}
	aliased, err := s.IDP.LinkIdentity(r.Context(), p, userID, req.AdapterID, req.ExternalID, req.ReplaceAccount, req.Confirm)
	if err != nil {
		Error(w, r, err)
		return
	}
	body := map[string]any{"user_id": userID, "adapter_id": req.AdapterID, "external_id": req.ExternalID}
	if aliased != "" {
		body["aliased_user_id"] = aliased
	}
	JSON(w, http.StatusCreated, body)
}

func (s *Server) handleUnlinkIdentity(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallUsersManage)
	if !ok || !s.requireIDP(w, r) {
		return
	}
	q := r.URL.Query()
	if err := s.Authz.CheckMayChangeAccount(r.Context(), p, chi.URLParam(r, "userID"),
		"unlink an identity from this account"); err != nil {
		Error(w, r, err)
		return
	}
	if err := s.IDP.UnlinkIdentity(r.Context(), p, chi.URLParam(r, "userID"), q.Get("adapter_id"), q.Get("external_id")); err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusNoContent, nil)
}

func (s *Server) handleLinkGroup(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallUsersManage)
	if !ok || !s.requireIDP(w, r) {
		return
	}
	// Everyone in the provider's group would hold what the Pando group does.
	if err := s.confersGroup(r.Context(), p, chi.URLParam(r, "groupID"),
		"link a provider's group to a group holding the role %s"); err != nil {
		Error(w, r, err)
		return
	}
	if err := s.IDP.LinkGroup(r.Context(), p, chi.URLParam(r, "groupID"), chi.URLParam(r, "syncedGroupID")); err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusNoContent, nil)
}

func (s *Server) handleUnlinkGroup(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallUsersManage)
	if !ok || !s.requireIDP(w, r) {
		return
	}
	if err := s.confersGroup(r.Context(), p, chi.URLParam(r, "groupID"),
		"unlink a provider's group from a group holding the role %s"); err != nil {
		Error(w, r, err)
		return
	}
	if err := s.IDP.UnlinkGroup(r.Context(), p, chi.URLParam(r, "groupID"), chi.URLParam(r, "syncedGroupID")); err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusNoContent, nil)
}

// AuditFunc is how a core service that audits on its own records an event
// with the request ID the HTTP layer assigned, and logs rather than fails
// when the write does not land — the same rule as Server.audit.
func AuditFunc(w interface {
	Write(ctx context.Context, e audit.Event) error
}) func(ctx context.Context, e audit.Event) {
	return func(ctx context.Context, e audit.Event) {
		if e.RequestID == "" {
			e.RequestID = RequestIDFrom(ctx)
		}
		if err := w.Write(ctx, e); err != nil {
			log.From(ctx).Error("audit write failed", zap.String("action", e.Action), zap.Error(err))
		}
	}
}
