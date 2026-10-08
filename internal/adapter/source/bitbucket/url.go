package bitbucket

import (
	"net/url"
	"strings"

	"github.com/trypando/pando/internal/adapter/source/forgekit"
)

// repoRef is a Bitbucket repository address, read. On Bitbucket Cloud the
// owner is the workspace; on Data Center it is the project key.
type repoRef struct {
	host string
	ssh  bool
	user string

	owner, slug string
}

// parse reads the forms Bitbucket writes a repository address in:
//
//	https://[{user}@]bitbucket.org/{workspace}/{repo}[.git]
//	git@bitbucket.org:{workspace}/{repo}.git
//	https://HOST[/{context}]/scm/{project}/{repo}.git
//	https://HOST[/{context}]/projects/{project}/repos/{repo}[/browse]
//	ssh://git@HOST:7999/{project}/{repo}.git
func parse(raw string) (repoRef, bool) {
	u, err := forgekit.ParseRepoURL(raw)
	if err != nil {
		return repoRef{}, false
	}
	segs := u.Segments()
	ref := repoRef{host: u.Host, ssh: u.SSH, user: u.User}

	if u.Host == cloudHost {
		if len(segs) < 2 {
			return repoRef{}, false
		}
		ref.owner, ref.slug = segs[0], segs[1]
		return ref, ref.owner != "" && ref.slug != ""
	}

	if u.SSH {
		if len(segs) != 2 {
			return repoRef{}, false
		}
		ref.owner, ref.slug = segs[0], segs[1]
		return ref, ref.owner != "" && ref.slug != ""
	}
	for i, s := range segs {
		switch {
		case s == "scm" && i+2 < len(segs):
			ref.owner, ref.slug = segs[i+1], segs[i+2]
			return ref, ref.owner != "" && ref.slug != ""
		case s == "projects" && i+3 < len(segs) && segs[i+2] == "repos":
			ref.owner, ref.slug = segs[i+1], segs[i+3]
			return ref, ref.owner != "" && ref.slug != ""
		}
	}
	return repoRef{}, false
}

// stripUser drops the user Bitbucket puts in a clone link
// ("https://alice@bitbucket.org/..."), which a person enters without.
func stripUser(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}

// sameOrigin reports whether a next-page address is on the API the connection
// is for, so its credential is never sent anywhere a response named.
func sameOrigin(next, base string) bool {
	n, err1 := url.Parse(next)
	b, err2 := url.Parse(base)
	return err1 == nil && err2 == nil && strings.EqualFold(n.Scheme, b.Scheme) && strings.EqualFold(n.Host, b.Host)
}
