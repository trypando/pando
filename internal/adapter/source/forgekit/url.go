// Package forgekit is what source adapters share: reading a repository
// address in each of the forms people write one, deciding whether a connection
// covers it, and building the credential core clones with.
//
// Not an adapter. Each forge's own package decides what its addresses look
// like and what it calls the parts; this package only does the parsing and the
// arithmetic they would otherwise each repeat.
package forgekit

import (
	"net"
	"net/url"
	"strings"

	"github.com/trypando/pando/internal/errs"
)

// RepoURL is a repository address, read.
type RepoURL struct {
	// SSH says the address is an SSH one: ssh://host/path or git@host:path.
	SSH bool

	// Host is the host name, lowercased, without a port.
	Host string

	// Port is the port, if one was written.
	Port string

	// User is the user in an SSH address, usually "git".
	User string

	// Path is the repository's path on the host with no leading slash and no
	// trailing ".git": "acme/api", "acme/platform/api",
	// "v3/acme/Payments/api" for an Azure DevOps SSH address.
	Path string
}

// Segments is Path split on "/".
func (u RepoURL) Segments() []string {
	if u.Path == "" {
		return nil
	}
	return strings.Split(u.Path, "/")
}

// ParseRepoURL reads https://host/owner/repo(.git), ssh://user@host[:port]/owner/repo(.git)
// and the scp-like user@host:owner/repo(.git).
func ParseRepoURL(raw string) (RepoURL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return RepoURL{}, errs.New(errs.ValidInvalid, "The repository address is empty.")
	}

	// scp-like: git@github.com:acme/api.git. No scheme, a colon before the
	// first slash.
	if !strings.Contains(raw, "://") {
		at := strings.Index(raw, "@")
		colon := strings.Index(raw, ":")
		slash := strings.Index(raw, "/")
		if colon > 0 && (slash < 0 || colon < slash) {
			user := ""
			host := raw[:colon]
			if at >= 0 && at < colon {
				user, host = raw[:at], raw[at+1:colon]
			}
			return RepoURL{
				SSH:  true,
				Host: strings.ToLower(host),
				User: user,
				Path: cleanPath(raw[colon+1:]),
			}, nil
		}
		return RepoURL{}, errs.Newf(errs.ValidInvalid,
			"%q is not a repository address Pando can read. Use the HTTPS address, such as https://github.com/acme/api, or the SSH one, such as git@github.com:acme/api.git.", raw)
	}

	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return RepoURL{}, errs.Newf(errs.ValidInvalid,
			"%q is not a repository address Pando can read. Use the HTTPS address, such as https://github.com/acme/api, or the SSH one, such as git@github.com:acme/api.git.", raw)
	}
	out := RepoURL{
		Host: strings.ToLower(u.Hostname()),
		Port: u.Port(),
		Path: cleanPath(u.Path),
	}
	switch strings.ToLower(u.Scheme) {
	case "ssh", "git+ssh", "ssh+git":
		out.SSH = true
		out.User = u.User.Username()
	case "http", "https":
	default:
		return RepoURL{}, errs.Newf(errs.ValidInvalid,
			"Pando clones over HTTPS or SSH, and %q uses %s.", raw, u.Scheme)
	}
	return out, nil
}

func cleanPath(p string) string {
	p = strings.Trim(p, "/")
	p = strings.TrimSuffix(p, ".git")
	return strings.Trim(p, "/")
}

// HostOf reads the host out of a host setting, which people write as
// "github.com", "https://gitlab.acme.internal" or "gitlab.acme.internal:8443".
// The port is dropped: a connection is for a host, whichever port it is
// reached on.
func HostOf(setting string) string {
	s := strings.TrimSpace(setting)
	if s == "" {
		return ""
	}
	if strings.Contains(s, "://") {
		if u, err := url.Parse(s); err == nil {
			return strings.ToLower(u.Hostname())
		}
	}
	s = strings.SplitN(s, "/", 2)[0]
	if h, _, err := net.SplitHostPort(s); err == nil {
		return strings.ToLower(h)
	}
	return strings.ToLower(s)
}

// ScopeSegments splits a scope setting — "acme", "acme/platform",
// "acme/Payments" — into its parts, ignoring slashes at either end.
func ScopeSegments(scope string) []string {
	scope = strings.Trim(strings.TrimSpace(scope), "/")
	if scope == "" {
		return nil
	}
	return strings.Split(scope, "/")
}

// Covers scores how specifically a connection for host, limited to scope,
// covers a repository path. Zero is not at all. One is the whole host; each
// scope segment that matches adds one, so the narrowest connection wins
// (api.SourceAdapter.Covers). Names are compared without case, as every forge
// Pando knows treats owner names.
//
// hosts lists every host the connection answers for — "dev.azure.com" and
// "ssh.dev.azure.com" are one Azure DevOps — and path is the repository path
// with any host-specific prefix already removed by the caller.
func Covers(hosts []string, scope []string, repoHost string, path []string) int {
	matched := false
	for _, h := range hosts {
		if h != "" && strings.EqualFold(h, repoHost) {
			matched = true
			break
		}
	}
	if !matched {
		return 0
	}
	if len(scope) > 0 && len(path) <= len(scope) {
		// The repository is the scope itself or above it — "acme" is not
		// a repository inside "acme".
		return 0
	}
	for i, s := range scope {
		if !strings.EqualFold(s, path[i]) {
			return 0
		}
	}
	return 1 + len(scope)
}

// HTTPSURL is the HTTPS address of a repository on host at path.
func HTTPSURL(host, port, path string) string {
	h := host
	if port != "" && port != "443" {
		h = net.JoinHostPort(host, port)
	}
	return "https://" + h + "/" + path + ".git"
}

// SCPURL is the scp-like SSH address of a repository on host at path, for
// the default port. With a port it is the ssh:// form, which is the only one
// that can carry one.
func SCPURL(user, host, port, path string) string {
	if user == "" {
		user = "git"
	}
	if port != "" && port != "22" {
		return "ssh://" + user + "@" + net.JoinHostPort(host, port) + "/" + path + ".git"
	}
	return user + "@" + host + ":" + path + ".git"
}
