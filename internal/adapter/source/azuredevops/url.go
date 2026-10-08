package azuredevops

import (
	"net/url"
	"strings"

	"github.com/trypando/pando/internal/adapter/source/forgekit"
)

const (
	servicesHost    = "dev.azure.com"
	servicesSSHHost = "ssh.dev.azure.com"
	legacySSHHost   = "vs-ssh.visualstudio.com"
	legacySuffix    = ".visualstudio.com"
)

// repoRef is an Azure DevOps repository address, read into its parts. On Azure
// DevOps Server the organization is the collection.
type repoRef struct {
	// services says the address is on Azure DevOps Services, whichever of its
	// host names it was written with; host is then dev.azure.com.
	services bool
	host     string

	ssh  bool
	user string

	// prefix is the path an Azure DevOps Server is served under, such as
	// "tfs", or empty.
	prefix string

	org, project, repo string
}

func (r repoRef) segments() []string { return []string{r.org, r.project, r.repo} }

// isServicesHost reports whether a host is one of Azure DevOps Services' own:
// dev.azure.com, its SSH host, or a legacy {organization}.visualstudio.com.
func isServicesHost(host string) bool {
	return host == servicesHost || host == servicesSSHHost || strings.HasSuffix(host, legacySuffix)
}

// legacyOrg is the organization a legacy {organization}.visualstudio.com host
// names.
func legacyOrg(host string) (string, bool) {
	if !strings.HasSuffix(host, legacySuffix) || host == legacySSHHost {
		return "", false
	}
	org := strings.TrimSuffix(host, legacySuffix)
	if org == "" || strings.Contains(org, ".") {
		return "", false
	}
	return org, true
}

// parse reads every form Azure DevOps writes a repository address in:
//
//	https://dev.azure.com/{org}/{project}/_git/{repo}
//	https://{user}@dev.azure.com/{org}/{project}/_git/{repo}
//	https://{org}.visualstudio.com[/DefaultCollection]/{project}/_git/{repo}
//	git@ssh.dev.azure.com:v3/{org}/{project}/{repo}
//	{org}@vs-ssh.visualstudio.com:v3/{org}/{project}/{repo}
//	https://HOST[/{prefix}]/{collection}/{project}/_git/{repo}
//	ssh://HOST:22[/{prefix}]/{collection}/{project}/_git/{repo}
//
// A repository named like its project may leave the project out:
// .../{org}/_git/{repo}.
func parse(raw string) (repoRef, bool) {
	u, err := forgekit.ParseRepoURL(raw)
	if err != nil {
		return repoRef{}, false
	}
	segs := u.Segments()
	ref := repoRef{ssh: u.SSH, user: u.User, host: u.Host}

	if u.Host == servicesSSHHost || u.Host == legacySSHHost {
		if len(segs) != 4 || !strings.EqualFold(segs[0], "v3") {
			return repoRef{}, false
		}
		ref.services, ref.host = true, servicesHost
		ref.org, ref.project, ref.repo = segs[1], segs[2], segs[3]
		return ref, valid(ref)
	}

	before, repo, ok := splitGit(segs)
	if !ok {
		return repoRef{}, false
	}
	ref.repo = repo

	if u.Host == servicesHost {
		ref.services = true
		switch len(before) {
		case 1:
			ref.org, ref.project = before[0], repo
		case 2:
			ref.org, ref.project = before[0], before[1]
		default:
			return repoRef{}, false
		}
		return ref, valid(ref)
	}

	if org, ok := legacyOrg(u.Host); ok {
		ref.services, ref.host, ref.org = true, servicesHost, org
		if len(before) == 2 && strings.EqualFold(before[0], "DefaultCollection") {
			before = before[1:]
		}
		switch len(before) {
		case 0:
			ref.project = repo
		case 1:
			ref.project = before[0]
		default:
			return repoRef{}, false
		}
		return ref, valid(ref)
	}

	// Azure DevOps Server.
	switch n := len(before); {
	case n == 1:
		ref.org, ref.project = before[0], repo
	case n >= 2:
		ref.org, ref.project = before[n-2], before[n-1]
		ref.prefix = strings.Join(before[:n-2], "/")
	default:
		return repoRef{}, false
	}
	return ref, valid(ref)
}

func valid(r repoRef) bool { return r.org != "" && r.project != "" && r.repo != "" }

// splitGit finds the "_git" segment: what comes before it, and the repository
// after it. Older Team Foundation Server SSH addresses say "_ssh".
func splitGit(segs []string) ([]string, string, bool) {
	for i, s := range segs {
		if (strings.EqualFold(s, "_git") || strings.EqualFold(s, "_ssh")) && i+1 < len(segs) {
			return segs[:i], segs[i+1], true
		}
	}
	return nil, "", false
}

// stripUser drops the user from an HTTPS address: Azure DevOps puts the
// organization there ("https://acme@dev.azure.com/..."), and a person enters
// the address without it.
func stripUser(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}
