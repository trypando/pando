package spec

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/trypando/pando/internal/errs"
)

// scpLike is git's shorthand for an SSH address, git@github.com:team/app.git:
// an optional user, a host, a colon, and a path that does not start with a
// slash-slash. The host has no slash in it, which is what tells it from a
// local path with a colon later on.
var scpLike = regexp.MustCompile(`^(?:[^@/\s]+@)?[^@/:\s]+:[^/\s].*$`)

// CheckSourceURL refuses a repository address Pando will not fetch, said when
// a person gives it rather than when detection fails (R-092; 2026-10-16
// assessment, finding 3).
//
// https, http and ssh are fetched, including git's git@host:path shorthand.
// Anything else — file://, a bare path on the machine Pando runs on, git:// —
// is refused, and the fetch refuses it too (source/transport.go). An address
// carrying a password or token is refused as well: the spec is append-only
// and exported (R-152), so a credential in it can never be taken back out,
// and Pando keeps credentials in source connections, encrypted (R-190).
//
// Where an address resolves to is not checked here: that depends on DNS when
// it is fetched, and the fetch checks every address it dials.
func CheckSourceURL(src Source) error {
	if src.Type != SourceGit || strings.TrimSpace(src.URL) == "" {
		return nil
	}
	raw := strings.TrimSpace(src.URL)

	if !strings.Contains(raw, "://") {
		if scpLike.MatchString(raw) && !windowsPath(raw) {
			return nil
		}
		return unfetchable(raw)
	}

	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return unfetchable(raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "https", "http":
		if u.User != nil {
			return credentialInURL()
		}
	case "ssh":
		// ssh://git@host/… names an account, not a secret.
		if _, hasPassword := u.User.Password(); hasPassword {
			return credentialInURL()
		}
	default:
		return unfetchable(raw)
	}
	return nil
}

// windowsPath is C:\… or C:/…, which the shorthand would otherwise read as a
// one-letter host.
func windowsPath(s string) bool {
	return len(s) >= 3 && s[1] == ':' && (s[2] == '\\' || s[2] == '/') &&
		(s[0] >= 'a' && s[0] <= 'z' || s[0] >= 'A' && s[0] <= 'Z')
}

func unfetchable(raw string) error {
	return errs.Newf(errs.ValidInvalid,
		"%q is not a repository address Pando can fetch. Pando fetches over https, http or ssh, such as https://github.com/team/app.git or git@github.com:team/app.git.", raw).
		WithRemedy("Use the address your Git host gives for cloning. To deploy files that are not in a repository, send them as an upload.")
}

func credentialInURL() error {
	return errs.New(errs.ValidInvalid,
		"This repository address has a username, password or token in it. Pando records an app's address in its spec, which is kept for good and can be exported, so a credential written into it could never be removed.").
		WithRemedy("Remove the credential from the address, and add it as a source connection under Adapters in the console instead. Pando keeps those encrypted and uses the one that covers the address.")
}
