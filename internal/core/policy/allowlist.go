package policy

import (
	"context"
	"net/url"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// AllowUploads is the allowlist entry that admits uploaded source.
//
// An upload has no host and no path, so no host entry can name it. Without an
// entry of its own, a non-empty allowlist either lets every upload through —
// which makes "only github.com/acme" mean nothing to anyone with a CLI token —
// or refuses them all with no way to say otherwise.
const AllowUploads = "upload"

// AllowsSource checks the source allowlist (R-092).
//
// Called before any clone or pull, so a blocked source produces zero disk
// writes. The check being here rather than inside the clone path is the whole
// point: by the time you are cloning, you have already written to disk.
//
// An entry is a host (github.com), a host suffix (.corp.example), or a host
// and a path prefix (github.com/acme, ghcr.io/acme/web,
// 123456789012.dkr.ecr.us-east-1.amazonaws.com/team). A path prefix matches on
// whole segments, so github.com/acme does not admit github.com/acme-evil. Git
// URLs and image references are read the same way, which is how one list
// restricts "named orgs, named repos, a specific forge, or registry
// namespaces" alike. An upload is admitted only by the entry `upload`.
func (e *Evaluator) AllowsSource(ctx context.Context, src spec.Source) error {
	doc, err := e.load(ctx)
	if err != nil {
		return err
	}
	if len(doc.SourceAllowlist) == 0 {
		return nil
	}

	loc, ok := locate(src)
	if !ok {
		// Nothing named yet — an app being written by hand, with no source.
		return nil
	}

	for _, entry := range doc.SourceAllowlist {
		if loc.upload {
			if strings.EqualFold(strings.TrimSpace(entry), AllowUploads) {
				return nil
			}
			continue
		}
		if matches(loc, entry) {
			return nil
		}
	}
	return refusal(src, loc, doc.SourceAllowlist)
}

// location is a source reduced to what the allowlist compares.
type location struct {
	host   string
	path   string // lowercased, no leading or trailing slash
	upload bool
	parsed bool // false when the source named something that is not an address
}

// locate reads a source as a host and a path. It reports false only when the
// source names nothing at all, which the allowlist has no opinion on.
func locate(src spec.Source) (location, bool) {
	switch src.Type {
	case spec.SourceUpload:
		return location{upload: true, parsed: true}, true

	case spec.SourceImage:
		raw := strings.TrimSpace(src.Image)
		if raw == "" {
			return location{}, false
		}
		ref, err := name.ParseReference(raw)
		if err != nil {
			return location{}, true
		}
		return location{
			host:   canonicalRegistry(ref.Context().RegistryStr()),
			path:   strings.ToLower(ref.Context().RepositoryStr()),
			parsed: true,
		}, true

	default:
		raw := strings.TrimSpace(src.URL)
		if raw == "" {
			return location{}, false
		}
		host, path := splitGitURL(raw)
		if host == "" {
			return location{}, true
		}
		return location{host: host, path: path, parsed: true}, true
	}
}

// splitGitURL reads https, ssh and scp-style remotes.
func splitGitURL(raw string) (host, path string) {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return strings.ToLower(u.Hostname()), cleanPath(u.Path)
	}
	// scp-style: git@github.com:acme/notes.git
	if _, rest, found := strings.Cut(raw, "@"); found {
		if host, path, ok := strings.Cut(rest, ":"); ok {
			return strings.ToLower(host), cleanPath(path)
		}
	}
	return "", ""
}

func cleanPath(p string) string {
	p = strings.Trim(strings.ToLower(p), "/")
	return strings.TrimSuffix(p, ".git")
}

// canonicalRegistry names Docker Hub one way. The same registry answers to
// three hosts, and an administrator who wrote docker.io should not find
// index.docker.io refused.
func canonicalRegistry(host string) string {
	host = strings.ToLower(host)
	switch host {
	case "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com":
		return "docker.io"
	}
	return host
}

// matches compares a location against one allowlist entry.
func matches(loc location, entry string) bool {
	if !loc.parsed || loc.host == "" {
		return false
	}
	entry = strings.ToLower(strings.TrimSpace(entry))
	if before, after, ok := strings.Cut(entry, "://"); ok && before != "" {
		entry = after
	}
	hostPattern, prefix, _ := strings.Cut(entry, "/")
	prefix = cleanPath(prefix)

	if !matchesHost(loc.host, canonicalRegistry(hostPattern)) {
		return false
	}
	if prefix == "" {
		return true
	}
	return loc.path == prefix || strings.HasPrefix(loc.path, prefix+"/")
}

// matchesHost compares a host against a bare host or a leading-dot suffix such
// as .corp.example.
func matchesHost(host, pattern string) bool {
	if host == "" || pattern == "" {
		return false
	}
	if strings.HasPrefix(pattern, ".") {
		return strings.HasSuffix(host, pattern) || host == strings.TrimPrefix(pattern, ".")
	}
	return host == pattern
}

func refusal(src spec.Source, loc location, allowed []string) error {
	var err *errs.Error
	switch {
	case loc.upload:
		err = errs.New(errs.PolicySourceNotAllowed,
			"Apps on this installation can only be created from approved sources, and uploaded files are not one of them.").
			WithRemedy("Deploy from an approved repository or registry instead, or ask an administrator to add `upload` to the installation's source allowlist.")
	case src.Type == spec.SourceImage:
		named := src.Image
		if loc.host != "" {
			named = loc.host + "/" + loc.path
		}
		err = errs.Newf(errs.PolicySourceNotAllowed,
			"Apps on this installation can only be created from approved sources, and the image %s is not one of them.", named).
			WithRemedy("Use an image from an approved registry, or ask an administrator to add this registry or namespace to the installation's source allowlist.")
	default:
		where := "that address"
		if loc.host != "" {
			where = strings.TrimSuffix(loc.host+"/"+loc.path, "/")
		}
		err = errs.Newf(errs.PolicySourceNotAllowed,
			"Apps on this installation can only be created from approved sources, and %s is not one of them.", where).
			WithRemedy("Use a repository from an approved source, or ask an administrator to add this one.")
	}
	return err.
		WithDetail("source", describeSource(src)).
		WithDetail("allowed", allowed)
}

func describeSource(src spec.Source) string {
	switch src.Type {
	case spec.SourceImage:
		return src.Image
	case spec.SourceUpload:
		return AllowUploads
	}
	return src.URL
}
